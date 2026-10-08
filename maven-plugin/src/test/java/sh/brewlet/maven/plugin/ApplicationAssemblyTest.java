// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import org.apache.maven.artifact.DefaultArtifact;
import org.apache.maven.artifact.handler.DefaultArtifactHandler;
import org.apache.maven.plugin.MojoExecutionException;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.CsvSource;
import org.junit.jupiter.params.provider.ValueSource;
import sh.brewlet.maven.plugin.oci.LocalStore;

import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.Map;
import java.util.Set;

import static org.junit.jupiter.api.Assertions.*;

class ApplicationAssemblyTest {
    @TempDir Path root;

    @ParameterizedTest
    @CsvSource({"image,false", "image,true", "artifact,false", "artifact,true"})
    void bothOrdersReuseOneAssemblyAndPublishExactlyTheLocalBytes(String format, boolean pushFirst) throws Exception {
        try (TestImageRegistry registry = new TestImageRegistry()) {
            BuildMojo build = build(registry, format);
            PushMojo push = push(build);
            (pushFirst ? push : build).execute();
            ApplicationAssembly first = ApplicationAssembly.get(build.project);
            assertNotNull(first);
            (pushFirst ? build : push).execute();
            assertSame(first, ApplicationAssembly.get(build.project), "Must reuse the actual assembly");
            assertTrue(first.published, "A local write must not discard publication state");
            String digest = first.result.digest();
            assertEquals(digest, LocalStore.sha256Hex(registry.tagged));
            Path layout = root.resolve("oci");
            assertEquals(digest, AbstractBrewletMojo.MAPPER.readTree(layout.resolve("index.json").toFile())
                    .path("manifests").get(0).path("digest").asText());
            for (var entry : registry.content.entrySet()) {
                assertArrayEquals(entry.getValue(),
                        Files.readAllBytes(new LocalStore(layout).blobPath(entry.getKey())));
            }
            Path handoff = root.resolve("out/push.json");
            byte[] savedPush = Files.readAllBytes(handoff);
            var result = AbstractBrewletMojo.MAPPER.readValue(savedPush, AbstractPushMojo.PushResult.class);
            assertEquals(digest, result.digest());
            assertEquals(first.result.reference(), result.image());
            if ("image".equals(format)) {
                ManifestMojo manifest = manifest(build);
                manifest.execute();
                assertEquals(result.deployImage(), manifest.resolveDeployImage());
                assertTrue(Files.readString(root.resolve("out/javaapplication.yaml")).contains(result.deployImage()));
            }
            int requests = registry.requests.get();
            build.execute();
            assertEquals(requests, registry.requests.get(), "Local rewrite must not contact the registry");
            assertSame(first, ApplicationAssembly.get(build.project));
            assertArrayEquals(savedPush, Files.readAllBytes(handoff));
            assertEquals(1, registry.taggedWrites.get());
        }
    }

