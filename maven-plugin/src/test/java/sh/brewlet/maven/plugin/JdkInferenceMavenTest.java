// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import sh.brewlet.maven.plugin.oci.LocalStore;
import sh.brewlet.maven.plugin.oci.MediaTypes;
import org.apache.maven.model.Model;
import org.apache.maven.model.io.xpp3.MavenXpp3Reader;
import org.apache.maven.artifact.DefaultArtifact;
import org.apache.maven.artifact.handler.DefaultArtifactHandler;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.CsvSource;
import org.junit.jupiter.params.provider.ValueSource;
import sh.brewlet.maven.plugin.util.JdkVersionResolver;

import java.io.File;
import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.TimeUnit;
import java.util.jar.JarEntry;
import java.util.jar.JarOutputStream;
import java.util.regex.Pattern;

import static org.junit.jupiter.api.Assertions.*;

/** Exercises the plugin in real Maven sessions, with private artifacts/settings/toolchains. */
class JdkInferenceMavenTest {
    @TempDir Path root;
    private Path repository;
    private Path settings;
    private Path emptySettings;
    private Path toolchains;
    private Path emptyToolchains;
    private String goal;
    private String lastOutput;
    private int invocation;

    @Test
    void selectedJdkReportsItsActualFeature() throws Exception {
        assertEquals(Runtime.version().feature(), feature(Path.of(System.getProperty("java.home"))));
    }

    @Test
    void unavailableSelectedJdkFailsExplicitly() {
        Path home = root.resolve("missing-jdk");
        IOException failure = assertThrows(IOException.class, () -> feature(home));
        assertTrue(failure.getMessage().contains(home.toString()), failure.getMessage());
    }

    @ParameterizedTest
    @CsvSource({"21, 21", "17, 17", "1.8, 8"})
    void selectedJdkSpecificationVersionIsParsedIndependently(String version, int expected) {
        assertEquals(expected, reportedFeature(root, "Property settings:\n"
                + "    java.version = 99.0.1\r\n    java.specification.version = " + version + "\r\n"));
    }

    @ParameterizedTest
    @ValueSource(strings = {
            "", "java.version = 21.0.1", "java.specification.version =",
            "java.specification.version = banana", "java.specification.version = 0",
            "java.specification.version = -1", "java.specification.version = 1.0",
            "java.specification.version = 17.0.8", "java.specification.version = 21-ea",
            "java.specification.version = 99999999999999999",
            "java.specification.version = 17\njava.specification.version = 21"})
    void invalidSelectedJdkReportNeverDefaults(String output) {
        AssertionError failure = assertThrows(AssertionError.class, () -> reportedFeature(root, output));
        assertTrue(failure.getMessage().contains(root.toString()), failure.getMessage());
        assertTrue(failure.getMessage().contains(output), failure.getMessage());
    }

    @Test
    void manifestUsesOnlyTheImageBuiltInTheCurrentMavenInvocation() throws Exception {
        preparePlugin();
        Path application = root.resolve("development-app");
        Files.createDirectories(application);
        writeToolchainProject(application, "<release>17</release>", "");
        Path jar = application.resolve("app.jar");
        Files.write(jar, TestApplications.zip(Map.of("META-INF/MANIFEST.MF",
                "Manifest-Version: 1.0\nMain-Class: App\n\n".getBytes(java.nio.charset.StandardCharsets.UTF_8))));
        String prefix = goal.substring(0, goal.lastIndexOf(':') + 1);
        run(application, true, "-Dbrewlet.jarFile=" + jar, "-Dbrewlet.dryRun=false",
                prefix + "build", prefix + "manifest");
        Path output = application.resolve("target/brewlet");
        String digest = AbstractBrewletMojo.MAPPER.readTree(output.resolve("oci/index.json").toFile())
                .path("manifests").get(0).path("digest").asText();
        String yaml = Files.readString(output.resolve("javaapplication.yaml"));
        assertTrue(yaml.contains("\"example.invalid/application@" + digest + "\""), yaml);
        assertTrue(yaml.contains("    version: 17\n"), yaml);
        assertFalse(Files.exists(output.resolve("push.json")), "Local generation must not publish");

        Files.writeString(output.resolve("push.json"),
                "{\"deployImage\":\"other.example.com/app@sha256:" + "2".repeat(64) + "\"}");
        String failure = run(application, false, "-Dbrewlet.dryRun=false",
                "-Dbrewlet.image=other.example.com/app@sha256:" + "2".repeat(64), prefix + "manifest");
        assertTrue(failure.contains("same Maven invocation"), failure);
        assertEquals(yaml, Files.readString(output.resolve("javaapplication.yaml")));

        String dryPush = run(application, false, "-Dbrewlet.jarFile=" + jar,
                goal, prefix + "manifest");
        assertTrue(dryPush.contains("same Maven invocation"), dryPush);
        assertTrue(dryPush.contains("A dry-run push does not produce an image"), dryPush);
        assertEquals(yaml, Files.readString(output.resolve("javaapplication.yaml")));
    }

