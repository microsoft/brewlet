// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.sun.net.httpserver.HttpServer;
import org.apache.maven.plugin.MojoExecutionException;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.ValueSource;
import org.junit.jupiter.params.provider.Arguments;
import org.junit.jupiter.params.provider.MethodSource;
import sh.brewlet.maven.plugin.model.JvmConfig;
import sh.brewlet.maven.plugin.oci.LocalStore;
import sh.brewlet.maven.plugin.oci.MediaTypes;
import sh.brewlet.maven.plugin.util.PreparedApplication;

import java.io.ByteArrayInputStream;
import java.nio.ByteBuffer;
import java.nio.ByteOrder;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.jar.JarFile;
import java.util.zip.GZIPInputStream;

import static org.junit.jupiter.api.Assertions.*;

class LayeredBootPackagingTest {
    @TempDir Path root;
    private static final ObjectMapper JSON = new ObjectMapper();

    @ParameterizedTest
    @ValueSource(strings = {"artifact", "image"})
    void builtPayloadActuallyRunsAndCdsTrainsOnIdenticalBytes(String format) throws Exception {
        var boot = TestApplications.boot(root);
        byte[] source = Files.readAllBytes(boot.source());
        var sourceTime = Files.getLastModifiedTime(boot.source());
        BuildMojo build = new BuildMojo();
        TestApplications.configure(build, boot.source(), root.resolve("build"), true);
        build.format = format;
        Path layout = root.resolve("oci");
        TestApplications.set(build, "ociOutputDirectory", layout.toFile());
        build.execute();

        Path deployed = root.resolve("deployed");
        JvmConfig config = unpack(layout, deployed);
        assertEquals(List.of("boot.jar", "lib/z-SNAPSHOT.jar", "lib/a.jar"), config.getEntry().getClassPath());
        assertEquals("app.Main", config.getEntry().getMainClass());
        assertArrayEquals(boot.first(), Files.readAllBytes(deployed.resolve("lib/z-SNAPSHOT.jar")));
        assertArrayEquals(boot.second(), Files.readAllBytes(deployed.resolve("lib/a.jar")));
        try (JarFile jar = new JarFile(deployed.resolve("boot.jar").toFile())) {
            assertNotNull(jar.getEntry("app/Main.class"));
            assertNotNull(jar.getEntry("message.txt"));
            assertNull(jar.getEntry("BOOT-INF/classes/app/Main.class"));
            assertFalse(jar.stream().anyMatch(e -> e.getName().endsWith(".jar")));
        }
        assertTrue(TestApplications.java(deployed, AppCdsMojo.launchSelector(config, config.getMainJar()))
                .contains("first:resource"));

        ConfigMojo configMojo = new ConfigMojo();
        TestApplications.configure(configMojo, boot.source(), root.resolve("config"), true);
        configMojo.execute();
        assertEquals(JSON.valueToTree(config),
                JSON.readTree(root.resolve("config/jvm-config.json").toFile()));
        assertArrayEquals(Files.readAllBytes(deployed.resolve("boot.jar")),
                Files.readAllBytes(root.resolve("config/prepared/boot.jar")));
        InspectMojo inspect = new InspectMojo();
        TestApplications.configure(inspect, boot.source(), root.resolve("inspect"), true);
        StringBuilder inspected = new StringBuilder();
        inspect.setLog(new org.apache.maven.plugin.logging.SystemStreamLog() {
            @Override public void info(CharSequence content) { inspected.append(content).append('\n'); }
        });
        inspect.execute();
        assertTrue(inspected.toString().contains(root.resolve("inspect/prepared/boot.jar").toString()));
        assertTrue(inspected.toString().contains("\"boot.jar\", \"lib/z-SNAPSHOT.jar\", \"lib/a.jar\""));

        AppCdsMojo cds = new AppCdsMojo();
        TestApplications.configure(cds, boot.source(), root.resolve("cds"), true);
        Path training = root.resolve("training");
        Files.createDirectories(training.resolve("lib"));
        Files.writeString(training.resolve("lib/stale.jar"), "must not train stale library");
        cds.stageTrainingInputs(config, "classpath", cds.prepareApplication(), training);
        assertFalse(Files.exists(training.resolve("lib/stale.jar")));
        for (String file : config.getEntry().getClassPath()) {
            assertEquals(LocalStore.sha256Hex(Files.readAllBytes(deployed.resolve(file))),
                    LocalStore.sha256Hex(Files.readAllBytes(training.resolve(file))));
            Files.setLastModifiedTime(deployed.resolve(file), PreparedApplication.CANONICAL_MTIME);
        }
        // Dynamic CDS exists on JDK 17; the public turnkey goal's JDK 21 floor is unchanged.
        List<String> trainArgs = new ArrayList<>();
        trainArgs.add("-XX:ArchiveClassesAtExit=trained.jsa");
        trainArgs.addAll(AppCdsMojo.launchSelector(config, config.getMainJar()));
        assertTrue(TestApplications.java(training, trainArgs).contains("first:resource"));
        assertTrue(Files.size(training.resolve("trained.jsa")) > 0);
        Files.copy(training.resolve("trained.jsa"), deployed.resolve("trained.jsa"));
        List<String> runArgs = new ArrayList<>(List.of(
                "-Xshare:on", "-XX:SharedArchiveFile=trained.jsa", "-Xlog:class+load=info"));
        runArgs.addAll(AppCdsMojo.launchSelector(config, config.getMainJar()));
        String mapped = TestApplications.java(deployed, runArgs);
        assertTrue(mapped.contains("app.Main source: shared objects file"), mapped);
        assertTrue(mapped.contains("lib.Dependency source: shared objects file"), mapped);

        build.execute();
        Path repeated = root.resolve("repeated");
        assertEquals(JSON.valueToTree(config), JSON.valueToTree(unpack(layout, repeated)));
        assertArrayEquals(Files.readAllBytes(deployed.resolve("boot.jar")),
                Files.readAllBytes(repeated.resolve("boot.jar")));
        assertArrayEquals(source, Files.readAllBytes(boot.source()));
        assertEquals(sourceTime, Files.getLastModifiedTime(boot.source()));

        build.cdsArchive = training.resolve("trained.jsa").toFile();
        build.execute();
        Path paired = root.resolve("paired");
        JvmConfig pairedConfig = unpack(layout, paired);
        assertEquals("trained.jsa", pairedConfig.getCds().getArchive());
        assertEquals(LocalStore.sha256Hex(Files.readAllBytes(training.resolve("trained.jsa"))),
                LocalStore.sha256Hex(Files.readAllBytes(paired.resolve("trained.jsa"))));
        for (String file : config.getEntry().getClassPath()) {
            assertArrayEquals(Files.readAllBytes(training.resolve(file)), Files.readAllBytes(paired.resolve(file)));
            Files.setLastModifiedTime(paired.resolve(file), PreparedApplication.CANONICAL_MTIME);
        }
        assertTrue(TestApplications.java(paired, runArgs).contains("app.Main source: shared objects file"));
    }

