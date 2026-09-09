// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin.oci;

import com.fasterxml.jackson.databind.MapperFeature;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.SerializationFeature;
import com.fasterxml.jackson.databind.json.JsonMapper;
import sh.brewlet.maven.plugin.model.DependencyBundleConfig;
import sh.brewlet.maven.plugin.model.DependencyLock;

import java.io.IOException;
import java.io.ByteArrayInputStream;
import java.io.ByteArrayOutputStream;
import java.nio.ByteBuffer;
import java.nio.charset.CharacterCodingException;
import java.nio.charset.CodingErrorAction;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.charset.StandardCharsets;
import java.util.Arrays;
import java.util.HashMap;
import java.util.HashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.zip.GZIPInputStream;
import java.util.zip.GZIPOutputStream;

/** Builds, validates, and loads Brewlet managed dependency bundles. */
public final class DependencyBundle {
    private static final int TAR_BLOCK = 512;
    private static final byte[] USTAR = {'u', 's', 't', 'a', 'r', 0, '0', '0'};
    private static final ObjectMapper CANONICAL = JsonMapper.builder()
            .enable(MapperFeature.SORT_PROPERTIES_ALPHABETICALLY)
            .disable(MapperFeature.ALLOW_COERCION_OF_SCALARS)
            .enable(SerializationFeature.ORDER_MAP_ENTRIES_BY_KEYS)
            .build();

    private DependencyBundle() {}

    /** Serializes an evidence/config value using Brewlet's canonical JSON rules. */
    public static String canonicalJson(Object value) throws IOException {
        return CANONICAL.writeValueAsString(value);
    }

    public record Content(DependencyBundleConfig config, DependencyLock lock,
                          byte[] compressedLayer, byte[] uncompressedLayer,
                          String manifestDigest,
                          byte[] configBytes, byte[] lockBytes, byte[] manifestBytes) {}

    public static Content build(DependencyBundleConfig config, DependencyLock lock,
                                ArtifactLayer layer) throws IOException {
        config.normalizeCompatibleJdks();
        validateConfig(config, false);
        validateLock(lock);
        validateLayer(layer.tar(), lock);
        byte[] lockBytes = CANONICAL.writeValueAsBytes(lock);
        config.setLockDigest(LocalStore.sha256Hex(lockBytes));
        byte[] compressedLayer = gzip(layer.tar());
        config.setLayerDigest(LocalStore.sha256Hex(compressedLayer));
        config.setLayerDiffId(LocalStore.sha256Hex(layer.tar()));
        byte[] configBytes = CANONICAL.writeValueAsBytes(config);
        OciDescriptor configDesc = descriptor(
                MediaTypes.DEPENDENCY_BUNDLE_CONFIG_MEDIA_TYPE, configBytes, null);
        OciDescriptor lockDesc = descriptor(MediaTypes.DEPENDENCY_LOCK_MEDIA_TYPE,
                lockBytes, "dependency-lock.json");
        OciDescriptor layerDesc = descriptor(MediaTypes.OCI_LAYER_GZIP_MEDIA_TYPE,
                compressedLayer, layer.name());
        layerDesc.setAnnotations(Map.of(
                MediaTypes.ANNOTATION_TITLE, layer.name(),
                MediaTypes.LAYER_ROLE_ANNOTATION, MediaTypes.LAYER_ROLE_CLASSPATH));

        OciManifest manifest = new OciManifest();
        manifest.setArtifactType(MediaTypes.DEPENDENCY_BUNDLE_ARTIFACT_TYPE);
        manifest.setConfig(configDesc);
        manifest.setLayers(List.of(layerDesc, lockDesc));
        byte[] manifestBytes = CANONICAL.writeValueAsBytes(manifest);
        return new Content(config, lock, compressedLayer, layer.tar(),
                LocalStore.sha256Hex(manifestBytes),
                configBytes, lockBytes, manifestBytes);
    }