    @ParameterizedTest
    @ValueSource(strings = {"inherited", "profile", "execution", "explicit", "compiler-toolchain", "session-toolchain"})
    void manifestOutputReflectsJdkPrecedenceWithoutManagedBundles(String scenario) throws Exception {
        preparePlugin();
        Path application = root.resolve("parent/app");
        Files.createDirectories(application);
        int currentFeature = Runtime.version().feature();
        boolean toolchain = scenario.endsWith("toolchain");
        String compiler = toolchain ? "" : "<release>17</release>";
        if ("compiler-toolchain".equals(scenario)) {
            compiler = "<jdkToolchain><version>" + currentFeature + "</version><vendor>fixture</vendor></jdkToolchain>";
        }
        Files.writeString(application.getParent().resolve("pom.xml"), """
                <project><modelVersion>4.0.0</modelVersion>
                  <groupId>test.inference</groupId><artifactId>parent</artifactId><version>1</version><packaging>pom</packaging>
                  <build><plugins><plugin><artifactId>maven-compiler-plugin</artifactId><version>3.16.0</version>
                    <configuration>%s</configuration>
                    <executions><execution><id>default-testCompile</id>
                      <configuration><release>21</release></configuration>
                    </execution></executions>
                  </plugin></plugins></build>
                </project>
                """.formatted(compiler));
        String sessionPlugin = "session-toolchain".equals(scenario) ? """
                <plugin><artifactId>maven-toolchains-plugin</artifactId><version>3.2.0</version>
                  <executions><execution><goals><goal>toolchain</goal></goals></execution></executions>
                  <configuration><toolchains><jdk><version>%d</version><vendor>fixture</vendor></jdk></toolchains></configuration>
                </plugin>
                """.formatted(currentFeature) : "";
        Files.writeString(application.resolve("pom.xml"), """
                <project><modelVersion>4.0.0</modelVersion>
                  <parent><groupId>test.inference</groupId><artifactId>parent</artifactId><version>1</version></parent>
                  <artifactId>app</artifactId>
                  <build><plugins>%s</plugins></build>
                  <profiles>
                    <profile><id>profile</id><build><plugins><plugin><artifactId>maven-compiler-plugin</artifactId>
                      <configuration><release>11</release></configuration>
                    </plugin></plugins></build></profile>
                    <profile><id>execution</id><build><plugins><plugin><artifactId>maven-compiler-plugin</artifactId>
                      <executions><execution><id>default-compile</id><configuration><release>8</release></configuration>
                      </execution></executions>
                    </plugin></plugins></build></profile>
                  </profiles>
                </project>
                """.formatted(sessionPlugin));
        Path jar = application.resolve("app.jar");
        Files.write(jar, TestApplications.zip(Map.of("META-INF/MANIFEST.MF",
                "Manifest-Version: 1.0\nMain-Class: App\n\n".getBytes(java.nio.charset.StandardCharsets.UTF_8))));
        List<String> args = new ArrayList<>(List.of("-Dbrewlet.jarFile=" + jar,
                "-Dbrewlet.dryRun=false", "-Dbrewlet.dependencyBundle="));
        int expected = switch (scenario) {
            case "profile", "explicit" -> 11;
            case "execution" -> 8;
            default -> toolchain ? currentFeature : 17;
        };
        if (!toolchain) args.add("-Dmaven.compiler.release=8");
        if ("profile".equals(scenario) || "execution".equals(scenario)) args.add("-P" + scenario);
        if ("explicit".equals(scenario)) args.add("-Dbrewlet.jdkFeature=11");
        if ("session-toolchain".equals(scenario)) args.add("validate");
        String prefix = goal.substring(0, goal.lastIndexOf(':') + 1);
        args.add(prefix + "build");
        args.add(prefix + "manifest");
        run(application, true, args.toArray(String[]::new));
        Path output = application.resolve("target/brewlet");
        String yaml = Files.readString(output.resolve("javaapplication.yaml"));
        assertTrue(yaml.contains("    version: " + expected + "\n"), yaml);
        assertFalse(Files.exists(output.resolve("push.json")));
        String digest = AbstractBrewletMojo.MAPPER.readTree(output.resolve("oci/index.json").toFile())
                .path("manifests").get(0).path("digest").asText();
        var index = AbstractBrewletMojo.MAPPER.readTree(new LocalStore(output.resolve("oci")).blobPath(digest).toFile());
        assertFalse(index.path("annotations").has(MediaTypes.MANAGED_DEPENDENCY_EVIDENCE_ANNOTATION));
    }

