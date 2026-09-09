// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ObjectNode;
import org.apache.maven.plugin.MojoExecutionException;
import org.junit.jupiter.api.DynamicTest;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.TestFactory;
import org.junit.jupiter.api.io.TempDir;
import sh.brewlet.maven.plugin.model.DependencyBundleConfig;
import sh.brewlet.maven.plugin.model.DependencyLock;
import sh.brewlet.maven.plugin.model.Entry;
import sh.brewlet.maven.plugin.model.JvmConfig;
import sh.brewlet.maven.plugin.model.ManagedDependencyEvidence;
import sh.brewlet.maven.plugin.oci.ArtifactLayer;
import sh.brewlet.maven.plugin.oci.DependencyBundle;
import sh.brewlet.maven.plugin.oci.LocalStore;
import sh.brewlet.maven.plugin.oci.MediaTypes;
import sh.brewlet.maven.plugin.oci.OciDescriptor;
import sh.brewlet.maven.plugin.oci.RunnableImageBuilder;
import sh.brewlet.maven.plugin.util.LayerBuilder;
import sh.brewlet.maven.plugin.util.TarWriter;

import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.Arrays;
import java.util.List;
import java.util.Locale;
import java.util.function.UnaryOperator;
import java.util.stream.Stream;
import java.util.zip.GZIPOutputStream;

import static org.junit.jupiter.api.Assertions.*;

class DependencyBundleTest {
    private static final ObjectMapper MAPPER = new ObjectMapper();

    @TempDir
    Path temp;

    @Test
    void bundleIsDeterministicAndRoundTripsThroughOciLayout() throws IOException {
        Path a = temp.resolve("a.jar");
        Path b = temp.resolve("b.jar");
        Files.writeString(a, "a");
        Files.writeString(b, "b");
        ArtifactLayer firstLayer = LayerBuilder.buildBundle(List.of(
                new LayerBuilder.Dep("b.jar", b, false),
                new LayerBuilder.Dep("a.jar", a, false)));
        ArtifactLayer secondLayer = LayerBuilder.buildBundle(List.of(
                new LayerBuilder.Dep("a.jar", a, false),
                new LayerBuilder.Dep("b.jar", b, false)));

        DependencyLock lock = lock("a", "b");
        DependencyBundleConfig cfg = config();
        DependencyBundle.Content first = DependencyBundle.build(cfg, lock, firstLayer);
        DependencyBundle.Content second = DependencyBundle.build(config(), lock("a", "b"), secondLayer);
        assertEquals(first.manifestDigest(), second.manifestDigest());
        assertArrayEquals(firstLayer.tar(), secondLayer.tar());
        JsonNode manifest = MAPPER.readTree(first.manifestBytes());
        JsonNode bundleLayer = manifest.get("layers").get(0);
        assertEquals(MediaTypes.OCI_LAYER_GZIP_MEDIA_TYPE,
                bundleLayer.get("mediaType").asText());
        assertEquals(MediaTypes.LAYER_ROLE_CLASSPATH,
                bundleLayer.get("annotations").get(
                        MediaTypes.LAYER_ROLE_ANNOTATION).asText());
        assertEquals(first.config().getLayerDigest(), bundleLayer.get("digest").asText());
        assertEquals(LocalStore.sha256Hex(firstLayer.tar()), first.config().getLayerDiffId());
        assertTrue(MAPPER.readTree(first.configBytes()).has("layerDiffId"));
        JsonNode lockJson = MAPPER.readTree(first.lockBytes());
        assertTrue(lockJson.has("artifacts"));
        assertTrue(lockJson.get("artifacts").get(0).has("fileName"));
        assertFalse(lockJson.get("artifacts").get(0).has("classifier"));

        LocalStore store = new LocalStore(temp.resolve("oci"));
        OciDescriptor descriptor = store.pushDependencyBundle("example/deps:1", first);
        assertEquals(MediaTypes.DEPENDENCY_BUNDLE_ARTIFACT_TYPE, descriptor.getArtifactType());

        DependencyBundle.Content loaded = DependencyBundle.loadLayout(temp.resolve("oci"));
        assertEquals(first.manifestDigest(), loaded.manifestDigest());
        assertEquals(lock.getDependencies(), loaded.lock().getDependencies());
        assertEquals(first.config().getLockDigest(), loaded.config().getLockDigest());
        assertArrayEquals(firstLayer.tar(), loaded.uncompressedLayer());
    }