    @ParameterizedTest
    @ValueSource(strings = {"jar", "dependency", "config", "cds", "aot", "bothCds", "bothAot", "format", "reference"})
    void changedInputsCannotSilentlyReplaceTheAssembly(String change) throws Exception {
        try (TestImageRegistry registry = new TestImageRegistry()) {
            BuildMojo build = build(registry, "image");
            build.layered = true;
            Path dependency = root.resolve("dependency.jar");
            Files.write(dependency, TestApplications.zip(Map.of("value", new byte[]{1})));
            DefaultArtifactHandler handler = new DefaultArtifactHandler("jar");
            handler.setAddedToClasspath(true);
            DefaultArtifact artifact = new DefaultArtifact("test", "dependency", "1",
                    "runtime", "jar", null, handler);
            artifact.setFile(dependency.toFile());
            build.project.setArtifacts(Set.of(artifact));
            boolean aotRun = "aot".equals(change) || "bothAot".equals(change);
            boolean both = change.startsWith("both");
            Path cds = root.resolve(aotRun ? "app.aot" : "app.jsa");
            Files.write(cds, new byte[]{1});
            // In the "both" runs the other archive ships too but stays unchanged.
            Path other = root.resolve(aotRun ? "app.jsa" : "app.aot");
            Files.write(other, new byte[]{1});
            if (aotRun) build.aotCache = cds.toFile(); else build.cdsArchive = cds.toFile();
            if (both) { if (aotRun) build.cdsArchive = other.toFile(); else build.aotCache = other.toFile(); }
            build.execute();
            ApplicationAssembly first = ApplicationAssembly.get(build.project);
            byte[] localIndex = Files.readAllBytes(root.resolve("oci/index.json"));
            PushMojo push = push(build);
            push.layered = true;
            if (aotRun) push.aotCache = cds.toFile(); else push.cdsArchive = cds.toFile();
            if (both) { if (aotRun) push.cdsArchive = other.toFile(); else push.aotCache = other.toFile(); }
            switch (change) {
                case "jar" -> Files.write(build.jarFile.toPath(), jar("new"));
                case "dependency" -> Files.write(dependency, TestApplications.zip(Map.of("value", new byte[]{2})));
                case "config" -> push.enablePreview = true;
                case "cds", "aot", "bothCds", "bothAot" -> Files.write(cds, new byte[]{2});
                case "format" -> push.format = "artifact";
                case "reference" -> push.image = registry.authority() + "/different:1";
                default -> fail("Unhandled change");
            }
            MojoExecutionException error = assertThrows(MojoExecutionException.class, push::execute);
            assertTrue(error.getMessage().contains("Conflicting build/push inputs"), error.getMessage());
            assertEquals(0, registry.requests.get());
            assertSame(first, ApplicationAssembly.get(build.project));
            assertNull(ApplicationBuildResult.get(build.project));
            assertThrows(MojoExecutionException.class, () -> manifest(build).execute());
            assertFalse(Files.exists(root.resolve("out/push.json")));
            assertArrayEquals(localIndex, Files.readAllBytes(root.resolve("oci/index.json")));
        }
    }

    @Test
    void changedLocalBuildAfterPushFailsWithoutLosingPublishedEvidence() throws Exception {
        try (TestImageRegistry registry = new TestImageRegistry()) {
            BuildMojo build = build(registry, "image");
            push(build).execute();
            ApplicationAssembly first = ApplicationAssembly.get(build.project);
            byte[] handoff = Files.readAllBytes(root.resolve("out/push.json"));
            build.enablePreview = true;
            assertTrue(assertThrows(MojoExecutionException.class, build::execute)
                    .getMessage().contains("Conflicting build/push inputs"));
            assertTrue(first.published);
            assertSame(first, ApplicationAssembly.get(build.project));
            assertNull(ApplicationBuildResult.get(build.project));
            assertFalse(Files.exists(root.resolve("oci/index.json")));
            assertArrayEquals(handoff, Files.readAllBytes(root.resolve("out/push.json")));
        }
    }

    @Test
    void failedAndDryRunPushNeverMarkLocalAssemblyPublishedAndRetryReusesIt() throws Exception {
        try (TestImageRegistry registry = new TestImageRegistry()) {
            BuildMojo build = build(registry, "image");
            build.execute();
            ApplicationAssembly first = ApplicationAssembly.get(build.project);
            PushMojo push = push(build);
            push.dryRun = true;
            push.execute();
            assertSame(first, ApplicationAssembly.get(build.project));
            assertFalse(first.published);
            assertNull(ApplicationBuildResult.get(build.project));
            assertEquals(0, registry.requests.get());
            push.dryRun = false;
            registry.fail = true;
            assertThrows(MojoExecutionException.class, push::execute);
            assertSame(first, ApplicationAssembly.get(build.project));
            assertFalse(first.published);
            assertNull(ApplicationBuildResult.get(build.project));
            assertFalse(Files.exists(root.resolve("out/push.json")));
            registry.fail = false;
            push.execute();
            assertSame(first, ApplicationAssembly.get(build.project));
            assertTrue(first.published);
            assertEquals(first.result.digest(), LocalStore.sha256Hex(registry.tagged));
        }
    }

    @Test
    void anotherProjectDoesNotReuseThePreviousAssemblyOrPublicationState() throws Exception {
        try (TestImageRegistry registry = new TestImageRegistry()) {
            BuildMojo first = build(registry, "image");
            push(first).execute();
            BuildMojo second = build(registry, "image");
            second.execute();
            assertNotSame(ApplicationAssembly.get(first.project), ApplicationAssembly.get(second.project));
            assertFalse(ApplicationAssembly.get(second.project).published);
        }
    }