    public static Content parse(byte[] manifestBytes, BlobSource blobs) throws IOException {
        requireFields(CANONICAL.readTree(manifestBytes), "dependency bundle manifest",
                "schemaVersion", "mediaType", "artifactType", "config", "layers");
        OciManifest manifest = CANONICAL.readValue(manifestBytes, OciManifest.class);
        if (manifest.getSchemaVersion() != 2
                || !MediaTypes.OCI_MANIFEST_MEDIA_TYPE.equals(manifest.getMediaType())) {
            throw new IOException("Dependency bundle must use OCI image manifest schema version 2");
        }
        if (manifest.getSubject() != null || manifest.getAnnotations() != null) {
            throw new IOException("Dependency bundle manifest contains non-contract fields");
        }
        if (!MediaTypes.DEPENDENCY_BUNDLE_ARTIFACT_TYPE.equals(manifest.getArtifactType())) {
            throw new IOException("Expected dependency bundle artifactType "
                    + MediaTypes.DEPENDENCY_BUNDLE_ARTIFACT_TYPE + " but found "
                    + manifest.getArtifactType());
        }
        OciDescriptor cfg = manifest.getConfig();
        requireMediaType(cfg, MediaTypes.DEPENDENCY_BUNDLE_CONFIG_MEDIA_TYPE, "config");
        if (manifest.getLayers() == null || manifest.getLayers().size() != 2) {
            throw new IOException("Dependency bundle must contain exactly a lock and classpath layer");
        }
        OciDescriptor lockDesc = manifest.getLayers().stream()
                .filter(layer -> MediaTypes.DEPENDENCY_LOCK_MEDIA_TYPE.equals(layer.getMediaType()))
                .findFirst().orElseThrow(() -> new IOException(
                        "Dependency bundle has no dependency lock"));
        OciDescriptor layerDesc = manifest.getLayers().stream()
                .filter(layer -> MediaTypes.OCI_LAYER_GZIP_MEDIA_TYPE.equals(layer.getMediaType()))
                .findFirst().orElseThrow(() -> new IOException(
                        "Dependency bundle has no classpath layer"));
        if (layerDesc.getAnnotations() == null
                || !MediaTypes.LAYER_ROLE_CLASSPATH.equals(
                layerDesc.getAnnotations().get(MediaTypes.LAYER_ROLE_ANNOTATION))) {
            throw new IOException("Dependency bundle layer must have brewlet.sh/layer=classpath");
        }

        byte[] configBytes = verifiedBlob(blobs, cfg);
        byte[] lockBytes = verifiedBlob(blobs, lockDesc);
        byte[] compressedLayer = verifiedBlob(blobs, layerDesc);
        byte[] uncompressedLayer = gunzip(compressedLayer);
        requireFields(CANONICAL.readTree(configBytes), "dependency bundle config",
                "schemaVersion", "name", "version", "sourceBom", "lockDigest",
                "layerDigest", "layerDiffId");
        requireFields(CANONICAL.readTree(lockBytes), "dependency lock",
                "schemaVersion", "artifacts");
        DependencyBundleConfig config = CANONICAL.readValue(configBytes, DependencyBundleConfig.class);
        DependencyLock lock = CANONICAL.readValue(lockBytes, DependencyLock.class);
        validateConfig(config, true);
        validateLock(lock);
        if (!lockDesc.getDigest().equals(config.getLockDigest())) {
            throw new IOException("Bundle config lockDigest does not match lock descriptor");
        }
        if (!layerDesc.getDigest().equals(config.getLayerDigest())) {
            throw new IOException("Bundle config layerDigest does not match layer descriptor");
        }
        if (!LocalStore.sha256Hex(uncompressedLayer).equals(config.getLayerDiffId())) {
            throw new IOException("Bundle config layerDiffId does not match uncompressed layer");
        }
        validateLayer(uncompressedLayer, lock);
        return new Content(config, lock, compressedLayer, uncompressedLayer,
                LocalStore.sha256Hex(manifestBytes), configBytes, lockBytes, manifestBytes);
    }