    @ParameterizedTest
    @ValueSource(booleans = {false, true})
    void realMavenBuildAndPushShareManagedImageInEitherOrder(boolean pushFirst) throws Exception {
        preparePlugin();
        Path application = root.resolve("ordered-goals");
        Files.createDirectories(application);
        writeToolchainProject(application, "<release>17</release>", "");
        Path jar = application.resolve("app.jar");
        Files.write(jar, TestApplications.zip(Map.of("META-INF/MANIFEST.MF",
                "Manifest-Version: 1.0\nMain-Class: App\n\n".getBytes(java.nio.charset.StandardCharsets.UTF_8))));
        String prefix = goal.substring(0, goal.lastIndexOf(':') + 1);
        try (TestImageRegistry registry = new TestImageRegistry()) {
            String log = run(application, true, "-Dbrewlet.jarFile=" + jar, "-Dbrewlet.dryRun=false",
                    "-Dbrewlet.image=" + registry.image(),
                    prefix + (pushFirst ? "push" : "build"), prefix + (pushFirst ? "build" : "push"),
                    prefix + "build", prefix + "manifest");
            assertEquals(1, log.split("Brewlet: assembled image ", -1).length - 1, log);
            assertEquals(2, log.split("Brewlet: reusing assembled image ", -1).length - 1, log);
            assertFalse(log.contains("this image has not been published"), log);
            Path output = application.resolve("target/brewlet");
            String digest = LocalStore.sha256Hex(registry.tagged);
            assertEquals(digest, AbstractBrewletMojo.MAPPER.readTree(output.resolve("oci/index.json").toFile())
                    .path("manifests").get(0).path("digest").asText());
            assertEquals(digest, AbstractBrewletMojo.MAPPER.readTree(output.resolve("push.json").toFile())
                    .path("digest").asText());
            assertTrue(Files.readString(output.resolve("javaapplication.yaml"))
                    .contains(registry.authority() + "/app@" + digest));
            var index = AbstractBrewletMojo.MAPPER.readTree(registry.tagged);
            assertTrue(index.path("annotations").has(MediaTypes.MANAGED_DEPENDENCY_EVIDENCE_ANNOTATION));
            LocalStore store = new LocalStore(output.resolve("oci"));
            for (var blob : registry.content.entrySet()) {
                assertArrayEquals(blob.getValue(), Files.readAllBytes(store.blobPath(blob.getKey())));
            }
            assertEquals(1, registry.taggedWrites.get());
        }
    }

