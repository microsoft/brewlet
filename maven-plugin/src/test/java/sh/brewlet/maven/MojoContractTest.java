package sh.brewlet.maven;

import static org.junit.jupiter.api.Assertions.*;

import com.fasterxml.jackson.databind.ObjectMapper;
import java.lang.reflect.Field;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.jar.JarOutputStream;
import org.apache.maven.artifact.Artifact;
import org.apache.maven.artifact.DefaultArtifact;
import org.apache.maven.artifact.handler.DefaultArtifactHandler;
import org.apache.maven.model.Build;
import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.project.DefaultProjectBuildingRequest;
import org.apache.maven.project.MavenProject;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

class MojoContractTest {
    @TempDir Path temp;

    private static void set(Object target, String name, Object value) throws Exception {
        Class<?> type = target.getClass();
        while (type != null) {
            try {
                Field field = type.getDeclaredField(name);
                field.setAccessible(true);
                field.set(target, value);
                return;
            } catch (NoSuchFieldException ignored) {
                type = type.getSuperclass();
            }
        }
        throw new NoSuchFieldException(name);
    }

    private Artifact artifact(String name, String scope, Path file) {
        Artifact artifact = new DefaultArtifact("example", name, "1", scope,
                "jar", null, new DefaultArtifactHandler("jar"));
        artifact.setFile(file.toFile());
        return artifact;
    }

    private MavenProject project() throws Exception {
        MavenProject project = new MavenProject();
        project.setFile(temp.resolve("pom.xml").toFile());
        project.setProjectBuildingRequest(new DefaultProjectBuildingRequest());
        Build build = new Build();
        build.setDirectory(temp.resolve("target").toString());
        project.setBuild(build);
        Path dependency = temp.resolve("library.jar");
        Files.writeString(dependency, "dependency");
        project.setArtifacts(new LinkedHashSet<>(List.of(artifact("library", "runtime", dependency))));
        Path jar = temp.resolve("application.jar");
        try (JarOutputStream ignored = new JarOutputStream(Files.newOutputStream(jar))) {}
        project.setArtifact(artifact("application", "compile", jar));
        return project;
    }