    @Test
    void runnableImageReusesCompressedBundleLayerAndDiffId() throws IOException {
        Path dependency = temp.resolve("a.jar");
        Files.writeString(dependency, "a");
        DependencyBundle.Content bundle = DependencyBundle.build(
                config(), lock("a"), LayerBuilder.buildBundle(List.of(
                        new LayerBuilder.Dep("a.jar", dependency, false))));
        Path jar = temp.resolve("app.jar");
        Files.writeString(jar, "thin");
        JvmConfig jvm = new JvmConfig();
        jvm.setMainJar("app.jar");
        Entry entry = new Entry("classpath", List.of("app.jar", "lib/*"));
        entry.setMainClass("com.acme.Main");
        jvm.setEntry(entry);
        String evidence = DependencyBundle.canonicalJson(new ManagedDependencyEvidence(
                true, "sha256:app", bundle.manifestDigest(),
                bundle.config().getLayerDigest(), bundle.config().getLockDigest(), "g:b:1"));

        RunnableImageBuilder.Result result =
                RunnableImageBuilder.buildWithManagedDependencyLayer(
                        jvm, jar, new RunnableImageBuilder.ManagedDependencyLayer(
                                bundle.compressedLayer(), bundle.config().getLayerDigest(),
                                bundle.config().getLayerDiffId(), "dependencies"), null,
                        java.util.Map.of(MediaTypes.MANAGED_DEPENDENCY_EVIDENCE_ANNOTATION,
                                evidence));
        JsonNode manifest = MAPPER.readTree(result.manifests.get(0).data());
        assertEquals(evidence, manifest.get("annotations")
                .get(MediaTypes.MANAGED_DEPENDENCY_EVIDENCE_ANNOTATION).asText());
        JsonNode reused = manifest.get("layers").get(1);
        assertEquals(bundle.config().getLayerDigest(), reused.get("digest").asText());
        String configDigest = manifest.get("config").get("digest").asText();
        RunnableImageBuilder.Blob imageConfig = result.blobs.stream()
                .filter(blob -> blob.digest().equals(configDigest))
                .findFirst().orElseThrow();
        JsonNode rootfs = MAPPER.readTree(imageConfig.data()).get("rootfs");
        assertEquals(bundle.config().getLayerDiffId(), rootfs.get("diff_ids").get(1).asText());
    }

    @Test
    void graphComparisonIncludesHashesAndCoordinates() throws IOException {
        DependencyLock changed = lock();
        changed.setDependencies(List.of(new DependencyLock.Entry(
                "g", "a", "1", "jar", null, "runtime", "a.jar", "sha256:changed")));
        IOException error = assertThrows(IOException.class,
                () -> DependencyBundle.verifyGraph(lock("a"), changed));
        assertTrue(error.getMessage().contains("does not match"));
    }

    @Test
    void graphComparisonRejectsScopeOnlyDifference() {
        DependencyLock changed = lock();
        changed.setDependencies(List.of(new DependencyLock.Entry(
                "g", "a", "1", "jar", null, "compile", "a.jar", "sha256:a")));
        assertThrows(IOException.class,
                () -> DependencyBundle.verifyGraph(lock("a"), changed));
    }

    @Test
    void bundleRejectsNonEmptyUstarPrefix() throws IOException {
        Path dependency = temp.resolve("a.jar");
        Files.writeString(dependency, "a");
        ArtifactLayer layer = LayerBuilder.buildBundle(List.of(
                new LayerBuilder.Dep("a.jar", dependency, false)));
        byte[] prefixedTar = layer.tar().clone();
        prefixedTar[345] = 'x';

        assertThrows(IOException.class, () -> DependencyBundle.build(
                config(), lock("a"), new ArtifactLayer(
                        layer.name(), prefixedTar, layer.mediaType())));
    }

    @Test
    void layoutLoadingRejectsTamperedBlob() throws IOException {
        Path dependency = temp.resolve("a.jar");
        Files.writeString(dependency, "a");
        DependencyBundle.Content content = DependencyBundle.build(config(), lock("a"),
                LayerBuilder.buildBundle(List.of(
                        new LayerBuilder.Dep("a.jar", dependency, false))));
        LocalStore store = new LocalStore(temp.resolve("tampered"));
        store.pushDependencyBundle("example/deps:1", content);
        Path lockBlob = store.blobPath(content.config().getLockDigest());
        Files.writeString(lockBlob, "tampered");
        IOException error = assertThrows(IOException.class,
                () -> DependencyBundle.loadLayout(temp.resolve("tampered")));
        assertTrue(error.getMessage().contains("SHA-256 mismatch"));
    }