    @Test
    void realMavenLifecycleOrderAndDisabledBindingsAreRespected() throws Exception {
        preparePlugin();
        Path application = root.resolve("lifecycle-application");
        Files.createDirectories(application.resolve("src/main/java"));
        Files.writeString(application.resolve("src/main/java/App.java"),
                "public class App { public static void main(String[] args) {} }\n");
        int currentFeature = Runtime.version().feature();
        Files.writeString(application.resolve("pom.xml"), """
                <project xmlns="http://maven.apache.org/POM/4.0.0"><modelVersion>4.0.0</modelVersion>
                  <groupId>test.inference</groupId><artifactId>lifecycle-application</artifactId><version>1</version>
                  <properties><disabled.phase>none</disabled.phase></properties>
                  <build><plugins>
                    <plugin><artifactId>maven-compiler-plugin</artifactId><version>3.16.0</version>
                      <configuration><release>8</release></configuration>
                      <executions>
                        <execution><id>default-compile</id><phase>${disabled.phase}</phase>
                          <configuration><release>8</release></configuration></execution>
                        <execution><id>replacement-main</id><phase>compile</phase><goals><goal>compile</goal></goals>
                          <configuration><release>%d</release></configuration></execution>
                        <execution><id>default-testCompile</id><configuration><release>25</release></configuration></execution>
                      </executions>
                    </plugin>
                    <plugin><artifactId>maven-jar-plugin</artifactId><version>3.4.2</version>
                      <configuration><archive><manifest><mainClass>App</mainClass></manifest></archive></configuration>
                    </plugin>
                  </plugins></build>
                </project>
                """.formatted(currentFeature));
        for (String phase : List.of("none", "disabled", "never")) {
            String output = run(application, true, "-Ddisabled.phase=" + phase, "clean", "package", goal);
            assertFalse(output.contains(":compile (default-compile)"), output);
            assertTrue(output.contains(":compile (replacement-main)"), output);
            assertCompiledAndInferred(application, currentFeature);
        }

        Path currentHome = Path.of(System.getProperty("java.home"));
        java.util.Map<Integer, Path> homes = new java.util.HashMap<>();
        homes.put(currentFeature, currentHome);
        String other = System.getProperty("brewlet.test.otherJdk");
        if (other != null) homes.put(feature(Path.of(other)), Path.of(other));
        Path mainHome = homes.getOrDefault(21, currentHome);
        Path lateHome = homes.getOrDefault(17, currentHome);
        int mainFeature = feature(mainHome);
        writeToolchains(new ArrayList<>(new java.util.LinkedHashSet<>(List.of(mainHome, lateHome))));
        String lateExecution = """
                <execution><id>late-jdk</id><phase>compile</phase><goals><goal>toolchain</goal></goals>
                  <configuration><toolchains><jdk><version>%d</version><vendor>fixture</vendor></jdk></toolchains></configuration>
                </execution>
                """.formatted(feature(lateHome));
        String template = """
                <project xmlns="http://maven.apache.org/POM/4.0.0"><modelVersion>4.0.0</modelVersion>
                  <groupId>test.inference</groupId><artifactId>lifecycle-application</artifactId><version>1</version>
                  <build><plugins>
                    <plugin><artifactId>maven-compiler-plugin</artifactId><version>3.16.0</version></plugin>
                    <plugin><artifactId>maven-toolchains-plugin</artifactId><version>3.2.0</version><executions>
                      <execution><id>main-jdk</id><phase>validate</phase><goals><goal>toolchain</goal></goals>
                        <configuration><toolchains><jdk><version>%d</version><vendor>fixture</vendor></jdk></toolchains></configuration>
                      </execution>%s
                    </executions></plugin>
                    <plugin><artifactId>maven-jar-plugin</artifactId><version>3.4.2</version>
                      <configuration><archive><manifest><mainClass>App</mainClass></manifest></archive></configuration>
                    </plugin>
                  </plugins></build>
                </project>
                """;
        Files.writeString(application.resolve("pom.xml"), template.formatted(mainFeature, lateExecution));
        String ambiguous = run(application, false, "clean", "package", goal);
        int selectMain = ambiguous.indexOf(":toolchain (main-jdk)");
        int compileMain = ambiguous.indexOf(":compile (default-compile)");
        int selectLate = ambiguous.indexOf(":toolchain (late-jdk)");
        assertTrue(selectMain >= 0 && compileMain > selectMain && selectLate > compileMain, ambiguous);
        assertTrue(ambiguous.substring(compileMain, selectLate).contains(
                "Toolchain in maven-compiler-plugin: JDK[" + mainHome), ambiguous);
        assertTrue(ambiguous.contains("at or after main compilation"), ambiguous);
        run(application, true, "-Dbrewlet.jdkFeature=" + mainFeature, goal);
        assertInferred(application, mainFeature);
        Files.writeString(application.resolve("pom.xml"), template.formatted(mainFeature, ""));
        run(application, true, goal);
        assertInferred(application, mainFeature);
        System.out.println("Verified " + invocation + " actual Maven lifecycle invocations: disabled default bindings, "
                + "replacement compilation, main-before-late toolchain ordering, and explicit override.");
    }