    @ParameterizedTest
    @ValueSource(strings = {"artifact", "image"})
    void pushUsesTheSameRunnablePayloadAsBuild(String format) throws Exception {
        var boot = TestApplications.boot(root);
        Path layout = root.resolve("uploaded");
        Files.createDirectories(layout.resolve("blobs/sha256"));
        HttpServer server = HttpServer.create(new java.net.InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/", exchange -> {
            try (exchange) {
                String path = exchange.getRequestURI().getPath();
                String method = exchange.getRequestMethod();
                if ("HEAD".equals(method)) {
                    exchange.sendResponseHeaders(404, -1);
                } else if ("POST".equals(method)) {
                    exchange.getResponseHeaders().set("Location", "/v2/app/blobs/uploads/test");
                    exchange.sendResponseHeaders(202, -1);
                } else if ("PUT".equals(method)) {
                    byte[] bytes = exchange.getRequestBody().readAllBytes();
                    String digest = LocalStore.sha256Hex(bytes);
                    Files.write(layout.resolve("blobs/sha256").resolve(digest.substring(7)), bytes);
                    if (path.endsWith("/manifests/1")) {
                        JSON.writeValue(layout.resolve("index.json").toFile(),
                                Map.of("manifests", List.of(Map.of("digest", digest, "size", bytes.length))));
                    }
                    exchange.sendResponseHeaders(201, -1);
                } else {
                    exchange.sendResponseHeaders(404, -1);
                }
            }
        });
        server.start();
        try {
            PushMojo push = new PushMojo();
            TestApplications.configure(push, boot.source(), root.resolve("push"), true);
            push.format = format;
            String registry = "127.0.0.1:" + server.getAddress().getPort();
            push.image = registry + "/app:1";
            push.insecureRegistries = List.of(registry);
            push.execute();
            Path deployed = root.resolve("pushed");
            JvmConfig config = unpack(layout, deployed);
            assertEquals(List.of("boot.jar", "lib/z-SNAPSHOT.jar", "lib/a.jar"), config.getEntry().getClassPath());
            assertTrue(TestApplications.java(deployed, AppCdsMojo.launchSelector(config, "boot.jar"))
                    .contains("first:resource"));
            assertArrayEquals(boot.first(), Files.readAllBytes(deployed.resolve("lib/z-SNAPSHOT.jar")));
            assertArrayEquals(Files.readAllBytes(push.prepareApplication().jar().toPath()),
                    Files.readAllBytes(deployed.resolve("boot.jar")));
        } finally {
            server.stop(0);
        }
    }

    @Test
    void managedModeRejectsRepackagedJarsBeforeTryingToResolveBundle() throws Exception {
        var boot = TestApplications.boot(root);
        PushMojo push = new PushMojo();
        TestApplications.configure(push, boot.source(), root.resolve("out"), true);
        push.format = "image";
        push.dependencyBundle = "must-not-be-contacted.test/bundle:1";
        assertTrue(assertThrows(MojoExecutionException.class, push::execute).getMessage().contains("thin application JAR"));
        boot.entries().keySet().removeIf(name -> name.startsWith("BOOT-INF/lib/"));
        Files.write(boot.source(), TestApplications.zip(boot.entries()));
        assertTrue(assertThrows(MojoExecutionException.class, push::execute).getMessage().contains("nested application classes"));
    }

    @Test
    void bootMetadataScansTheExactPackagedLibrariesNotTheMavenGraph() throws Exception {
        var boot = TestApplications.boot(root);
        boot.entries().put("BOOT-INF/lib/z-SNAPSHOT.jar", TestApplications.zip(Map.of(
                "META-INF/native/linux-x86_64/libexample.so", new byte[]{1})));
        Files.write(boot.source(), TestApplications.zip(boot.entries()));
        BuildMojo mojo = new BuildMojo();
        TestApplications.configure(mojo, boot.source(), root.resolve("out"), true);
        mojo.detectNativeArch = true;
        assertEquals(List.of("amd64"), mojo.buildConfig().getArch());
        mojo.arch = List.of("arm64");
        assertEquals(List.of("arm64"), mojo.buildConfig().getArch(), "explicit architecture override wins");
    }

    @Test
    void stagingNeverDeletesOrOverwritesItsOriginalInput() throws Exception {
        var boot = TestApplications.boot(root);
        byte[] original = Files.readAllBytes(boot.source());
        assertThrows(java.io.IOException.class,
                () -> PreparedApplication.prepare(boot.source().toFile(), true, root, List.of()));
        assertArrayEquals(original, Files.readAllBytes(boot.source()));
        AppCdsMojo mojo = new AppCdsMojo();
        TestApplications.configure(mojo, boot.source(), root.resolve("output"), true);
        JvmConfig config = mojo.buildConfig();
        assertThrows(MojoExecutionException.class,
                () -> mojo.stageTrainingInputs(config, "classpath", mojo.prepareApplication(), root));
        assertArrayEquals(original, Files.readAllBytes(boot.source()));
    }

    @Test
    void nestedMavenMetadataWithUnknownDirectoryPermissionsIsPreserved() throws Exception {
        var boot = TestApplications.boot(root);
        Map<String, byte[]> libraryEntries = new LinkedHashMap<>();
        libraryEntries.put("META-INF/maven/", new byte[0]);
        libraryEntries.put("lib/Dependency.class",
                Files.readAllBytes(root.resolve("first/classes/lib/Dependency.class")));
        byte[] library = TestApplications.zip(libraryEntries);
        ByteBuffer zip = ByteBuffer.wrap(library).order(ByteOrder.LITTLE_ENDIAN);
        int central = zip.getInt(library.length - 22 + 16);
        zip.putShort(central + 4, (short) (3 << 8 | 20));
        zip.putInt(central + 38, 0xffff0010);
        boot.entries().put("BOOT-INF/lib/z-SNAPSHOT.jar", library);
        byte[] source = TestApplications.zip(boot.entries());
        Files.write(boot.source(), source);
        var sourceTime = Files.getLastModifiedTime(boot.source());
        Path output = root.resolve("prepared");
        PreparedApplication prepared = PreparedApplication.prepare(boot.source().toFile(), true, output, List.of());
        assertArrayEquals(library, Files.readAllBytes(prepared.dependencies().get(0).path()));
        assertTrue(TestApplications.java(output, List.of("-cp",
                String.join(java.io.File.pathSeparator, prepared.classPath()), "app.Main")).contains("first:resource"));
        byte[] first = Files.readAllBytes(prepared.jar().toPath());
        assertArrayEquals(first, Files.readAllBytes(
                PreparedApplication.prepare(boot.source().toFile(), true, output, List.of()).jar().toPath()));
        assertArrayEquals(source, Files.readAllBytes(boot.source()));
        assertEquals(sourceTime, Files.getLastModifiedTime(boot.source()));
    }

    @ParameterizedTest
    @MethodSource("directoryLayouts")
    void preparedDirectoriesSupportPackageAndResourceScanning(String format, boolean explicitDirectories)
            throws Exception {
        var boot = TestApplications.boot(root);
        Path endpoint = TestApplications.compile(root.resolve("endpoint"), "app.scanned.Endpoint", """
                package app.scanned;
                public class Endpoint {
                    public static String message() { return "discovered-controller"; }
                }
                """);
        Path main = TestApplications.compile(root.resolve("scanner"), "app.Main", """
                package app;
                public class Main {
                    public static void main(String[] args) throws Exception {
                        var loader = Thread.currentThread().getContextClassLoader();
                        int controllers = 0;
                        var packages = loader.getResources("app/scanned/");
                        while (packages.hasMoreElements()) {
                            var connection = (java.net.JarURLConnection) packages.nextElement().openConnection();
                            var classes = connection.getJarFile().stream()
                                .map(java.util.jar.JarEntry::getName)
                                .filter(n -> n.startsWith("app/scanned/") && n.endsWith(".class")).toList();
                            for (String name : classes) {
                                var type = Class.forName(name.substring(0, name.length() - 6).replace('/', '.'));
                                if ("discovered-controller".equals(type.getMethod("message").invoke(null))) controllers++;
                            }
                        }
                        if (controllers != 1 || !loader.getResources("app/scanned").hasMoreElements())
                            throw new AssertionError("Package-directory scanning failed");
                        for (String name : java.util.List.of("templates/", "static/", "static/css/",
                                "empty/", "empty/assets/", "META-INF/metadata-empty/")) {
                            if (!loader.getResources(name).hasMoreElements())
                                throw new AssertionError("Missing resource directory: " + name);
                        }
                        var templates = loader.getResources("templates/").nextElement();
                        try (var in = new java.net.URL(templates, "welcome.html").openStream()) {
                            String html = new String(in.readAllBytes(), java.nio.charset.StandardCharsets.UTF_8);
                            if (!html.contains("Welcome")) throw new AssertionError("Template lookup failed");
                        }
                        System.out.println(lib.Dependency.message() + ":package scan and templates");
                    }
                }
                """, "-classpath", root.resolve("compile-dependency.jar").toString());
        boot.entries().put("BOOT-INF/classes/app/Main.class", Files.readAllBytes(main.resolve("app/Main.class")));
        boot.entries().put("BOOT-INF/classes/app/scanned/Endpoint.class",
                Files.readAllBytes(endpoint.resolve("app/scanned/Endpoint.class")));
        boot.entries().put("BOOT-INF/classes/templates/welcome.html", "<html>Welcome</html>".getBytes(StandardCharsets.UTF_8));
        boot.entries().put("BOOT-INF/classes/static/css/site.css", "body {}".getBytes(StandardCharsets.UTF_8));
        boot.entries().put("BOOT-INF/classes/empty/assets/", new byte[0]);
        boot.entries().put("META-INF/metadata-empty/", new byte[0]);
        if (explicitDirectories) {
            for (String name : List.of("META-INF/", "BOOT-INF/classes/", "BOOT-INF/classes/META-INF/",
                    "BOOT-INF/classes/app/", "BOOT-INF/classes/app/scanned/",
                    "BOOT-INF/classes/templates/", "BOOT-INF/classes/static/")) {
                boot.entries().put(name, new byte[0]);
            }
        }
        byte[] source = TestApplications.zip(boot.entries());
        Files.write(boot.source(), source);
        var sourceTime = Files.getLastModifiedTime(boot.source());
        BuildMojo mojo = new BuildMojo();
        TestApplications.configure(mojo, boot.source(), root.resolve("output"), true);
        mojo.format = format;
        Path layout = root.resolve("oci");
        TestApplications.set(mojo, "ociOutputDirectory", layout.toFile());
        mojo.execute();
        Path deployed = root.resolve("deployed");
        JvmConfig config = unpack(layout, deployed);
        assertTrue(TestApplications.java(deployed, AppCdsMojo.launchSelector(config, "boot.jar"))
                .contains("first:package scan and templates"));
        byte[] first = Files.readAllBytes(deployed.resolve("boot.jar"));
        mojo.execute();
        assertArrayEquals(first, Files.readAllBytes(mojo.prepareApplication().jar().toPath()));
        assertArrayEquals(source, Files.readAllBytes(boot.source()));
        assertEquals(sourceTime, Files.getLastModifiedTime(boot.source()));
    }

    static java.util.stream.Stream<Arguments> directoryLayouts() {
        return java.util.stream.Stream.of(Arguments.of("artifact", true), Arguments.of("artifact", false),
                Arguments.of("image", true), Arguments.of("image", false));
    }

    @Test
    void preservedAndSynthesizedDirectoriesStillRejectRelocationCollisions() throws Exception {
        var boot = TestApplications.boot(root);
        Map<String, byte[]> original = new LinkedHashMap<>(boot.entries());
        for (String name : List.of("BOOT-INF/classes/META-INF",
                "BOOT-INF/classes/META-INF/MANIFEST.MF/",
                "BOOT-INF/classes/META-INF/MANIFEST.MF/child")) {
            Map<String, byte[]> entries = new LinkedHashMap<>(original);
            entries.put(name, name.endsWith("/") ? new byte[0] : new byte[]{1});
            Files.write(boot.source(), TestApplications.zip(entries));
            assertThrows(java.io.IOException.class,
                    () -> PreparedApplication.prepare(boot.source().toFile(), true, root.resolve("out"), List.of()), name);
        }
        for (boolean directoryFirst : List.of(true, false)) {
            Map<String, byte[]> entries = new LinkedHashMap<>(original);
            if (directoryFirst) entries.put("BOOT-INF/classes/META-INF/resource/", new byte[0]);
            entries.put("META-INF/resource", new byte[]{1});
            if (!directoryFirst) entries.put("BOOT-INF/classes/META-INF/resource/", new byte[0]);
            Files.write(boot.source(), TestApplications.zip(entries));
            assertThrows(java.io.IOException.class,
                    () -> PreparedApplication.prepare(boot.source().toFile(), true, root.resolve("out"), List.of()));
        }
    }

    @Test
    void publicAppCdsGoalTrainsThePreparedBootPayload() throws Exception {
        org.junit.jupiter.api.Assumptions.assumeTrue(Runtime.version().feature() >= 21,
                "The public training goal intentionally requires JDK 21+");
        var boot = TestApplications.boot(root);
        AppCdsMojo mojo = new AppCdsMojo();
        Path output = root.resolve("output");
        Path archive = root.resolve("app.jsa");
        TestApplications.configure(mojo, boot.source(), output, true);
        TestApplications.set(mojo, "cdsArchiveOutput", archive.toFile());
        TestApplications.set(mojo, "timeoutSeconds", 20);
        TestApplications.set(mojo, "trainingJavaHome", new java.io.File(System.getProperty("java.home")));
        mojo.execute();
        assertTrue(Files.size(archive) > 0);
        var payload = mojo.prepareApplication();
        for (String path : payload.classPath()) {
            assertArrayEquals(Files.readAllBytes(output.resolve("prepared").resolve(path)),
                    Files.readAllBytes(output.resolve("appcds-training").resolve(path)));
        }
    }

    @Test
    void signedDependenciesRemainSignedAndRewrittenApplicationDropsOnlyStaleSignatures() throws Exception {
        var boot = TestApplications.boot(root);
        Path lib = root.resolve("signed.jar");
        Files.write(lib, boot.first());
        Path keys = root.resolve("test-keystore.p12");
        runJdkTool("keytool", "-genkeypair", "-keyalg", "RSA", "-keystore", keys.toString(),
                "-storepass", "test-password", "-keypass", "test-password",
                "-alias", "fixture", "-dname", "CN=Test fixture", "-validity", "1");
        runJdkTool("jarsigner", "-keystore", keys.toString(), "-storepass", "test-password", lib.toString(), "fixture");
        byte[] signedLibrary = Files.readAllBytes(lib);
        boot.entries().put("BOOT-INF/lib/z-SNAPSHOT.jar", signedLibrary);
        boot.entries().put("META-INF/services/java.nio.file.spi.FileSystemProvider",
                "org.springframework.boot.loader.nio.file.NestedFileSystemProvider\n".getBytes(StandardCharsets.UTF_8));
        Files.write(boot.source(), TestApplications.zip(boot.entries()));
        runJdkTool("jarsigner", "-keystore", keys.toString(), "-storepass", "test-password", boot.source().toString(), "fixture");
        byte[] signedSource = Files.readAllBytes(boot.source());
        PreparedApplication payload = PreparedApplication.prepare(boot.source().toFile(), true, root.resolve("out"), List.of());
        assertArrayEquals(signedLibrary, Files.readAllBytes(payload.dependencies().get(0).path()));
        try (JarFile jar = new JarFile(payload.dependencies().get(0).path().toFile(), true)) {
            var entry = jar.getJarEntry("lib/Dependency.class");
            try (var in = jar.getInputStream(entry)) { in.readAllBytes(); }
            assertNotNull(entry.getCertificates(), "signed nested library lost its signature");
        }
        try (JarFile jar = new JarFile(payload.jar(), true)) {
            assertNull(jar.getEntry("META-INF/FIXTURE.SF"));
            assertNull(jar.getEntry("META-INF/services/java.nio.file.spi.FileSystemProvider"));
            assertTrue(jar.getManifest().getEntries().isEmpty(), "stale per-entry signature digests survived relocation");
            assertEquals("app.Main", jar.getManifest().getMainAttributes().getValue("Main-Class"));
        }
        assertArrayEquals(signedSource, Files.readAllBytes(boot.source()));
        assertTrue(TestApplications.java(root.resolve("out"), List.of("-cp",
                String.join(java.io.File.pathSeparator, payload.classPath()), "app.Main")).contains("first:resource"));
    }

    private void runJdkTool(String tool, String... args) throws Exception {
        List<String> command = new ArrayList<>();
        command.add(Path.of(System.getProperty("java.home"), "bin", tool).toString());
        command.addAll(List.of(args));
        Path log = root.resolve(tool + ".log");
        Process process = new ProcessBuilder(command).redirectErrorStream(true).redirectOutput(log.toFile()).start();
        try {
            assertTrue(process.waitFor(20, java.util.concurrent.TimeUnit.SECONDS));
            assertEquals(0, process.exitValue(), Files.readString(log));
        } finally {
            if (process.isAlive()) {
                process.destroyForcibly();
                assertTrue(process.waitFor(5, java.util.concurrent.TimeUnit.SECONDS));
            }
        }
    }

    @Test
    void noIndexPreservesPackagedLibraryOrderAndOverridesRemainExplicit() throws Exception {
        var boot = TestApplications.boot(root);
        boot.entries().remove("BOOT-INF/classpath.idx");
        Files.write(boot.source(), TestApplications.zip(boot.entries()));
        BuildMojo mojo = new BuildMojo();
        TestApplications.configure(mojo, boot.source(), root.resolve("out"), true);
        assertEquals(List.of("boot.jar", "lib/a.jar", "lib/z-SNAPSHOT.jar"),
                mojo.buildConfig().getEntry().getClassPath());
        mojo.mainClass = "org.springframework.boot.loader.launch.JarLauncher";
        assertThrows(MojoExecutionException.class, mojo::buildConfig);
        mojo.mainClass = "app.Main";
        assertEquals("app.Main", mojo.buildConfig().getEntry().getMainClass());
        mojo.entryMode = "jar";
        assertThrows(MojoExecutionException.class, mojo::buildConfig);
        mojo.layered = false;
        mojo.mainClass = null;
        assertEquals("jar", mojo.buildConfig().getEntry().getMode());
        assertEquals(boot.source().toFile(), mojo.prepareApplication().jar());
        mojo.entryMode = "classpath";
        assertThrows(MojoExecutionException.class, mojo::buildConfig);
    }

    @Test
    void rejectsUnsupportedLayoutsAndAmbiguousIndexesBeforePublishing() throws Exception {
        var boot = TestApplications.boot(root);
        Map<String, byte[]> original = new LinkedHashMap<>(boot.entries());
        for (String bad : List.of("../escape", "/absolute", "BOOT-INF/classes/../escape",
                "BOOT-INF/classes/a\\b", "BOOT-INF/classes/a:b", "BOOT-INF/classes/app",
                "BOOT-INF/lib/sub/lib.jar", "WEB-INF/lib/x.jar", "custom/Main.class",
                "BOOT-INF/classes/module-info.class", "BOOT-INF/classes/nested.jar",
                "BOOT-INF/classes/META-INF/MANIFEST.MF")) {
            Map<String, byte[]> entries = new LinkedHashMap<>(original);
            entries.put(bad, new byte[]{1});
            Files.write(boot.source(), TestApplications.zip(entries));
            assertThrows(java.io.IOException.class,
                    () -> PreparedApplication.prepare(boot.source().toFile(), true, root.resolve("out"), List.of()), bad);
        }
        for (String index : List.of(
                "- \"BOOT-INF/lib/a.jar\"\n", "- \"BOOT-INF/lib/missing.jar\"\n",
                "- \"BOOT-INF/lib/a.jar\"\n- \"BOOT-INF/lib/a.jar\"\n", "not YAML")) {
            Map<String, byte[]> entries = new LinkedHashMap<>(original);
            entries.put("BOOT-INF/classpath.idx", index.getBytes(StandardCharsets.UTF_8));
            Files.write(boot.source(), TestApplications.zip(entries));
            assertThrows(java.io.IOException.class,
                    () -> PreparedApplication.prepare(boot.source().toFile(), true, root.resolve("out"), List.of()));
        }
        for (String launcher : List.of("PropertiesLauncher", "WarLauncher", "CustomLauncher")) {
            Map<String, byte[]> entries = new LinkedHashMap<>(original);
            entries.put("META-INF/MANIFEST.MF", new String(original.get("META-INF/MANIFEST.MF"), StandardCharsets.UTF_8)
                    .replace("JarLauncher", launcher).getBytes(StandardCharsets.UTF_8));
            Files.write(boot.source(), TestApplications.zip(entries));
            assertThrows(java.io.IOException.class,
                    () -> PreparedApplication.prepare(boot.source().toFile(), true, root.resolve("out"), List.of()));
        }
    }

    @Test
    void rejectsDuplicateSymlinkAndMismatchedLocalZipEntries() throws Exception {
        var boot = TestApplications.boot(root);
        Map<String, byte[]> entries = new LinkedHashMap<>(boot.entries());
        entries.put("BOOT-INF/classes/x.txt", new byte[]{1});
        entries.put("BOOT-INF/classes/y.txt", new byte[]{2});
        byte[] valid = TestApplications.zip(entries);
        byte[] duplicate = valid.clone();
        replaceAscii(duplicate, "BOOT-INF/classes/y.txt", "BOOT-INF/classes/x.txt");
        assertInvalidZip(boot.source(), duplicate);

        byte[] symlink = valid.clone();
        ByteBuffer bytes = ByteBuffer.wrap(symlink).order(ByteOrder.LITTLE_ENDIAN);
        for (int i = 0; i < symlink.length - 46; i++) {
            if (bytes.getInt(i) == 0x02014b50) {
                bytes.putShort(i + 4, (short) (3 << 8 | 20));
                bytes.putInt(i + 38, 0120777 << 16);
                break;
            }
        }
        assertInvalidZip(boot.source(), symlink);
        byte[] localMismatch = valid.clone();
        localMismatch[30] = 'X';
        assertInvalidZip(boot.source(), localMismatch);
        byte[] prefixed = new byte[valid.length + 1];
        System.arraycopy(valid, 0, prefixed, 1, valid.length);
        assertInvalidZip(boot.source(), prefixed);
    }

    @Test
    void plainAndModularJarsRemainUnchangedAndRunnable() throws Exception {
        Path classes = TestApplications.compile(root.resolve("plain"), "Main",
                "public class Main { public static void main(String[] args) { System.out.println(\"plain\"); } }");
        Path plain = root.resolve("plain.jar");
        Files.write(plain, TestApplications.zip(Map.of(
                "META-INF/MANIFEST.MF", "Manifest-Version: 1.0\nMain-Class: Main\n\n".getBytes(StandardCharsets.UTF_8),
                "Main.class", Files.readAllBytes(classes.resolve("Main.class")))));
        BuildMojo mojo = new BuildMojo();
        TestApplications.configure(mojo, plain, root.resolve("plain-out"), true);
        assertEquals(plain.toFile(), mojo.prepareApplication().jar());
        assertTrue(TestApplications.java(root, AppCdsMojo.launchSelector(mojo.buildConfig(), "plain.jar")).contains("plain"));

        Path module = TestApplications.compile(root.resolve("module"), "module-info", "module demo.app {}");
        Path app = TestApplications.compile(root.resolve("module-main"), "demo.Main",
                "package demo; public class Main { public static void main(String[] args) { System.out.println(\"modular\"); } }");
        Path modular = root.resolve("modular.jar");
        Files.write(modular, TestApplications.zip(Map.of(
                "module-info.class", Files.readAllBytes(module.resolve("module-info.class")),
                "demo/Main.class", Files.readAllBytes(app.resolve("demo/Main.class")))));
        TestApplications.configure(mojo, modular, root.resolve("module-out"), true);
        mojo.mainClass = "demo.Main";
        assertEquals(modular.toFile(), mojo.prepareApplication().jar());
        assertEquals("module", mojo.buildConfig().getEntry().getMode());
        assertTrue(TestApplications.java(root, AppCdsMojo.launchSelector(mojo.buildConfig(), "modular.jar")).contains("modular"));
    }

    private void assertInvalidZip(Path source, byte[] bytes) throws Exception {
        Files.write(source, bytes);
        assertThrows(java.io.IOException.class,
                () -> PreparedApplication.prepare(source.toFile(), true, root.resolve("out"), List.of()));
    }

    private static void replaceAscii(byte[] bytes, String old, String replacement) {
        byte[] a = old.getBytes(StandardCharsets.UTF_8), b = replacement.getBytes(StandardCharsets.UTF_8);
        for (int i = 0; i <= bytes.length - a.length; i++) {
            if (Arrays.equals(Arrays.copyOfRange(bytes, i, i + a.length), a)) System.arraycopy(b, 0, bytes, i, b.length);
        }
    }

    static JvmConfig unpack(Path layout, Path target) throws Exception {
        JsonNode index = JSON.readTree(layout.resolve("index.json").toFile());
        JsonNode manifest = JSON.readTree(blob(layout, index.path("manifests").get(0)));
        boolean image = manifest.has("manifests");
        if (image) manifest = JSON.readTree(blob(layout, manifest.path("manifests").get(0)));
        JvmConfig config = image
                ? JSON.readValue(manifest.path("annotations").path(MediaTypes.JVM_CONFIG_ANNOTATION).asText(), JvmConfig.class)
                : JSON.readValue(blob(layout, manifest.path("config")), JvmConfig.class);
        Files.createDirectories(target);
        for (JsonNode layer : manifest.path("layers")) {
            byte[] content = blob(layout, layer);
            if (!image && MediaTypes.JAR_LAYER_MEDIA_TYPE.equals(layer.path("mediaType").asText())) {
                Files.write(target.resolve(config.getMainJar()), content);
                continue;
            }
            if (!image && MediaTypes.CDS_LAYER_MEDIA_TYPE.equals(layer.path("mediaType").asText())) {
                Files.write(target.resolve(config.getCds().getArchive()), content);
                continue;
            }
            boolean app = image && MediaTypes.LAYER_ROLE_APP.equals(
                    layer.path("annotations").path(MediaTypes.LAYER_ROLE_ANNOTATION).asText());
            if (image) try (var gz = new GZIPInputStream(new ByteArrayInputStream(content))) { content = gz.readAllBytes(); }
            Path dir = app ? target : target.resolve("lib");
            Files.createDirectories(dir);
            for (int pos = 0; pos + 512 <= content.length && content[pos] != 0;) {
                int end = pos;
                while (content[end] != 0) end++;
                String name = new String(content, pos, end - pos, StandardCharsets.UTF_8);
                String octal = new String(content, pos + 124, 12, StandardCharsets.UTF_8).replace("\0", "").trim();
                int length = Integer.parseInt(octal, 8);
                Files.write(dir.resolve(name), Arrays.copyOfRange(content, pos + 512, pos + 512 + length));
                pos += 512 + (length + 511) / 512 * 512;
            }
        }
        return config;
    }

    private static byte[] blob(Path layout, JsonNode descriptor) throws Exception {
        byte[] bytes = Files.readAllBytes(layout.resolve("blobs/sha256")
                .resolve(descriptor.path("digest").asText().substring(7)));
        assertEquals(descriptor.path("digest").asText(), LocalStore.sha256Hex(bytes));
        assertEquals(descriptor.path("size").asLong(), bytes.length);
        return bytes;
    }
}
