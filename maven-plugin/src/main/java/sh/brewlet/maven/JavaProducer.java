package sh.brewlet.maven;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.SerializationFeature;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.LinkOption;
import java.nio.file.Path;
import java.nio.file.StandardCopyOption;
import java.nio.file.attribute.FileTime;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.jar.JarFile;

/** Java project knowledge stops at this producer; descriptors are checked with the Java contract before the CLI runs. */
final class JavaProducer {
    record Inputs(Path jar, List<Path> dependencies, Path launch, String mainClass,
                  String module, List<String> javaArgs, List<String> runtimeImages, Path appCdsArchive) {
        Inputs(Path jar, List<Path> dependencies, Path launch, String mainClass,
               String module, List<String> javaArgs, List<String> runtimeImages) {
            this(jar, dependencies, launch, mainClass, module, javaArgs, runtimeImages, null);
        }
    }
    static final String APP_CDS_PATH = "/app/app.jsa";
    record Prepared(Path launch, Path application, Path dependencies, String dependencyTarget) {}

    static Prepared prepare(Inputs inputs, Path output) throws IOException {
        requireFile(inputs.jar());
        Path app = Files.createDirectory(output.resolve("application"));
        Files.copy(inputs.jar(), app.resolve("app.jar"), StandardCopyOption.COPY_ATTRIBUTES);
        if (inputs.appCdsArchive() != null) {
            requireFile(inputs.appCdsArchive());
            // COPY_ATTRIBUTES keeps the archive mtime, which AppCDS validates at startup.
            Files.copy(inputs.appCdsArchive(), app.resolve("app.jsa"), StandardCopyOption.COPY_ATTRIBUTES);
        }
        Path dependencies = null;
        if (!inputs.dependencies().isEmpty()) {
            dependencies = Files.createDirectory(output.resolve("dependencies"));
            for (Path source : inputs.dependencies().stream()
                    .sorted(Comparator.comparing(path -> path.getFileName().toString())).toList()) {
                requireFile(source);
                Path target = dependencies.resolve(source.getFileName().toString());
                if (Files.exists(target, LinkOption.NOFOLLOW_LINKS)) {
                    throw new IOException("Duplicate dependency filename: " + source.getFileName());
                }
                Files.copy(source, target, StandardCopyOption.COPY_ATTRIBUTES);
            }
            Files.setLastModifiedTime(dependencies, FileTime.fromMillis(0));
        }
        Files.setLastModifiedTime(app, FileTime.fromMillis(0));
        Path launch = inputs.launch();
        if (launch != null) {
            requireFile(launch);
            ArtifactContract.parseLaunch(new String(Files.readAllBytes(launch), StandardCharsets.UTF_8));
        } else {
            String mainClass = inputs.mainClass();
            if (mainClass == null || mainClass.isBlank()) {
                try (JarFile jar = new JarFile(inputs.jar().toFile())) {
                    if (jar.getManifest() != null) {
                        mainClass = jar.getManifest().getMainAttributes().getValue("Main-Class");
                    }
                }
            }
            List<String> args = new ArrayList<>(inputs.javaArgs());
            if (inputs.appCdsArchive() != null) {
                args.addAll(List.of("-XX:SharedArchiveFile=" + APP_CDS_PATH, "-Xshare:auto"));
            }
            if (inputs.module() != null && !inputs.module().isBlank()) {
                args.addAll(List.of("--module-path", "/app/app.jar:/app/mods", "--module",
                        inputs.module() + (mainClass == null ? "" : "/" + mainClass)));
            } else if (mainClass != null && !mainClass.isBlank()) {
                args.addAll(List.of("--class-path", "/app/app.jar:/app/lib/*", mainClass));
            } else {
                throw new IOException("Set brewlet.mainClass, brewlet.module, or brewlet.launch; "
                        + "the project JAR has no Main-Class");
            }
            Map<String, Object> descriptor = new LinkedHashMap<>();
            descriptor.put("schemaVersion", 2);
            descriptor.put("process", Map.of("entrypoint", List.of("java"),
                    "args", args, "workingDir", "/app"));
            if (!inputs.runtimeImages().isEmpty()) {
                descriptor.put("runtimeImages", inputs.runtimeImages());
            }
            launch = output.resolve("launch.json");
            new ObjectMapper().enable(SerializationFeature.ORDER_MAP_ENTRIES_BY_KEYS)
                    .writerWithDefaultPrettyPrinter().writeValue(launch.toFile(), descriptor);
            ArtifactContract.parseLaunch(Files.readString(launch));
        }
        return new Prepared(launch, app, dependencies,
                inputs.module() == null || inputs.module().isBlank() ? "/app/lib" : "/app/mods");
    }

    static List<String> command(String executable, String image, Path store,
                                List<String> platforms, List<String> layerSets, Prepared prepared) {
        if (platforms.isEmpty()) {
            throw new IllegalArgumentException("Explicit brewlet.platforms are required");
        }
        List<String> command = new ArrayList<>(List.of(executable, "build", image));
        if (store != null) {
            command.addAll(List.of("--store", store.toString()));
        }
        for (String platform : platforms) {
            command.addAll(List.of("--platform", platform));
        }
        command.addAll(List.of("--launch", prepared.launch().toString()));
        for (String layerSet : layerSets) {
            command.addAll(List.of("--layer-set", layerSet));
        }
        if (prepared.dependencies() != null) {
            command.addAll(List.of("--layer",
                    "dependency=" + prepared.dependencies() + ":" + prepared.dependencyTarget()));
        }
        command.addAll(List.of("--layer", "app=" + prepared.application() + ":/app"));
        return command;
    }

    /** Publishes the layout written by {@link #command} unchanged. */
    static List<String> pushCommand(String executable, String image, Path store) {
        return new ArrayList<>(List.of(executable, "push", image, "--store", store.toString()));
    }

    static void requireThinJar(Path path) throws IOException {
        requireFile(path);
        try (JarFile jar = new JarFile(path.toFile())) {
            var entries = jar.entries();
            while (entries.hasMoreElements()) {
                var entry = entries.nextElement();
                if (!entry.isDirectory() && entry.getName().toLowerCase(java.util.Locale.ROOT).endsWith(".jar")) {
                    throw new IOException("Managed layer sets require a thin application JAR; nested JAR: "
                            + entry.getName());
                }
            }
        }
    }

    private static void requireFile(Path file) throws IOException {
        if (!Files.isRegularFile(file, LinkOption.NOFOLLOW_LINKS)) {
            throw new IOException("Expected a regular, non-symlink producer input: " + file);
        }
    }

    private JavaProducer() {}
}