    @Test
    void parsingRejectsNonContractManifest() throws IOException {
        Path dependency = temp.resolve("a.jar");
        Files.writeString(dependency, "a");
        DependencyBundle.Content content = DependencyBundle.build(config(), lock("a"),
                LayerBuilder.buildBundle(List.of(
                        new LayerBuilder.Dep("a.jar", dependency, false))));

        ObjectNode wrongVersion = (ObjectNode) MAPPER.readTree(content.manifestBytes());
        wrongVersion.put("schemaVersion", 3);
        assertThrows(IOException.class, () -> DependencyBundle.parse(
                MAPPER.writeValueAsBytes(wrongVersion), digest -> new byte[0]));

        ObjectNode missingVersion = (ObjectNode) MAPPER.readTree(content.manifestBytes());
        missingVersion.remove("schemaVersion");
        assertThrows(IOException.class, () -> DependencyBundle.parse(
                MAPPER.writeValueAsBytes(missingVersion), digest -> new byte[0]));

        ObjectNode wrongMediaType = (ObjectNode) MAPPER.readTree(content.manifestBytes());
        wrongMediaType.put("mediaType", "application/json");
        assertThrows(IOException.class, () -> DependencyBundle.parse(
                MAPPER.writeValueAsBytes(wrongMediaType), digest -> new byte[0]));

        ObjectNode missingMediaType = (ObjectNode) MAPPER.readTree(content.manifestBytes());
        missingMediaType.remove("mediaType");
        assertThrows(IOException.class, () -> DependencyBundle.parse(
                MAPPER.writeValueAsBytes(missingMediaType), digest -> new byte[0]));

        ObjectNode unknownField = (ObjectNode) MAPPER.readTree(content.manifestBytes());
        unknownField.put("unexpected", true);
        assertThrows(IOException.class, () -> DependencyBundle.parse(
                MAPPER.writeValueAsBytes(unknownField), digest -> new byte[0]));

        ObjectNode subject = (ObjectNode) MAPPER.readTree(content.manifestBytes());
        subject.putObject("subject")
                .put("mediaType", MediaTypes.OCI_MANIFEST_MEDIA_TYPE)
                .put("digest", "sha256:" + "a".repeat(64))
                .put("size", 1);
        assertThrows(IOException.class, () -> DependencyBundle.parse(
                MAPPER.writeValueAsBytes(subject), digest -> new byte[0]));

        ObjectNode annotations = (ObjectNode) MAPPER.readTree(content.manifestBytes());
        annotations.putObject("annotations").put("example", "value");
        assertThrows(IOException.class, () -> DependencyBundle.parse(
                MAPPER.writeValueAsBytes(annotations), digest -> new byte[0]));
    }

    @Test
    void parsingRejectsDuplicateCompatibleJdks() throws IOException {
        Path dependency = temp.resolve("a.jar");
        Files.writeString(dependency, "a");
        DependencyBundle.Content content = DependencyBundle.build(config(), lock("a"),
                LayerBuilder.buildBundle(List.of(
                        new LayerBuilder.Dep("a.jar", dependency, false))));
        ObjectNode config = (ObjectNode) MAPPER.readTree(content.configBytes());
        config.putArray("compatibleJdks").add(21).add(21);
        byte[] configBytes = MAPPER.writeValueAsBytes(config);
        String configDigest = LocalStore.sha256Hex(configBytes);

        ObjectNode manifest = (ObjectNode) MAPPER.readTree(content.manifestBytes());
        ((ObjectNode) manifest.path("config"))
                .put("digest", configDigest)
                .put("size", configBytes.length);
        byte[] manifestBytes = MAPPER.writeValueAsBytes(manifest);

        assertThrows(IOException.class, () -> DependencyBundle.parse(manifestBytes, digest -> {
            if (digest.equals(configDigest)) {
                return configBytes;
            }
            if (digest.equals(content.config().getLockDigest())) {
                return content.lockBytes();
            }
            if (digest.equals(content.config().getLayerDigest())) {
                return content.compressedLayer();
            }
            throw new IOException("unexpected digest " + digest);
        }));
    }