    @Test
    void effectiveCompilerConfigurationAndToolchainsDriveBundleCompatibility() throws Exception {
        preparePlugin();
        Path application = root.resolve("parent/application");
        Files.createDirectories(application.resolve("src/main/java"));
        Files.writeString(application.resolve("src/main/java/App.java"),
                "public class App { public static void main(String[] args) {} }\n");
        Files.writeString(application.getParent().resolve("pom.xml"), """
                <project xmlns="http://maven.apache.org/POM/4.0.0">
                  <modelVersion>4.0.0</modelVersion>
                  <groupId>test.inference</groupId><artifactId>parent</artifactId><version>1</version><packaging>pom</packaging>
                  <build><plugins>
                    <plugin><groupId>org.apache.maven.plugins</groupId><artifactId>maven-compiler-plugin</artifactId>
                      <version>3.16.0</version><configuration><release>17</release></configuration>
                      <executions><execution><id>default-testCompile</id>
                        <configuration><release>21</release></configuration>
                      </execution></executions>
                    </plugin>
                    <plugin><groupId>org.apache.maven.plugins</groupId><artifactId>maven-jar-plugin</artifactId>
                      <version>3.4.2</version><configuration><archive><manifest><mainClass>App</mainClass></manifest></archive></configuration>
                    </plugin>
                  </plugins></build>
                  <profiles><profile><id>property-release</id>
                    <properties><application.release>17</application.release></properties>
                    <build><plugins><plugin><artifactId>maven-compiler-plugin</artifactId>
                      <configuration><release>${application.release}</release></configuration>
                    </plugin></plugins></build>
                  </profile></profiles>
                </project>
                """);
        Files.writeString(application.resolve("pom.xml"), """
                <project xmlns="http://maven.apache.org/POM/4.0.0">
                  <modelVersion>4.0.0</modelVersion>
                  <parent><groupId>test.inference</groupId><artifactId>parent</artifactId><version>1</version></parent>
                  <artifactId>application</artifactId>
                  <profiles>
                    <profile><id>execution-release</id><properties><main.release>11</main.release></properties>
                      <build><plugins><plugin><artifactId>maven-compiler-plugin</artifactId><executions>
                        <execution><id>default-compile</id><configuration><release>${main.release}</release></configuration></execution>
                      </executions></plugin></plugins></build>
                    </profile>
                    <profile><id>ambiguous-main</id><build><plugins><plugin><artifactId>maven-compiler-plugin</artifactId><executions>
                      <execution><id>other-main</id><goals><goal>compile</goal></goals><configuration><release>21</release></configuration></execution>
                    </executions></plugin></plugins></build></profile>
                  </profiles>
                </project>
                """);

        run(application, true, "-Dmaven.compiler.release=8", "clean", "package", goal);
        assertCompiledAndInferred(application, 17);
        run(application, true, "-Dmaven.compiler.release=8", goal);
        assertInferred(application, 17); // Fresh standalone session, conventional packaged-JAR resolution.
        run(application, true, "-Pproperty-release", "-Dapplication.release=11",
                "-Dmaven.compiler.release=8", "clean", "package", goal);
        assertCompiledAndInferred(application, 11);
        run(application, true, "-Pexecution-release", "-Dmain.release=8",
                "-Dmaven.compiler.release=11", "clean", "package", goal);
        assertCompiledAndInferred(application, 8);
        String ambiguous = run(application, false, "-Pambiguous-main", goal);
        assertTrue(ambiguous.contains("different application JDKs"), ambiguous);

        Path selected = root.resolve("toolchain-application");
        Files.createDirectories(selected);
        Path appJar = application.resolve("target/application-1.jar");
        writeToolchainProject(selected, "<jdkToolchain><version>${compiler.requirement}</version>"
                + "<vendor>fixture</vendor></jdkToolchain>", "");
        Path currentHome = Path.of(System.getProperty("java.home"));
        int currentFeature = feature(currentHome);
        writeToolchains(List.of(currentHome));
        run(selected, true, "-Dbrewlet.jarFile=" + appJar, "-Dcompiler.requirement=" + currentFeature, goal);
        assertInferred(selected, currentFeature);

        String other = System.getProperty("brewlet.test.otherJdk");
        if (other != null) {
            Path otherHome = Path.of(other);
            int otherFeature = feature(otherHome);
            writeToolchains(List.of(otherHome, currentHome));
            run(selected, true, "-Dbrewlet.jarFile=" + appJar, "-Dcompiler.requirement=[17,30)", goal);
            assertInferred(selected, otherFeature); // First configured match, not Maven's JDK or maximum.
            run(selected, true, "-Dbrewlet.jarFile=" + appJar, "-Dcompiler.requirement=" + currentFeature, goal);
            assertInferred(selected, currentFeature);
        }
        String noMatch = run(selected, false, "-Dbrewlet.jarFile=" + appJar,
                "-Dcompiler.requirement=[999,1000)", goal);
        assertTrue(noMatch.contains("No configured JDK"), noMatch);
        run(selected, true, "-Dbrewlet.jarFile=" + appJar, "-Dcompiler.requirement=[999,1000)",
                "-Dbrewlet.jdkFeature=17", goal);
        assertInferred(selected, 17);
        String invalidOverride = run(selected, false, "-Dbrewlet.jarFile=" + appJar,
                "-Dbrewlet.jdkFeature=0", goal);
        assertTrue(invalidOverride.contains("positive JDK feature"), invalidOverride);

        writeToolchainProject(selected, "", """
                <plugin><artifactId>maven-toolchains-plugin</artifactId><version>3.2.0</version>
                  <executions><execution><goals><goal>toolchain</goal></goals></execution></executions>
                  <configuration><toolchains><jdk><version>${compiler.requirement}</version><vendor>fixture</vendor></jdk></toolchains></configuration>
                </plugin>
                """);
        run(selected, true, "-Dbrewlet.jarFile=" + appJar, "-Dcompiler.requirement=" + currentFeature, goal);
        assertInferred(selected, currentFeature);
        String selectedInSession = run(selected, true, "-Dbrewlet.jarFile=" + appJar,
                "-Dcompiler.requirement=" + currentFeature, "validate", goal);
        assertTrue(selectedInSession.contains("session-selected JDK toolchain"), selectedInSession);
        writeToolchainProject(selected, "<jdkToolchain><version>[999,1000)</version></jdkToolchain>", """
                <plugin><artifactId>maven-toolchains-plugin</artifactId><version>3.2.0</version>
                  <executions><execution><goals><goal>toolchain</goal></goals></execution></executions>
                  <configuration><toolchains><jdk><version>${compiler.requirement}</version><vendor>fixture</vendor></jdk></toolchains></configuration>
                </plugin>
                """);
        String compilerFallback = run(selected, true, "-Dbrewlet.jarFile=" + appJar,
                "-Dcompiler.requirement=" + currentFeature, "validate", goal);
        assertTrue(compilerFallback.contains("session-selected JDK toolchain"), compilerFallback);
        assertInferred(selected, currentFeature);
        writeToolchainProject(selected, "", """
                <plugin><artifactId>maven-toolchains-plugin</artifactId><version>3.2.0</version>
                  <executions><execution><goals><goal>toolchain</goal></goals></execution></executions>
                  <configuration><toolchains><jdk><version>${compiler.requirement}</version><vendor>fixture</vendor></jdk></toolchains></configuration>
                </plugin>
                """);
        Files.writeString(toolchains, "<toolchains/>");
        String unavailable = run(selected, false, "-Dbrewlet.jarFile=" + appJar,
                "-Dcompiler.requirement=" + currentFeature, goal);
        assertTrue(unavailable.contains("No configured JDK toolchain"), unavailable);
        System.out.println("Verified " + invocation + " actual Maven invocations: inherited/profile/execution compiler "
                + "precedence, bytecode levels, standalone publication, and configured/session toolchain selection.");
    }