    public static Content loadLayout(Path root) throws IOException {
        Path indexPath = root.resolve("index.json");
        if (!Files.isRegularFile(indexPath)) {
            throw new IOException("Not an OCI image layout (missing index.json): " + root);
        }
        OciIndex index = CANONICAL.readValue(indexPath.toFile(), OciIndex.class);
        if (index.getManifests() == null) {
            throw new IOException("OCI layout contains no manifests: " + root);
        }
        List<OciDescriptor> bundles = index.getManifests().stream()
                .filter(d -> MediaTypes.DEPENDENCY_BUNDLE_ARTIFACT_TYPE.equals(d.getArtifactType()))
                .toList();
        if (bundles.size() != 1) {
            throw new IOException("OCI layout must contain exactly one dependency bundle; found "
                    + bundles.size());
        }
        OciDescriptor descriptor = bundles.get(0);
        if (!MediaTypes.OCI_MANIFEST_MEDIA_TYPE.equals(descriptor.getMediaType())) {
            throw new IOException("Dependency bundle index descriptor has invalid media type");
        }
        byte[] manifest = readLayoutBlob(root, descriptor.getDigest());
        if (manifest.length != descriptor.getSize()) {
            throw new IOException("Dependency bundle index descriptor size mismatch");
        }
        verifyDigest(manifest, descriptor.getDigest(), "manifest");
        return parse(manifest, digest -> readLayoutBlob(root, digest));
    }

    private static void requireFields(com.fasterxml.jackson.databind.JsonNode value,
                                      String document, String... fields) throws IOException {
        if (!value.isObject()) {
            throw new IOException(document + " must be a JSON object");
        }
        for (String field : fields) {
            if (!value.hasNonNull(field)) {
                throw new IOException(document + " is missing required field " + field);
            }
        }
    }

    public static void verifyGraph(DependencyLock expected, DependencyLock actual) throws IOException {
        if (!expected.getDependencies().equals(actual.getDependencies())) {
            throw new IOException("Current Maven runtime dependency graph does not match the "
                    + "dependency bundle lock. Rebuild the bundle or align project dependencies.");
        }
    }

    @FunctionalInterface
    public interface BlobSource {
        byte[] read(String digest) throws IOException;
    }

    private static OciDescriptor descriptor(String mediaType, byte[] bytes, String title) {
        OciDescriptor desc = new OciDescriptor(mediaType, LocalStore.sha256Hex(bytes), bytes.length);
        if (title != null) {
            desc.setAnnotations(Map.of(MediaTypes.ANNOTATION_TITLE, title));
        }
        return desc;
    }

    private static void requireMediaType(OciDescriptor descriptor, String expected, String role)
            throws IOException {
        if (descriptor == null || !expected.equals(descriptor.getMediaType())) {
            throw new IOException("Invalid dependency bundle " + role + " media type; expected "
                    + expected);
        }
    }

    private static byte[] verifiedBlob(BlobSource blobs, OciDescriptor descriptor) throws IOException {
        byte[] bytes = blobs.read(descriptor.getDigest());
        verifyDigest(bytes, descriptor.getDigest(), descriptor.getMediaType());
        if (bytes.length != descriptor.getSize()) {
            throw new IOException("Blob size mismatch for " + descriptor.getDigest());
        }
        return bytes;
    }

    private static void verifyDigest(byte[] bytes, String expected, String role) throws IOException {
        if (!LocalStore.sha256Hex(bytes).equals(expected)) {
            throw new IOException("SHA-256 mismatch for dependency bundle " + role);
        }
    }

    private static byte[] readLayoutBlob(Path root, String digest) throws IOException {
        if (digest == null || !digest.matches("sha256:[0-9a-f]{64}")) {
            throw new IOException("Invalid OCI digest: " + digest);
        }
        return Files.readAllBytes(root.resolve("blobs/sha256").resolve(digest.substring(7)));
    }

    private static void validateConfig(DependencyBundleConfig config, boolean requireLock)
            throws IOException {
        if (config == null || config.getSchemaVersion() != 1) {
            throw new IOException("Unsupported dependency bundle config schema version");
        }
        if (config.getName() == null || config.getName().isBlank()
                || config.getVersion() == null || config.getVersion().isBlank()) {
            throw new IOException("Dependency bundle config requires name and version");
        }
        if (config.getSourceBom() == null
                || !config.getSourceBom().matches("[^:\\s]+:[^:\\s]+:[^:\\s]+")) {
            throw new IOException("Dependency bundle sourceBom must use G:A:V syntax");
        }
        if (requireLock && (!isDigest(config.getLockDigest())
                || !isDigest(config.getLayerDigest()) || !isDigest(config.getLayerDiffId()))) {
            throw new IOException("Dependency bundle config has an invalid lock/layer/diff digest");
        }

        if (config.getCompatibleJdks() != null
                && config.getCompatibleJdks().stream().anyMatch(jdk -> jdk == null || jdk < 1)) {
            throw new IOException("compatibleJdks must contain positive feature versions");
        }
        if (config.getCompatibleJdks() != null) {
            List<Integer> canonical = config.getCompatibleJdks().stream().sorted().distinct().toList();
            if (!canonical.equals(config.getCompatibleJdks())) {
                throw new IOException("compatibleJdks must be unique and sorted");
            }
        }
    }