    @Test
    void parsingRejectsLegacyUnsignedPolicyField() throws IOException {
        Path dependency = temp.resolve("a.jar");
        Files.writeString(dependency, "a");
        DependencyBundle.Content content = DependencyBundle.build(config(), lock("a"),
                LayerBuilder.buildBundle(List.of(
                        new LayerBuilder.Dep("a.jar", dependency, false))));
        ObjectNode config = (ObjectNode) MAPPER.readTree(content.configBytes());
        config.put("allowUnsigned", "true");
        byte[] configBytes = MAPPER.writeValueAsBytes(config);
        String configDigest = LocalStore.sha256Hex(configBytes);
        ObjectNode manifest = (ObjectNode) MAPPER.readTree(content.manifestBytes());
        ((ObjectNode) manifest.path("config"))
                .put("digest", configDigest)
                .put("size", configBytes.length);

        assertThrows(IOException.class, () -> DependencyBundle.parse(
                MAPPER.writeValueAsBytes(manifest), digest -> {
                    if (digest.equals(configDigest)) {
                        return configBytes;
                    }
                    if (digest.equals(content.config().getLockDigest())) {
                        return content.lockBytes();
                    }
                    return content.compressedLayer();
                }));
    }

    @Test
    void sourceBomRequiresGav() {
        assertDoesNotThrow(() -> DependencyBundleMojo.validateSourceBom("com.acme:platform:1.2.3"));
        assertThrows(MojoExecutionException.class,
                () -> DependencyBundleMojo.validateSourceBom("com.acme:platform"));
        assertThrows(MojoExecutionException.class,
                () -> DependencyBundleMojo.validateSourceBom(null));
    }

    @Test
    void evidenceIsCanonicalAndContainsOnlyInformationalFields() throws IOException {
        String json = DependencyBundle.canonicalJson(new ManagedDependencyEvidence(
                true, "sha256:app", "sha256:bundle", "sha256:layer",
                "sha256:lock", "g:b:1"));
        JsonNode node = MAPPER.readTree(json);
        assertEquals(1, node.get("schemaVersion").asInt());
        assertTrue(node.get("thinJar").asBoolean());
        assertEquals("sha256:bundle", node.get("dependencyBundleDigest").asText());
        assertEquals("sha256:lock", node.get("dependencyLockDigest").asText());
    }

    private record BadTar(String name, UnaryOperator<byte[]> mutation, String diagnostic) {}

