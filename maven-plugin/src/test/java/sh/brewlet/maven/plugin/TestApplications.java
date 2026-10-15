// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import org.apache.maven.project.MavenProject;
import sh.brewlet.maven.plugin.model.JvmConfig;

import javax.tools.ToolProvider;
import java.io.ByteArrayOutputStream;
import java.io.File;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.TimeUnit;
import java.util.zip.ZipEntry;
import java.util.zip.ZipOutputStream;

import static org.junit.jupiter.api.Assertions.*;

final class TestApplications {
    private TestApplications() {}

    static final String MAIN = """
            package app;
            public class Main {
                public static void main(String[] args) throws Exception {
                    try (var resource = Main.class.getResourceAsStream("/message.txt")) {
                        System.out.println(lib.Dependency.message() + ":" +
                            new String(resource.readAllBytes(), java.nio.charset.StandardCharsets.UTF_8));
                    }
                }
            }
            """;

    record Boot(Path source, Map<String, byte[]> entries, byte[] first, byte[] second) {}

    static Boot boot(Path root) throws Exception {
        Path firstClasses = compile(root.resolve("first"), "lib.Dependency",
                "package lib; public class Dependency { public static String message() { return \"first\"; } }");
        Path secondClasses = compile(root.resolve("second"), "lib.Dependency",
                "package lib; public class Dependency { public static String message() { return \"second\"; } }");
        byte[] first = zip(Map.of("lib/Dependency.class", Files.readAllBytes(firstClasses.resolve("lib/Dependency.class"))));
        byte[] second = zip(Map.of("lib/Dependency.class", Files.readAllBytes(secondClasses.resolve("lib/Dependency.class"))));
        Path library = root.resolve("compile-dependency.jar");
        Files.write(library, first);
        Path appClasses = compile(root.resolve("app"), "app.Main", MAIN, "-classpath", library.toString());
        Map<String, byte[]> entries = new LinkedHashMap<>();
        entries.put("META-INF/MANIFEST.MF", ("""
                Manifest-Version: 1.0
                Main-Class: org.springframework.boot.loader.launch.JarLauncher
                Start-Class: app.Main
                Spring-Boot-Classes: BOOT-INF/classes/
                Spring-Boot-Lib: BOOT-INF/lib/

                """).getBytes(StandardCharsets.UTF_8));
        entries.put("BOOT-INF/classes/app/Main.class", Files.readAllBytes(appClasses.resolve("app/Main.class")));
        entries.put("BOOT-INF/classes/message.txt", "resource".getBytes(StandardCharsets.UTF_8));
        // Deliberately different ZIP order, filename order, layer order, and runtime order.
        entries.put("BOOT-INF/lib/a.jar", second);
        entries.put("BOOT-INF/lib/z-SNAPSHOT.jar", first);
        entries.put("BOOT-INF/classpath.idx", """
                - "BOOT-INF/lib/z-SNAPSHOT.jar"
                - "BOOT-INF/lib/a.jar"
                """.getBytes(StandardCharsets.UTF_8));
        Path source = root.resolve("boot.jar");
        Files.write(source, zip(entries));
        return new Boot(source, entries, first, second);
    }

    static Path compile(Path root, String name, String source, String... options) throws Exception {
        Path java = root.resolve("sources").resolve(name.replace('.', '/') + ".java");
        Files.createDirectories(java.getParent());
        Files.writeString(java, source);
        Path classes = root.resolve("classes");
        Files.createDirectories(classes);
        List<String> args = new ArrayList<>(List.of("--release", "17", "-d", classes.toString()));
        args.addAll(List.of(options));
        args.add(java.toString());
        assertEquals(0, ToolProvider.getSystemJavaCompiler().run(null, null, null, args.toArray(String[]::new)));
        return classes;
    }

    static byte[] zip(Map<String, byte[]> entries) throws Exception {
        ByteArrayOutputStream out = new ByteArrayOutputStream();
        try (ZipOutputStream zip = new ZipOutputStream(out)) {
            for (var entry : entries.entrySet()) {
                zip.putNextEntry(new ZipEntry(entry.getKey()));
                zip.write(entry.getValue());
                zip.closeEntry();
            }
        }
        return out.toByteArray();
    }

    static void configure(AbstractBrewletMojo mojo, Path jar, Path output, boolean layered) {
        mojo.project = new MavenProject();
        mojo.project.setGroupId("test");
        mojo.project.setArtifactId("app");
        mojo.project.setVersion("1");
        mojo.jarFile = jar.toFile();
        mojo.outputDirectory = output.toFile();
        mojo.layered = layered;
        mojo.splitSnapshotLayers = true;
        mojo.image = "example.test/app:1";
        mojo.format = "artifact";
    }

    static void set(Object target, String name, Object value) throws Exception {
        Class<?> type = target.getClass();
        while (type != null) {
            try {
                var field = type.getDeclaredField(name);
                field.setAccessible(true);
                field.set(target, value);
                return;
            } catch (NoSuchFieldException e) {
                type = type.getSuperclass();
            }
        }
        throw new NoSuchFieldException(name);
    }

    static List<String> launchSelector(JvmConfig cfg, String mainJar) {
        var entry = cfg != null ? cfg.getEntry() : null;
        String mode = entry != null && entry.getMode() != null ? entry.getMode() : "jar";
        return switch (mode) {
            case "classpath" -> {
                String cp = entry.getClassPath() != null && !entry.getClassPath().isEmpty()
                        ? String.join(File.pathSeparator, entry.getClassPath())
                        : mainJar + File.pathSeparator + "lib/*";
                yield List.of("-cp", cp, entry.getMainClass());
            }
            case "module" -> {
                String mp = entry.getModulePath() != null && !entry.getModulePath().isEmpty()
                        ? String.join(File.pathSeparator, entry.getModulePath())
                        : mainJar;
                String target = entry.getMainClass() != null && !entry.getMainClass().isEmpty()
                        ? entry.getModule() + "/" + entry.getMainClass()
                        : entry.getModule();
                yield List.of("-p", mp, "-m", target);
            }
            default -> List.of("-jar", mainJar);
        };
    }

    static String java(Path directory, List<String> args) throws Exception {
        List<String> command = new ArrayList<>();
        command.add(Path.of(System.getProperty("java.home"), "bin", "java").toString());
        command.addAll(args);
        Path log = directory.resolve("jvm.log");
        Process process = new ProcessBuilder(command).directory(directory.toFile())
                .redirectErrorStream(true).redirectOutput(log.toFile()).start();
        try {
            assertTrue(process.waitFor(20, TimeUnit.SECONDS), "JVM timed out");
            String output = Files.readString(log);
            assertEquals(0, process.exitValue(), output);
            return output;
        } finally {
            if (process.isAlive()) {
                process.destroyForcibly();
                assertTrue(process.waitFor(5, TimeUnit.SECONDS));
            }
        }
    }
}
