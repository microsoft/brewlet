package sh.brewlet.maven;

import static org.junit.jupiter.api.Assertions.*;

import java.io.File;
import java.lang.reflect.Field;
import java.lang.reflect.Proxy;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Map;
import java.util.jar.JarOutputStream;
import org.apache.maven.artifact.Artifact;
import org.apache.maven.artifact.DefaultArtifact;
import org.apache.maven.artifact.handler.DefaultArtifactHandler;
import org.apache.maven.model.Build;
import org.apache.maven.model.Dependency;
import org.apache.maven.model.DependencyManagement;
import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.project.DefaultProjectBuildingRequest;
import org.apache.maven.project.MavenProject;
import org.apache.maven.project.ProjectBuilder;
import org.apache.maven.project.ProjectBuildingResult;
import org.apache.maven.repository.RepositorySystem;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

class MojoContractTest {
    @TempDir Path temp;
    private static final String DIGEST = "sha256:" + "a".repeat(64);

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
        mojo.sourceBom = "example:bom:1";
        Path inspected = temp.resolve("inspection.json");
        var graph = new MavenGraph(mojo.sourceBom, MavenGraph.runtimeMembers(project.getArtifacts()), "/app/lib");
        Files.write(inspected, MavenGraph.JSON.writeValueAsBytes(Map.of("digest", DIGEST,
                "evidence", Map.of("manifestDigest", DIGEST),
                "layerSet", Map.of("schemaVersion", 1, "ecosystem", "java"),
                "lock", Map.of("schemaVersion", 1, "entries", graph.neutralEntries()))));
        Path cli = temp.resolve("fake brewlet");
        Files.writeString(cli, "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + quote(temp.resolve("argv"))
                + "\nif [ \"$1\" = inspect ]; then cat " + quote(inspected) + "; fi\n");
        assertTrue(cli.toFile().setExecutable(true));
        mojo.executable = cli.toString();