    private static boolean isDigest(String value) {
        return value != null && value.matches("sha256:[0-9a-f]{64}");
    }

    private static byte[] gzip(byte[] data) throws IOException {
        ByteArrayOutputStream output = new ByteArrayOutputStream();
        try (GZIPOutputStream gzip = new GZIPOutputStream(output)) {
            gzip.write(data);
        }
        return output.toByteArray();
    }

    private static byte[] gunzip(byte[] data) throws IOException {
        try (GZIPInputStream gzip = new GZIPInputStream(new ByteArrayInputStream(data))) {
            return gzip.readAllBytes();
        } catch (IOException e) {
            throw new IOException("Dependency bundle layer is not valid gzip", e);
        }
    }

    private static void validateLock(DependencyLock lock) throws IOException {
        if (lock == null || lock.getSchemaVersion() != 1) {
            throw new IOException("Unsupported dependency lock schema version");
        }
        if (lock.getDependencies() == null || lock.getDependencies().isEmpty()) {
            throw new IOException("Dependency lock must contain at least one artifact");
        }
        Set<String> coordinates = new HashSet<>();
        Set<String> filenames = new HashSet<>();
        for (DependencyLock.Entry entry : lock.getDependencies()) {
            if (entry.groupId() == null || entry.groupId().isBlank()
                    || entry.artifactId() == null || entry.artifactId().isBlank()
                    || entry.version() == null || entry.version().isBlank()
                    || entry.type() == null || entry.type().isBlank()
                    || entry.scope() == null || entry.scope().isBlank()
                    || entry.filename() == null || entry.filename().isBlank()
                    || entry.filename().contains("/") || entry.filename().contains("\\")
                    || entry.sha256() == null
                    || !entry.sha256().matches("[0-9a-f]{64}")) {
                throw new IOException("Dependency lock contains an invalid entry");
            }
            if (!coordinates.add(entry.coordinate()) || !filenames.add(entry.filename())) {
                throw new IOException("Dependency lock contains duplicate coordinates or filenames");
            }
        }
    }