    @Test
    void localDryRunBuildWritesLayoutButManifestOnlyPreviews() throws Exception {
        try (TestImageRegistry registry = new TestImageRegistry()) {
            BuildMojo build = build(registry, "image");
            build.dryRun = true;
            java.util.List<String> logs = new java.util.ArrayList<>();
            var log = new org.apache.maven.plugin.logging.SystemStreamLog() {
                @Override public void info(CharSequence message) { logs.add(message.toString()); }
            };
            build.setLog(log);
            build.execute();
            assertTrue(Files.isRegularFile(root.resolve("oci/index.json")));
            assertFalse(ApplicationAssembly.get(build.project).published);
            Path yaml = root.resolve("out/javaapplication.yaml");
            Files.createDirectories(yaml.getParent());
            Files.writeString(yaml, "previous manifest");
            ManifestMojo manifest = manifest(build);
            manifest.dryRun = true;
            manifest.setLog(log);
            manifest.execute();
            assertEquals("previous manifest", Files.readString(yaml));
            assertEquals(0, registry.requests.get());
            assertFalse(Files.exists(root.resolve("out/push.json")));
            assertTrue(logs.stream().anyMatch(message -> message.contains("build writes the local OCI layout")));
            assertTrue(logs.stream().anyMatch(message -> message.contains("dry-run development manifest")
                    && message.contains(ApplicationBuildResult.get(build.project).digest())));
        }
    }

    @Test
    void nativeArtifactShipsCdsAndAotAsTwoLayers() throws Exception {
        try (TestImageRegistry registry = new TestImageRegistry()) {
            BuildMojo build = build(registry, "artifact");
            Path jsa = root.resolve("app.jsa");
            Files.write(jsa, new byte[]{1});
            Path aot = root.resolve("app.aot");
            Files.write(aot, new byte[]{2});
            build.cdsArchive = jsa.toFile();
            build.aotCache = aot.toFile();
            build.execute();
            var manifest = AbstractBrewletMojo.MAPPER.readTree(
                    ApplicationAssembly.get(build.project).image.root().data());
            var layers = manifest.path("layers");
            var byType = new java.util.HashMap<String, String>();
            for (var layer : layers) {
                byType.put(layer.path("mediaType").asText(),
                        layer.path("annotations").path("org.opencontainers.image.title").asText());
            }
            assertEquals("app.jsa", byType.get(sh.brewlet.maven.plugin.oci.MediaTypes.CDS_LAYER_MEDIA_TYPE), layers.toString());
            assertEquals("app.aot", byType.get(sh.brewlet.maven.plugin.oci.MediaTypes.AOT_LAYER_MEDIA_TYPE), layers.toString());
        }
    }

    private BuildMojo build(TestImageRegistry registry, String format) throws Exception {
        Path jar = root.resolve("app.jar");
        Files.write(jar, jar("original"));
        BuildMojo build = new BuildMojo();
        TestApplications.configure(build, jar, root.resolve("out"), false);
        build.format = format;
        build.image = registry.image();
        TestApplications.set(build, "ociOutputDirectory", root.resolve("oci").toFile());
        return build;
    }

    private PushMojo push(BuildMojo build) {
        PushMojo push = new PushMojo();
        TestApplications.configure(push, build.jarFile.toPath(), build.outputDirectory.toPath(), build.layered);
        push.project = build.project;
        push.image = build.image;
        push.format = build.format;
        return push;
    }

    private ManifestMojo manifest(BuildMojo build) {
        ManifestMojo manifest = new ManifestMojo();
        manifest.project = build.project;
        manifest.outputDirectory = build.outputDirectory;
        manifest.jdkFeature = 17;
        manifest.appName = "app";
        manifest.namespace = "default";
        return manifest;
    }

    private byte[] jar(String content) throws Exception {
        return TestApplications.zip(Map.of("META-INF/MANIFEST.MF",
                "Manifest-Version: 1.0\nMain-Class: App\n\n".getBytes(StandardCharsets.UTF_8),
                "content", content.getBytes(StandardCharsets.UTF_8)));
    }
}
