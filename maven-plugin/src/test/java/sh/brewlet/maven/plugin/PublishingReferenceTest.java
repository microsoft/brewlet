// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import com.sun.net.httpserver.HttpServer;
import org.apache.maven.artifact.DefaultArtifact;
import org.apache.maven.artifact.handler.DefaultArtifactHandler;
import org.apache.maven.plugin.MojoExecutionException;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.ValueSource;
import sh.brewlet.maven.plugin.oci.DependencyBundle;
import sh.brewlet.maven.plugin.oci.LocalStore;
import sh.brewlet.maven.plugin.oci.MediaTypes;
import sh.brewlet.maven.plugin.oci.RegistryClient;

import java.net.InetSocketAddress;
import java.net.URLDecoder;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.CopyOnWriteArrayList;

import static org.junit.jupiter.api.Assertions.*;

class PublishingReferenceTest {
    @TempDir
    Path root;

    @ParameterizedTest
    @ValueSource(strings = {"push", "dependency-bundle"})
    void rejectsDigestDestinationsBeforeBuildingOrPublishing(String goal) throws Exception {
        for (String format : List.of("image", "artifact")) {
            for (boolean dryRun : List.of(false, true)) {
                for (String suffix : List.of("@sha256:" + "a".repeat(64),
                        ":old@sha256:" + "b".repeat(64), "@sha512:" + "c".repeat(128),
                        "@sha256:invalid")) {
                    for (boolean bundleImage : List.of(false, true)) {
                        AbstractBrewletMojo mojo = mojo(goal);
                        mojo.format = format;
                        mojo.dryRun = dryRun;
                        String ref = "localhost:5000/team/app" + suffix;
                        mojo.image = ref;
                        if (goal.equals("dependency-bundle") && bundleImage) {
                            mojo.image = "localhost:5000/team/valid:1";
                            TestApplications.set(mojo, "dependencyBundleImage", ref);
                        }
                        MojoExecutionException error =
                                assertThrows(MojoExecutionException.class, mojo::execute);
                        assertEquals("Image \"" + ref + "\" is digest-pinned; "
                                + "push needs a tag (e.g. localhost:5000/team/app:1.0.0) "
                                + "and prints the pinned digest afterwards.", error.getMessage());
                        assertFalse(Files.exists(root.resolve("output")));
                        assertFalse(Files.exists(root.resolve("bundle")));
                    }
                }
            }
        }
    }

    @ParameterizedTest
    @ValueSource(strings = {"push", "dependency-bundle"})
    void acceptsTaggedAndImplicitLatestDestinations(String goal) throws Exception {
        for (String ref : List.of("registry.example.com/team/app:1.0.0",
                "registry.example.com/team/app", "localhost:5000/team/app:1.0.0-SNAPSHOT",
                "localhost:5000/team/app", "docker.io/me/app:1", "localhost/app:1")) {
            AbstractBrewletMojo mojo = mojo(goal);
            mojo.jarFile = applicationJar().toFile();
            mojo.image = ref;
            mojo.dryRun = true;
            assertDoesNotThrow(mojo::execute);
            assertEquals(ref, mojo.image);
            assertFalse(Files.exists(root.resolve("output/push.json")));
            if (goal.equals("dependency-bundle")) {
                assertTrue(Files.isRegularFile(root.resolve("bundle/index.json")));
            }
        }
    }

    @Test
    void preservesRegistryDerivedImageAndExplicitHostRequirement() throws Exception {
        AbstractBrewletMojo mojo = mojo("push");
        mojo.jarFile = applicationJar().toFile();
        mojo.dryRun = true;
        mojo.image = null;
        mojo.registry = "localhost:5000/team";
        mojo.execute();
        assertEquals("localhost:5000/team/app:1", mojo.image);

        mojo.image = "team/app:1";
        assertTrue(assertThrows(MojoExecutionException.class, mojo::execute)
                .getMessage().contains("has no registry host"));
    }

    @Test
    void bundleDestinationOverrideKeepsExistingDefaults() throws Exception {
        AbstractBrewletMojo mojo = mojo("dependency-bundle");
        mojo.image = "registry.example.com/unused@sha256:" + "a".repeat(64);
        TestApplications.set(mojo, "dependencyBundleImage", "team/bundle");
        mojo.dryRun = true;
        mojo.execute();
        var index = AbstractBrewletMojo.MAPPER.readTree(root.resolve("bundle/index.json").toFile());
        assertEquals("team/bundle", index.path("manifests").get(0).path("annotations")
                .path(MediaTypes.ANNOTATION_REF_NAME).asText());
        assertEquals("latest", RegistryClient.extractTag("team/bundle"));
    }