    private void preparePlugin() throws Exception {
        Path module = Path.of(System.getProperty("basedir", ".")).toAbsolutePath();
        Model model;
        try (var reader = Files.newBufferedReader(module.resolve("pom.xml"))) {
            model = new MavenXpp3Reader().read(reader);
        }
        repository = root.resolve("repository");
        Path artifact = repository.resolve(model.getGroupId().replace('.', '/'))
                .resolve(model.getArtifactId()).resolve(model.getVersion());
        Files.createDirectories(artifact);
        String file = model.getArtifactId() + "-" + model.getVersion();
        Files.copy(module.resolve("pom.xml"), artifact.resolve(file + ".pom"));
        Path classes = Path.of(JdkVersionResolver.class.getProtectionDomain().getCodeSource().getLocation().toURI());
        assertTrue(Files.isRegularFile(classes.resolve("META-INF/maven/plugin.xml")),
                "Maven process-classes must generate the real plugin descriptor before this test");
        try (var jar = new JarOutputStream(Files.newOutputStream(artifact.resolve(file + ".jar")));
             var files = Files.walk(classes)) {
            for (Path path : files.filter(Files::isRegularFile).sorted().toList()) {
                jar.putNextEntry(new JarEntry(classes.relativize(path).toString().replace(File.separatorChar, '/')));
                Files.copy(path, jar);
                jar.closeEntry();
            }
        }
        goal = model.getGroupId() + ":" + model.getArtifactId() + ":" + model.getVersion() + ":push";
        prepareDependencyBundle();
        Path cache = Path.of(System.getProperty("maven.repo.local",
                Path.of(System.getProperty("user.home"), ".m2/repository").toString()));
        settings = root.resolve("settings.xml");
        // Reuse immutable dependencies from this build as a file repository.
        // Central remains a fallback for standard lifecycle plugins not used yet.
        Files.writeString(settings, """
                <settings><profiles><profile><id>build-cache</id>
                  <repositories><repository><id>build-cache</id><url>%s</url><releases><checksumPolicy>ignore</checksumPolicy></releases></repository></repositories>
                  <pluginRepositories><pluginRepository><id>build-cache</id><url>%s</url><releases><checksumPolicy>ignore</checksumPolicy></releases></pluginRepository></pluginRepositories>
                </profile></profiles><activeProfiles><activeProfile>build-cache</activeProfile></activeProfiles></settings>
                """.formatted(cache.toUri(), cache.toUri()));
        emptySettings = root.resolve("empty-settings.xml");
        Files.writeString(emptySettings, "<settings/>");
        toolchains = root.resolve("toolchains.xml");
        emptyToolchains = root.resolve("empty-toolchains.xml");
        Files.writeString(emptyToolchains, "<toolchains/>");
        writeToolchains(List.of(Path.of(System.getProperty("java.home"))));
    }