    @TestFactory
    Stream<DynamicTest> malformedTarIsRejectedEvenWhenOuterDigestsMatch() throws IOException {
        byte[] validTar = new TarWriter().addFile("a.jar", new byte[]{'a'}).toByteArray();
        ArtifactLayer layer = new ArtifactLayer("dependencies", validTar, MediaTypes.CLASSPATH_LAYER_MEDIA_TYPE);
        DependencyBundle.Content template = DependencyBundle.build(config(), lock("a"), layer);
        return Stream.of(
                new BadTar("header checksum", bytes -> { bytes[100] ^= 1; return bytes; }, "checksum"),
                new BadTar("invalid checksum field", bytes -> { bytes[148] = '8'; return bytes; }, "checksum"),
                new BadTar("empty checksum field", bytes -> {
                    Arrays.fill(bytes, 148, 156, (byte) ' '); return bytes;
                }, "checksum"),
                new BadTar("nonzero file padding", bytes -> { bytes[513] = 1; return bytes; }, "padding"),
                new BadTar("no end markers", bytes -> Arrays.copyOf(bytes, 1024), "tar"),
                new BadTar("one end marker", bytes -> Arrays.copyOf(bytes, 1536), "tar"),
                new BadTar("partial block", bytes -> Arrays.copyOf(bytes, bytes.length - 1), "tar"),
                new BadTar("partial header", bytes -> Arrays.copyOf(bytes, 511), "tar"),
                new BadTar("invalid second end block", bytes -> { bytes[bytes.length - 1] = 1; return bytes; }, "tar"),
                new BadTar("data after end markers", bytes -> {
                    byte[] extended = Arrays.copyOf(bytes, bytes.length + 512);
                    extended[bytes.length] = 1;
                    return extended;
                }, "tar"),
                new BadTar("oversized payload", bytes -> headerField(bytes, 124, 12, "00000010000"), "size"),
                new BadTar("invalid size", bytes -> headerField(bytes, 124, 12, "00000000008"), "size"),
                new BadTar("invalid mode", bytes -> headerField(bytes, 100, 8, "invalid"), "mode"),
                new BadTar("invalid uid", bytes -> headerField(bytes, 108, 8, "invalid"), "uid"),
                new BadTar("invalid gid", bytes -> headerField(bytes, 116, 8, "invalid"), "gid"),
                new BadTar("invalid mtime", bytes -> headerField(bytes, 136, 12, "invalid"), "mtime"),
                new BadTar("invalid device major", bytes -> headerField(bytes, 329, 8, "invalid"), "device major"),
                new BadTar("invalid device minor", bytes -> headerField(bytes, 337, 8, "invalid"), "device minor"),
                new BadTar("unknown header format", bytes -> headerField(bytes, 257, 6, "other"), "USTAR"),
                new BadTar("unknown USTAR version", bytes -> headerField(bytes, 263, 2, "01"), "USTAR"),
                new BadTar("prefix", bytes -> headerField(bytes, 345, 155, "nested"), "flat regular"),
                new BadTar("link", bytes -> headerField(bytes, 156, 1, "2"), "flat regular"),
                new BadTar("traversal", bytes -> headerField(bytes, 0, 100, "../a.jar"), "flat regular"),
                new BadTar("unlocked file", bytes -> headerField(bytes, 0, 100, "b.jar"), "absent from the lock"),
                new BadTar("duplicate file", bytes -> {
                    byte[] duplicate = new byte[bytes.length + 1024];
                    System.arraycopy(bytes, 0, duplicate, 0, 1024);
                    System.arraycopy(bytes, 0, duplicate, 1024, bytes.length);
                    return duplicate;
                }, "duplicate"),
                new BadTar("locked content changed", bytes -> { bytes[512] = 'b'; return bytes; }, "checksum")
        ).map(test -> DynamicTest.dynamicTest(test.name(), () -> {
            byte[] malformed = test.mutation().apply(validTar.clone());
            IOException buildFailure = assertThrows(IOException.class, () -> DependencyBundle.build(
                    config(), lock("a"), new ArtifactLayer(layer.name(), malformed, layer.mediaType())));
            assertTrue(buildFailure.getMessage().contains(test.diagnostic()), buildFailure.getMessage());
            IOException parseFailure = assertThrows(IOException.class, () -> parseWithTar(template, malformed));
            assertTrue(parseFailure.getMessage().contains(test.diagnostic()), parseFailure.getMessage());
        }));
    }

    @Test
    void validTarFramingAndHistoricalChecksumRemainCompatible() throws IOException {
        for (String name : List.of("a.jar", "caf\u00e9.jar")) {
            byte[] content = new byte[512];
            Arrays.fill(content, (byte) 'a');
            DependencyLock inventory = new DependencyLock();
            inventory.setDependencies(List.of(new DependencyLock.Entry(
                    "g", "a", "1", "jar", null, "runtime", name,
                    LocalStore.sha256Hex(content).substring(7))));
            byte[] tar = new TarWriter().addFile(name, content).toByteArray();
            headerField(tar, 100, 8, "0000755");
            headerField(tar, 108, 8, "0000765");
            headerField(tar, 116, 8, "0000024");
            headerField(tar, 136, 12, "00000000001");
            for (boolean signed : List.of(false, true)) {
                byte[] variant = tar.clone();
                tarChecksum(variant, signed);
                variant = Arrays.copyOf(variant, variant.length + 512);
                DependencyBundle.Content built = DependencyBundle.build(config(), inventory,
                        new ArtifactLayer("dependencies", variant, MediaTypes.CLASSPATH_LAYER_MEDIA_TYPE));
                assertArrayEquals(variant, parseWithTar(built, variant).uncompressedLayer());
            }
        }
        assertThrows(IOException.class, () -> DependencyBundle.build(config(), lock("a"),
                new ArtifactLayer("dependencies", new TarWriter().toByteArray(), MediaTypes.CLASSPATH_LAYER_MEDIA_TYPE)));
        assertThrows(IOException.class, () -> DependencyBundle.build(config(), lock("a"),
                new ArtifactLayer("dependencies", new byte[0], MediaTypes.CLASSPATH_LAYER_MEDIA_TYPE)));
    }

