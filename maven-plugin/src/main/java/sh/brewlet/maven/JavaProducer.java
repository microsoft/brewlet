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
                  String module, List<String> javaArgs, Path appCdsArchive) {
        Inputs(Path jar, List<Path> dependencies, Path launch, String mainClass,
               String module, List<String> javaArgs) {
            this(jar, dependencies, launch, mainClass, module, javaArgs, null);
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
            descriptor.put("schemaVersion", 1);
            descriptor.put("process", Map.of("command", List.of("java"),
                    "args", args, "workingDir", "/app"));
            launch = output.resolve("launch.json");
            new ObjectMapper().enable(SerializationFeature.ORDER_MAP_ENTRIES_BY_KEYS)
                    .writerWithDefaultPrettyPrinter().writeValue(launch.toFile(), descriptor);
            ArtifactContract.parseLaunch(Files.readString(launch));
        }
        return new Prepared(launch, app, dependencies,
                inputs.module() == null || inputs.module().isBlank() ? "/app/lib" : "/app/mods");
    }

    static List<String> command(String executable, String image, Path store,
                                List<String> platforms, Prepared prepared) {
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
        if (prepared.dependencies() != null) {
            command.addAll(List.of("--layer",
                    prepared.dependencies() + ":" + prepared.dependencyTarget()));
        }
        command.addAll(List.of("--layer", prepared.application() + ":/app"));
        return command;
    }

    /**
     * Publishes the image {@link #command} wrote under {@code image}, unchanged. {@code --source}
     * stops the CLI from falling back to another reference's build left in the shared layout.
     */
    static List<String> pushCommand(String executable, String image, Path store) {
        return new ArrayList<>(List.of(executable, "push", image, "--store", store.toString(),
                "--source", image));
    }

    private static void requireFile(Path file) throws IOException {
        if (!Files.isRegularFile(file, LinkOption.NOFOLLOW_LINKS)) {
            throw new IOException("Expected a regular, non-symlink producer input: " + file);
        }
    }

    private JavaProducer() {}
}