    @Test
    void applicationPushConsumesPinnedBundleSources() throws Exception {
        AbstractBrewletMojo publisher = mojo("dependency-bundle");
        publisher.dryRun = true;
        publisher.execute();
        LocalStore store = new LocalStore(root.resolve("bundle"));
        String digest = DependencyBundle.loadLayout(root.resolve("bundle")).manifestDigest();
        List<String> requests = new CopyOnWriteArrayList<>();
        HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/v2/team/bundle/", exchange -> {
            try (exchange) {
                String path = exchange.getRequestURI().getPath();
                requests.add(exchange.getRequestMethod() + " " + path);
                byte[] body;
                if (!exchange.getRequestMethod().equals("GET")) {
                    exchange.sendResponseHeaders(405, -1);
                    return;
                }
                if (path.equals("/v2/team/bundle/referrers/" + digest)) {
                    String type = URLDecoder.decode(
                            exchange.getRequestURI().getRawQuery().substring("artifactType=".length()),
                            StandardCharsets.UTF_8);
                    body = AbstractBrewletMojo.MAPPER.writeValueAsBytes(Map.of(
                            "schemaVersion", 2, "mediaType", MediaTypes.OCI_INDEX_MEDIA_TYPE,
                            "manifests", store.referrers(digest, type)));
                } else if (path.equals("/v2/team/bundle/tags/list")) {
                    body = AbstractBrewletMojo.MAPPER.writeValueAsBytes(
                            Map.of("name", "team/bundle", "tags", List.of()));
                } else {
                    String requestedDigest = path.substring(path.lastIndexOf('/') + 1);
                    if (!requestedDigest.matches("sha256:[0-9a-f]{64}")) {
                        exchange.sendResponseHeaders(404, -1);
                        return;
                    }
                    body = Files.readAllBytes(store.blobPath(requestedDigest));
                }
                exchange.sendResponseHeaders(200, body.length);
                exchange.getResponseBody().write(body);
            }
        });
        server.start();
        try {
            String repository = "127.0.0.1:" + server.getAddress().getPort() + "/team/bundle";
            for (String source : List.of(repository + "@" + digest,
                    repository + ":approved@" + digest, root.resolve("bundle").toString())) {
                AbstractBrewletMojo push = mojo("push");
                push.jarFile = applicationJar().toFile();
                push.dependencyBundle = source;
                push.mainClass = "app.Main";
                push.dryRun = true;
                assertDoesNotThrow(push::execute, source);
            }
            assertEquals(2, requests.stream()
                    .filter(r -> r.equals("GET /v2/team/bundle/manifests/" + digest)).count());
            assertTrue(requests.stream().allMatch(r -> r.startsWith("GET ")));
        } finally {
            server.stop(0);
        }
    }

    private AbstractBrewletMojo mojo(String goal) throws Exception {
        AbstractBrewletMojo mojo = switch (goal) {
            case "push" -> new PushMojo();
            case "dependency-bundle" -> new DependencyBundleMojo();
            default -> throw new IllegalArgumentException(goal);
        };
        TestApplications.configure(mojo, root.resolve("missing.jar"), root.resolve("output"), false);
        mojo.format = "image";
        Path library = root.resolve("library.jar");
        if (!Files.exists(library)) {
            Files.write(library, TestApplications.zip(Map.of("resource.txt", new byte[]{1})));
        }
        DefaultArtifactHandler handler = new DefaultArtifactHandler("jar");
        handler.setAddedToClasspath(true);
        DefaultArtifact dependency = new DefaultArtifact(
                "test", "library", "1", "runtime", "jar", null, handler);
        dependency.setFile(library.toFile());
        mojo.project.setArtifacts(Set.of(dependency));
        if (mojo instanceof DependencyBundleMojo) {
            TestApplications.set(mojo, "sourceBom", "test:platform:1");
            TestApplications.set(mojo, "dependencyBundleOutputDirectory", root.resolve("bundle").toFile());
        }
        return mojo;
    }

    private Path applicationJar() throws Exception {
        Path jar = root.resolve("app.jar");
        Files.write(jar, TestApplications.zip(Map.of("META-INF/MANIFEST.MF",
                "Manifest-Version: 1.0\nMain-Class: app.Main\n\n".getBytes(StandardCharsets.UTF_8))));
        return jar;
    }
}