    private void prepareDependencyBundle() throws Exception {
        Path artifact = repository.resolve("test/inference/library/1");
        Files.createDirectories(artifact);
        Path jar = artifact.resolve("library-1.jar");
        Files.write(jar, TestApplications.zip(Map.of("fixture.txt", new byte[]{1})));
        Files.writeString(artifact.resolve("library-1.pom"), """
                <project><modelVersion>4.0.0</modelVersion>
                  <groupId>test.inference</groupId><artifactId>library</artifactId><version>1</version>
                </project>
                """);
        DefaultArtifactHandler handler = new DefaultArtifactHandler("jar");
        handler.setAddedToClasspath(true);
        DefaultArtifact dependency = new DefaultArtifact(
                "test.inference", "library", "1", "compile", "jar", null, handler);
        dependency.setFile(jar.toFile());
        DependencyBundleMojo bundle = new DependencyBundleMojo();
        TestApplications.configure(bundle, jar, root.resolve("bundle-output"), false);
        bundle.project.setArtifacts(Set.of(dependency));
        bundle.dryRun = true;
        TestApplications.set(bundle, "sourceBom", "test.inference:platform:1");
        TestApplications.set(bundle, "compatibleJdks",
                java.util.stream.IntStream.rangeClosed(1, Runtime.version().feature() + 10).boxed().toList());
        TestApplications.set(bundle, "dependencyBundleOutputDirectory", root.resolve("bundle").toFile());
        bundle.execute();
    }

    private void writeToolchainProject(Path project, String compilerConfiguration, String otherPlugins) throws Exception {
        Files.writeString(project.resolve("pom.xml"), """
                <project xmlns="http://maven.apache.org/POM/4.0.0"><modelVersion>4.0.0</modelVersion>
                  <groupId>test.inference</groupId><artifactId>toolchain-application</artifactId><version>1</version>
                  <properties><compiler.requirement>[17,30)</compiler.requirement></properties>
                  <build><plugins><plugin><artifactId>maven-compiler-plugin</artifactId><version>3.16.0</version>
                    <configuration>%s</configuration></plugin>%s</plugins></build>
                </project>
                """.formatted(compilerConfiguration, otherPlugins));
    }

    private void writeToolchains(List<Path> homes) throws Exception {
        StringBuilder xml = new StringBuilder("<toolchains>");
        for (Path home : homes) {
            xml.append("<toolchain><type>jdk</type><provides><version>").append(feature(home))
                    .append("</version><vendor>fixture</vendor></provides><configuration><jdkHome>")
                    .append(home.toString().replace("&", "&amp;").replace("<", "&lt;"))
                    .append("</jdkHome></configuration></toolchain>");
        }
        Files.writeString(toolchains, xml.append("</toolchains>").toString());
    }