        Dependency approved = new Dependency();
        approved.setGroupId("example");
        approved.setArtifactId("library");
        approved.setVersion("1");
        DependencyManagement management = new DependencyManagement();
        management.addDependency(approved);
        MavenProject bom = new MavenProject();
        bom.setPackaging("pom");
        bom.getModel().setDependencyManagement(management);
        ProjectBuildingResult result = (ProjectBuildingResult) Proxy.newProxyInstance(
                getClass().getClassLoader(), new Class<?>[] {ProjectBuildingResult.class},
                (proxy, method, args) -> method.getName().equals("getProject") ? bom : null);
        ProjectBuilder builder = (ProjectBuilder) Proxy.newProxyInstance(
                getClass().getClassLoader(), new Class<?>[] {ProjectBuilder.class},
                (proxy, method, args) -> {
                    assertEquals("build", method.getName());
                    Artifact requested = (Artifact) args[0];
                    assertEquals("example", requested.getGroupId());
                    assertEquals("bom", requested.getArtifactId());
                    assertEquals("1", requested.getVersion());
                    return result;
                });
        RepositorySystem repository = (RepositorySystem) Proxy.newProxyInstance(
                getClass().getClassLoader(), new Class<?>[] {RepositorySystem.class},
                (proxy, method, args) -> new DefaultArtifact((String) args[0], (String) args[1],
                        (String) args[2], null, "pom", null, new DefaultArtifactHandler("pom")));
        set(mojo, "projectBuilder", builder);
        set(mojo, "repositorySystem", repository);
    }

    private static String quote(Path path) {
        return "'" + path.toString().replace("'", "'\\''") + "'";
    }

    @Test void conformanceDelegatesAllFixturesAndPreservesNormalizedResults() throws Exception {
        ConformanceMojo mojo = new ConformanceMojo();
        initialize(mojo, project());
        Path fixtures = temp.resolve("shared fixtures.json");
        Files.writeString(fixtures, "{\"schemaVersion\":1,\"cases\":[]}");
        Path result = temp.resolve("results/conformance.json");
        Path cli = temp.resolve("conformance cli");
        String normalized = "[{\"name\":\"unsafe-layer\",\"reason\":\"LayerPathViolation\",\"passed\":true}]";
        Files.writeString(cli, "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + quote(temp.resolve("argv"))
                + "\nprintf '%s' '" + normalized + "'\n");
        assertTrue(cli.toFile().setExecutable(true));
        mojo.executable = cli.toString();
        set(mojo, "fixtures", fixtures.toFile());
        set(mojo, "result", result.toFile());
        mojo.execute();
        assertEquals(List.of("conformance", fixtures.toString()), Files.readAllLines(temp.resolve("argv")));
        assertEquals(normalized, Files.readString(result));
        Files.writeString(cli, "#!/bin/sh\nexit 9\n");
        assertThrows(MojoExecutionException.class, mojo::execute);
        assertEquals(normalized, Files.readString(result), "failed validation must not replace prior evidence");
    }

    @Test void conformanceRequiresExplicitExistingFixturesAndResult() {
        ConformanceMojo mojo = new ConformanceMojo();
        assertThrows(MojoExecutionException.class, mojo::execute);
    }

    @Test void pushInspectsThenPinsWithoutBundlingAndForwardsTrustButNotSigning() throws Exception {
        PushMojo mojo = new PushMojo();
        initialize(mojo, project());
        set(mojo, "image", "registry.test/app:v2");
        set(mojo, "platforms", List.of("linux/arm64"));
        set(mojo, "mainClass", "example.Main");
        set(mojo, "layerSets", List.of("registry.test/dependencies:approved"));
        set(mojo, "trustKeys", List.of("/trust key one.pem", "/trust key two.pem"));
        set(mojo, "signerIdentity", "trusted builder");
        set(mojo, "requireProvenance", true);
        mojo.signingKey = new File("/signing key.pem");
        mojo.builderIdentity = "application builder";
        mojo.execute();
        List<String> argv = Files.readAllLines(temp.resolve("argv"));
        assertEquals("inspect", argv.get(0));
        assertEquals("registry.test/dependencies:approved", argv.get(1));
        assertTrue(argv.contains("registry.test/dependencies@" + DIGEST));
        assertEquals(4, argv.stream().filter("--trust-key"::equals).count());
        assertEquals(2, argv.stream().filter("--require-provenance"::equals).count());
        assertTrue(argv.contains("/trust key one.pem"));
        assertTrue(argv.contains("trusted builder"));
        assertFalse(argv.contains("--signing-key"));
        assertFalse(argv.contains("--builder-identity"));
        assertEquals(1, argv.stream().filter("--layer"::equals).count());
        assertFalse(argv.stream().anyMatch(arg -> arg.startsWith("dependency=")));
    }

    @Test void standaloneLayerSetDoesNotRequireApplicationAndDelegatesNeutralFiles() throws Exception {
        LayerSetMojo mojo = new LayerSetMojo();
        MavenProject project = project();
        initialize(mojo, project);
        project.setArtifact(null);
        set(mojo, "layerSet", "registry.test/dependencies:approved");
        set(mojo, "name", "approved");
        set(mojo, "version", "2026.10");
        mojo.signingKey = new File("/signing key.pem");
        mojo.builderIdentity = "dependency builder";
        mojo.execute();
        List<String> argv = Files.readAllLines(temp.resolve("argv"));
        assertEquals("layer-set", argv.get(0));
        assertFalse(argv.contains("push"));
        assertTrue(argv.contains("/signing key.pem"));
        assertTrue(argv.contains("dependency builder"));
        Path config = Path.of(argv.get(argv.indexOf("--config") + 1));
        Path lock = Path.of(argv.get(argv.indexOf("--lock") + 1));
        assertEquals("java", MavenGraph.JSON.readTree(config.toFile()).path("ecosystem").asText());
        assertEquals(2, MavenGraph.JSON.readTree(lock.toFile()).path("entries").size());
        String layer = argv.get(argv.indexOf("--layer") + 1);
        Path dependencies = Path.of(layer.substring("dependency=".length(), layer.length() - ":/app".length()));
        assertTrue(Files.exists(dependencies.resolve("lib/library.jar")));
        assertTrue(Files.exists(dependencies.resolve(".brewlet/maven-lock.json")));
        assertFalse(Files.exists(dependencies.resolve("app.jar")));
    }

    @Test void graphMismatchAndMultipleSetsFailBeforePush() throws Exception {
        PushMojo mojo = new PushMojo();
        initialize(mojo, project());
        set(mojo, "image", "app");
        set(mojo, "platforms", List.of("linux/arm64"));
        set(mojo, "mainClass", "example.Main");
        set(mojo, "layerSets", List.of("repo:approved"));
        Files.writeString(temp.resolve("library.jar"), "substituted dependency");
        assertThrows(MojoExecutionException.class, mojo::execute);
        assertFalse(Files.readAllLines(temp.resolve("argv")).contains("push"));
        Files.delete(temp.resolve("argv"));
        set(mojo, "layerSets", List.of("repo:first", "repo:second"));
        assertThrows(MojoExecutionException.class, mojo::execute);
        assertFalse(Files.exists(temp.resolve("argv")));
    }

    @Test void localLayoutIsPinnedByManifestNotMutableIndex() throws Exception {
        Path layout = Files.createDirectory(temp.resolve("layout with spaces:tag"));
        assertEquals(layout + "@" + DIGEST, LayerSets.pin(layout.toString(), DIGEST));
        assertEquals(layout + "@" + DIGEST, LayerSets.pin(layout + "@sha256:" + "b".repeat(64), DIGEST));
        assertEquals("registry.test:5000/repo@" + DIGEST,
                LayerSets.pin("registry.test:5000/repo:tag", DIGEST));
    }
}