    private void initialize(AbstractJavaMojo mojo, MavenProject project) throws Exception {
        mojo.project = project;
        Path cli = temp.resolve("fake brewlet");
        Files.writeString(cli, "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + quote(temp.resolve("argv")) + "\n");
        assertTrue(cli.toFile().setExecutable(true));
        mojo.executable = cli.toString();
    }

    private static String quote(Path path) {
        return "'" + path.toString().replace("'", "'\\''") + "'";
    }

    @Test void conformanceEvaluatesFixturesInJavaWithoutCliAndPreservesNormalizedResults() throws Exception {
        ConformanceMojo mojo = new ConformanceMojo();
        Path fixtures = temp.resolve("shared fixtures.json");
        Files.writeString(fixtures, "{\"schemaVersion\":1,\"cases\":[{\"name\":\"unsafe-layer\",\"kind\":\"layer\","
                + "\"entries\":[{\"path\":\"etc/passwd\",\"type\":\"file\",\"mode\":420}],"
                + "\"reason\":\"LayerPathViolation\"}]}");
        Path result = temp.resolve("results/conformance.json");
        set(mojo, "fixtures", fixtures.toFile());
        set(mojo, "result", result.toFile());
        mojo.execute();
        String normalized = "[{\"name\":\"unsafe-layer\",\"reason\":\"LayerPathViolation\",\"passed\":true}]";
        assertEquals(normalized, Files.readString(result).strip());
        Files.writeString(fixtures, Files.readString(fixtures).replace("etc/passwd", "app/passwd"));
        assertThrows(MojoExecutionException.class, mojo::execute);
        assertEquals(normalized, Files.readString(result).strip(), "failed validation must not replace prior evidence");
    }

    @Test void conformanceRequiresExplicitExistingFixturesAndResult() {
        ConformanceMojo mojo = new ConformanceMojo();
        assertThrows(MojoExecutionException.class, mojo::execute);
    }

    @Test void pushPackagesRuntimeLibrariesAndApplicationAsSeparateAppLayers() throws Exception {
        PushMojo mojo = new PushMojo();
        MavenProject project = project();
        Path provided = Files.writeString(temp.resolve("provided.jar"), "provided");
        project.getArtifacts().add(artifact("provided", "provided", provided));
        initialize(mojo, project);
        set(mojo, "image", "registry.test/app:v2");
        set(mojo, "platforms", List.of("linux/arm64"));
        set(mojo, "mainClass", "example.Main");
        mojo.execute();
        List<String> argv = Files.readAllLines(temp.resolve("argv"));
        assertEquals("build", argv.get(0));
        int push = argv.lastIndexOf("push");
        assertTrue(push > 0, "build without a store must be followed by push");
        assertEquals(List.of("push", "registry.test/app:v2", "--store",
                argv.get(argv.indexOf("--store") + 1)), argv.subList(push, argv.size()));
        assertEquals(2, argv.stream().filter("--layer"::equals).count());
        String libraryLayer = argv.get(argv.indexOf("--layer") + 1);
        assertTrue(libraryLayer.startsWith("app=") && libraryLayer.endsWith(":/app/lib"));
        Path libraries = Path.of(libraryLayer.substring(4, libraryLayer.length() - ":/app/lib".length()));
        assertEquals("dependency", Files.readString(libraries.resolve("library.jar")));
        assertFalse(Files.exists(libraries.resolve("provided.jar")));
        String appLayer = argv.get(argv.lastIndexOf("--layer") + 1);
        assertTrue(appLayer.startsWith("app=") && appLayer.endsWith(":/app"));
        Path application = Path.of(appLayer.substring(4, appLayer.length() - ":/app".length()));
        assertTrue(Files.isRegularFile(application.resolve("app.jar")));
    }

    @Test void pushValidatesPlatformsAndProposesRuntimeImagesBeforeTheCli() throws Exception {
        PushMojo mojo = new PushMojo();
        MavenProject project = project();
        project.getProperties().setProperty("maven.compiler.release", "21");
        initialize(mojo, project);
        set(mojo, "image", "app");
        set(mojo, "platforms", List.of("Linux/amd64"));
        set(mojo, "mainClass", "example.Main");
        set(mojo, "runtimeImageMappings", List.of("25=java-25", "17=java-17", "21=java-21"));
        MojoExecutionException platform = assertThrows(MojoExecutionException.class, mojo::execute);
        assertTrue(platform.getMessage().contains("InvalidPlatform"), platform.getMessage());
        assertFalse(Files.exists(temp.resolve("argv")));
        set(mojo, "platforms", List.of("linux/amd64"));
        mojo.execute();
        List<String> argv = Files.readAllLines(temp.resolve("argv"));
        var launch = new ObjectMapper().readTree(Path.of(argv.get(argv.indexOf("--launch") + 1)).toFile());
        assertEquals("[\"java-21\",\"java-25\"]", launch.path("runtimeImages").toString());
        Files.delete(temp.resolve("argv"));
        set(mojo, "runtimeImages", List.of("Java_21"));
        MojoExecutionException id = assertThrows(MojoExecutionException.class, mojo::execute);
        assertTrue(id.getMessage().contains("InvalidRuntimeImageID"), id.getMessage());
        assertFalse(Files.exists(temp.resolve("argv")));
    }

    @Test void pushRejectsUnresolvedRuntimeDependenciesBeforeCallingCli() throws Exception {
        PushMojo mojo = new PushMojo();
        MavenProject project = project();
        project.getArtifacts().add(artifact("unresolved", "compile", temp.resolve("unused.jar")));
        project.getArtifacts().stream().filter(a -> a.getArtifactId().equals("unresolved"))
                .findFirst().orElseThrow().setFile(null);
        initialize(mojo, project);
        set(mojo, "image", "app");
        set(mojo, "platforms", List.of("linux/amd64"));
        set(mojo, "mainClass", "example.Main");
        MojoExecutionException error = assertThrows(MojoExecutionException.class, mojo::execute);
        assertTrue(error.getMessage().contains("Unresolved runtime dependency"), error.getMessage());
        assertFalse(Files.exists(temp.resolve("argv")));
    }

}