    private int feature(Path home) throws Exception {
        Path java = home.resolve("bin").resolve(File.separatorChar == '\\' ? "java.exe" : "java");
        Path log = Files.createTempFile(root, "jdk-version-", ".log");
        Process process;
        try {
            process = new ProcessBuilder(java.toString(), "-XshowSettings:properties", "-version")
                    .redirectErrorStream(true).redirectOutput(log.toFile()).start();
        } catch (IOException e) {
            throw new IOException("Cannot query selected JDK at " + home, e);
        }
        try {
            boolean finished = process.waitFor(20, TimeUnit.SECONDS);
            String output = Files.readString(log);
            assertTrue(finished, "Selected JDK query exceeded its deadline at " + home + "\n" + output);
            assertEquals(0, process.exitValue(), "Selected JDK query failed at " + home + "\n" + output);
            return reportedFeature(home, output);
        } finally {
            if (process.isAlive()) {
                process.toHandle().destroyForcibly();
                assertTrue(process.waitFor(5, TimeUnit.SECONDS), "Selected JDK failed to terminate at " + home);
            }
        }
    }

    private static int reportedFeature(Path home, String output) {
        String diagnostic = "Invalid java.specification.version reported by selected JDK at " + home + "\n" + output;
        List<String> versions = Pattern.compile("(?m)^\\h*java\\.specification\\.version\\h*=\\h*([^\\r\\n]*)$")
                .matcher(output).results().map(match -> match.group(1).trim()).toList();
        assertEquals(1, versions.size(), diagnostic);
        String version = versions.get(0);
        assertTrue(version.matches("[1-9][0-9]*|1\\.[1-8]"), diagnostic);
        String feature = version.startsWith("1.") ? version.substring(2) : version;
        return assertDoesNotThrow(() -> Integer.parseInt(feature), diagnostic);
    }

    private String run(Path project, boolean success, String... arguments) throws Exception {
        Path pom = project.resolve("pom.xml");
        String xml = Files.readString(pom);
        if (!xml.contains("<artifactId>library</artifactId>")) {
            Files.writeString(pom, xml.replace("</project>", """
                    <dependencies><dependency><groupId>test.inference</groupId>
                      <artifactId>library</artifactId><version>1</version>
                    </dependency></dependencies></project>"""));
        }
        String mavenHome = System.getProperty("maven.home");
        String maven = mavenHome == null ? "mvn" : Path.of(mavenHome, "bin", "mvn").toString();
        List<String> command = new ArrayList<>(List.of(maven,
                "-B", "-ntp", "-nsu", "--settings", settings.toString(), "--global-settings", emptySettings.toString(),
                "--toolchains", toolchains.toString(), "--global-toolchains", emptyToolchains.toString(),
                "-Dmaven.repo.local=" + repository, "-Dmaven.test.skip=true",
                "-Dbrewlet.image=example.invalid/application:1", "-Dbrewlet.dryRun=true",
                "-Dbrewlet.mainClass=App", "-Dbrewlet.dependencyBundle=" + root.resolve("bundle")));
        command.addAll(List.of(arguments));
        Path log = root.resolve("maven-" + ++invocation + ".log");
        ProcessBuilder builder = new ProcessBuilder(command).directory(project.toFile())
                .redirectErrorStream(true).redirectOutput(log.toFile());
        builder.environment().put("JAVA_HOME", System.getProperty("java.home"));
        builder.environment().put("MAVEN_OPTS", "-Djava.io.tmpdir=" + root);
        Process process = builder.start();
        try {
            assertTrue(process.waitFor(120, TimeUnit.SECONDS), "Maven invocation exceeded its deadline: " + command);
            String output = Files.readString(log);
            lastOutput = output;
            if (success) assertEquals(0, process.exitValue(), output);
            else {
                assertNotEquals(0, process.exitValue(), output);
                assertTrue(output.contains("BUILD FAILURE"), output);
            }
            return output;
        } finally {
            if (process.isAlive()) {
                process.toHandle().destroyForcibly();
                assertTrue(process.waitFor(5, TimeUnit.SECONDS), "Nested Maven failed to terminate");
            }
        }
    }

    private void assertCompiledAndInferred(Path application, int feature) throws Exception {
        byte[] bytecode = Files.readAllBytes(application.resolve("target/classes/App.class"));
        int major = Byte.toUnsignedInt(bytecode[6]) * 256 + Byte.toUnsignedInt(bytecode[7]);
        assertEquals(feature + 44, major, "Actual Maven compiler target differs from expected feature");
        assertInferred(application, feature);
    }

    private void assertInferred(Path application, int feature) {
        assertTrue(lastOutput.contains("application JDK " + feature + " from "), lastOutput);
        assertFalse(Files.exists(application.resolve("target/brewlet/javaapplication.yaml")));
        assertFalse(Files.exists(application.resolve("target/brewlet/push.json")), "Dry run must not publish");
    }
}