    private static void validateLayer(byte[] tar, DependencyLock lock) throws IOException {
        if (tar.length < 2 * TAR_BLOCK || tar.length % TAR_BLOCK != 0) {
            throw new IOException("Invalid dependency classpath tar block framing or truncated archive");
        }
        Map<String, String> expected = new HashMap<>();
        for (DependencyLock.Entry entry : lock.getDependencies()) {
            expected.put(entry.filename(), entry.sha256());
        }
        Set<String> seen = new HashSet<>();
        int offset = 0;
        boolean terminated = false;
        while (offset < tar.length) {
            if (allZero(tar, offset, TAR_BLOCK)) {
                if (tar.length - offset < 2 * TAR_BLOCK
                        || !allZero(tar, offset + TAR_BLOCK, TAR_BLOCK)) {
                    throw new IOException("Dependency classpath tar requires two zero end blocks");
                }
                if (!allZero(tar, offset + 2 * TAR_BLOCK, tar.length - offset - 2 * TAR_BLOCK)) {
                    throw new IOException("Dependency classpath tar contains nonzero trailing data");
                }
                terminated = true;
                break;
            }
            validateTarChecksum(tar, offset);
            if (!Arrays.equals(tar, offset + 257, offset + 265, USTAR, 0, USTAR.length)) {
                throw new IOException("Dependency classpath layer must use USTAR headers");
            }
            tarOctal(tar, offset + 100, 8, "mode", false);
            tarOctal(tar, offset + 108, 8, "uid", false);
            tarOctal(tar, offset + 116, 8, "gid", false);
            tarOctal(tar, offset + 136, 12, "mtime", false);
            tarOctal(tar, offset + 329, 8, "device major", false);
            tarOctal(tar, offset + 337, 8, "device minor", false);
            String name = tarString(tar, offset, 100);
            String prefix = tarString(tar, offset + 345, 155);
            int type = tar[offset + 156] & 0xff;
            if (!prefix.isEmpty() || (type != 0 && type != '0')
                    || name.contains("/") || name.contains("\\")
                    || !name.toLowerCase(java.util.Locale.ROOT).endsWith(".jar")) {
                throw new IOException("Dependency classpath layer entry must be a flat regular JAR: "
                        + name);
            }
            if (!expected.containsKey(name)) {
                throw new IOException("Dependency classpath layer contains an artifact absent "
                        + "from the lock: " + name);
            }
            if (!seen.add(name)) {
                throw new IOException("Dependency classpath layer contains duplicate entry: " + name);
            }
            long size = tarOctal(tar, offset + 124, 12, "size", false);
            long dataEnd = offset + (long) TAR_BLOCK + size;
            long paddedEnd = offset + (long) TAR_BLOCK + ((size + TAR_BLOCK - 1) / TAR_BLOCK) * TAR_BLOCK;
            if (size > Integer.MAX_VALUE || dataEnd > tar.length || paddedEnd > tar.length) {
                throw new IOException("Invalid dependency classpath tar size for " + name);
            }
            if (!allZero(tar, (int) dataEnd, (int) (paddedEnd - dataEnd))) {
                throw new IOException("Nonzero dependency classpath tar padding for " + name);
            }
            byte[] content = Arrays.copyOfRange(tar, offset + TAR_BLOCK, (int) dataEnd);
            String digest = LocalStore.sha256Hex(content).substring("sha256:".length());
            if (!digest.equals(expected.get(name))) {
                throw new IOException("Dependency checksum mismatch for " + name);
            }
            offset = (int) paddedEnd;
        }
        if (!terminated) {
            throw new IOException("Dependency classpath tar is missing its end blocks");
        }
        if (!seen.equals(expected.keySet())) {
            Set<String> missing = new HashSet<>(expected.keySet());
            missing.removeAll(seen);
            throw new IOException("Dependency classpath layer is missing locked artifacts: "
                    + missing);
        }
    }

    private static boolean allZero(byte[] bytes, int offset, int length) {
        for (int i = offset; i < offset + length; i++) {
            if (bytes[i] != 0) {
                return false;
            }
        }
        return true;
    }

    private static void validateTarChecksum(byte[] bytes, int offset) throws IOException {
        long expected = tarOctal(bytes, offset + 148, 8, "checksum", true);
        int unsigned = 0;
        int signed = 0;
        for (int i = 0; i < TAR_BLOCK; i++) {
            int value = i >= 148 && i < 156 ? ' ' : bytes[offset + i];
            unsigned += value & 0xff;
            signed += value;
        }
        // Go's archive/tar also accepts the historical signed-byte checksum.
        if (expected != unsigned && expected != signed) {
            throw new IOException("Invalid dependency classpath tar header checksum");
        }
    }

    private static String tarString(byte[] bytes, int offset, int length) throws IOException {
        int end = offset;
        while (end < offset + length && bytes[end] != 0) {
            end++;
        }
        try {
            return StandardCharsets.UTF_8.newDecoder()
                    .onMalformedInput(CodingErrorAction.REPORT)
                    .onUnmappableCharacter(CodingErrorAction.REPORT)
                    .decode(ByteBuffer.wrap(bytes, offset, end - offset)).toString();
        } catch (CharacterCodingException e) {
            throw new IOException("Invalid UTF-8 in dependency classpath tar header", e);
        }
    }

    private static long tarOctal(byte[] bytes, int offset, int length, String field, boolean required)
            throws IOException {
        int start = offset;
        int end = offset + length;
        while (start < end && (bytes[start] == 0 || bytes[start] == ' ')) start++;
        while (end > start && (bytes[end - 1] == 0 || bytes[end - 1] == ' ')) end--;
        if (required && start == end) {
            throw new IOException("Missing dependency classpath tar " + field);
        }
        long value = 0;
        for (int i = start; i < end; i++) {
            int digit = bytes[i] - '0';
            if (digit < 0 || digit > 7 || value > (Long.MAX_VALUE - digit) / 8) {
                throw new IOException("Invalid dependency classpath tar " + field);
            }
            value = value * 8 + digit;
        }
        return value;
    }
}