    @Test
    void invalidUtf8CannotAliasALockedReplacementCharacter() throws IOException {
        DependencyLock inventory = new DependencyLock();
        inventory.setDependencies(List.of(new DependencyLock.Entry(
                "g", "a", "1", "jar", null, "runtime", "\ufffd.jar",
                LocalStore.sha256Hex(new byte[]{'a'}).substring(7))));
        byte[] valid = new TarWriter().addFile("\ufffd.jar", new byte[]{'a'}).toByteArray();
        DependencyBundle.Content template = DependencyBundle.build(config(), inventory,
                new ArtifactLayer("dependencies", valid, MediaTypes.CLASSPATH_LAYER_MEDIA_TYPE));
        byte[] invalid = new TarWriter().addFile("a.jar", new byte[]{'a'}).toByteArray();
        invalid[0] = (byte) 0xff;
        tarChecksum(invalid, false);
        IOException failure = assertThrows(IOException.class, () -> parseWithTar(template, invalid));
        assertTrue(failure.getMessage().contains("UTF-8"), failure.getMessage());
    }

    private static byte[] headerField(byte[] tar, int offset, int length, String value) {
        Arrays.fill(tar, offset, offset + length, (byte) 0);
        byte[] field = value.getBytes(StandardCharsets.US_ASCII);
        System.arraycopy(field, 0, tar, offset, field.length);
        tarChecksum(tar, false);
        return tar;
    }

    private static void tarChecksum(byte[] tar, boolean signed) {
        Arrays.fill(tar, 148, 156, (byte) ' ');
        int sum = 0;
        for (int i = 0; i < 512; i++) sum += signed ? tar[i] : tar[i] & 0xff;
        byte[] checksum = String.format(Locale.ROOT, "%06o", sum).getBytes(StandardCharsets.US_ASCII);
        System.arraycopy(checksum, 0, tar, 148, checksum.length);
        tar[154] = 0;
    }

    private static DependencyBundle.Content parseWithTar(DependencyBundle.Content original, byte[] tar)
            throws IOException {
        ByteArrayOutputStream buffer = new ByteArrayOutputStream();
        try (GZIPOutputStream gzip = new GZIPOutputStream(buffer)) {
            gzip.write(tar);
        }
        byte[] layer = buffer.toByteArray();
        String layerDigest = LocalStore.sha256Hex(layer);
        ObjectNode config = (ObjectNode) MAPPER.readTree(original.configBytes());
        config.put("layerDigest", layerDigest);
        config.put("layerDiffId", LocalStore.sha256Hex(tar));
        byte[] configBytes = MAPPER.writeValueAsBytes(config);
        String configDigest = LocalStore.sha256Hex(configBytes);
        ObjectNode manifest = (ObjectNode) MAPPER.readTree(original.manifestBytes());
        ((ObjectNode) manifest.get("config")).put("digest", configDigest).put("size", configBytes.length);
        for (JsonNode descriptor : manifest.get("layers")) {
            if (MediaTypes.OCI_LAYER_GZIP_MEDIA_TYPE.equals(descriptor.get("mediaType").asText())) {
                ((ObjectNode) descriptor).put("digest", layerDigest).put("size", layer.length);
            }
        }
        return DependencyBundle.parse(MAPPER.writeValueAsBytes(manifest), digest -> {
            if (digest.equals(configDigest)) return configBytes;
            if (digest.equals(layerDigest)) return layer;
            if (digest.equals(original.config().getLockDigest())) return original.lockBytes();
            throw new IOException("unexpected fixture digest " + digest);
        });
    }

    private static DependencyBundleConfig config() {
        DependencyBundleConfig config = new DependencyBundleConfig();
        config.setName("platform");
        config.setVersion("1");
        config.setSourceBom("g:b:1");
        config.setCompatibleJdks(List.of(21, 17, 17));
        return config;
    }

    private static DependencyLock lock(String... names) {
        DependencyLock lock = new DependencyLock();
        lock.setDependencies(java.util.Arrays.stream(names)
                .map(name -> new DependencyLock.Entry(
                        "g", name, "1", "jar", null, "runtime", name + ".jar",
                        name.equals("a")
                                ? LocalStore.sha256Hex("a".getBytes()).substring(7)
                                : LocalStore.sha256Hex("b".getBytes()).substring(7)))
                .toList());
        return lock;
    }
}
