// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin.oci;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;

import java.io.IOException;
import java.net.URI;
import java.net.URLEncoder;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.Base64;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.ArrayList;
import java.util.LinkedHashSet;
import java.util.HashSet;
import java.util.Set;
import java.util.logging.Logger;

/**
 * Minimal OCI Distribution Spec v1.1 client that pushes a Brewlet OCI artifact
 * to any OCI-compliant registry. Implemented using the JDK's built-in
 * {@link HttpClient} — no external binary required (Option A from the design).
 *
 * <p>Push flow:
 * <ol>
 *   <li>Upload preassembled config and layer blobs not already present.</li>
 *   <li>PUT child manifests by digest.</li>
 *   <li>PUT the tagged root manifest or image index.</li>
 * </ol>
 *
 * <p>Authentication:
 * <ol>
 *   <li>Try request unauthenticated; if 401, parse {@code WWW-Authenticate} header.</li>
 *   <li>If ****** exchange credentials for a token at the realm URL.</li>
 *   <li>If Basic challenge, use Basic auth directly.</li>
 * </ol>
 */
public class RegistryClient {

    private static final Logger LOG = Logger.getLogger(RegistryClient.class.getName());
    private static final ObjectMapper MAPPER = new ObjectMapper();

    private final String registry;
    private final String repository;
    private final Credential credential;
    private final RegistryTrustPolicy trustPolicy;
    private final HttpClient httpClient;

    /** Resolved bearer token (cached after first auth). */
    private volatile String bearerToken;

    public RegistryClient(String registry, String repository, Credential credential) {
        this(registry, repository, credential, RegistryTrustPolicy.secureDefault());
    }

    /**
     * @param registry   registry authority ({@code host} or {@code host:port})
     * @param repository repository path within the registry
     * @param credential registry credentials, or {@code null} for anonymous access
     * @param trustPolicy transport and credential-forwarding policy; see
     *                    {@link RegistryTrustPolicy}
     */
    public RegistryClient(String registry, String repository, Credential credential,
                          RegistryTrustPolicy trustPolicy) {
        this.trustPolicy = trustPolicy == null
                ? RegistryTrustPolicy.secureDefault() : trustPolicy;
        if (!RegistryTrustPolicy.isBareAuthority(registry)) {
            throw new IllegalArgumentException("Invalid registry '" + registry
                    + "': expected a bare host or host:port authority");
        }
        this.registry = registry;
        this.repository = repository;
        this.credential = credential;
        this.httpClient = HttpClient.newBuilder()
                .connectTimeout(Duration.ofSeconds(30))
                .followRedirects(HttpClient.Redirect.NEVER)
                .build();
    }

    /** Publishes preassembled content, retaining managed-layer mount support. */
    public String pushApplicationImage(String reference, ApplicationImage image,
                                      String managedLayerDigest, String sourceRepository)
            throws IOException, InterruptedException {
        for (RunnableImageBuilder.Blob blob : image.blobs()) {
            if (!blobExists(blob.digest())) {
                LOG.info(String.format("Pushing %s blob %s (%,d bytes) ...",
                        blob.mediaType(), blob.digest(), blob.data().length));
                boolean mounted = blob.digest().equals(managedLayerDigest)
                        && sourceRepository != null
                        && mountOrUploadBlob(blob.digest(), sourceRepository, blob.data());
                if (!mounted) {
                    pushBlob(blob.digest(), blob.data());
                }
            } else {
                LOG.info(blob.mediaType() + " blob " + blob.digest()
                        + " already exists in registry (skipping upload).");
            }
        }
        for (RunnableImageBuilder.Blob manifest : image.manifests()) {
            pushManifest(manifest.digest(), manifest.data(), manifest.mediaType());
        }
        pushManifest(reference, image.root().data(), image.root().mediaType());
        LOG.info("Published " + image.root().mediaType() + ": " + image.root().digest());
        return image.root().digest();
    }

    /**
     * Attempts an OCI cross-repository blob mount. A 202 response means the
     * registry declined the mount and opened an upload; complete that session
     * using its Location rather than abandoning it and starting another upload.
     */
    private boolean mountOrUploadBlob(String digest, String sourceRepository, byte[] content)
            throws IOException, InterruptedException {
        String query = "?mount=" + URLEncoder.encode(digest, StandardCharsets.UTF_8)
                + "&from=" + URLEncoder.encode(sourceRepository, StandardCharsets.UTF_8);
        URI uri = registryUri("/v2/" + repository + "/blobs/uploads/" + query);
        HttpRequest request = authedRequest(uri, HttpRequest.newBuilder(uri)
                .POST(HttpRequest.BodyPublishers.noBody()));
        HttpResponse<Void> response =
                send(request, HttpResponse.BodyHandlers.discarding());
        if (response.statusCode() == 401) {
            negotiate(response);
            request = authedRequest(uri, HttpRequest.newBuilder(uri)
                    .POST(HttpRequest.BodyPublishers.noBody()));
            response = send(request, HttpResponse.BodyHandlers.discarding());
        }
        if (response.statusCode() == 201) {
            LOG.info("Mounted managed dependency blob " + digest + " from "
                    + sourceRepository);
            return true;
        }
        if (response.statusCode() == 202) {
            completeUpload(response, digest, content);
            return true;
        }
        if (response.statusCode() == 400
                || response.statusCode() == 404 || response.statusCode() == 405) {
            return false;
        }
        throw new IOException("Registry cross-repository mount failed: HTTP "
                + response.statusCode());
    }

    /** Pushes a managed dependency bundle and returns its manifest digest. */
    public String pushDependencyBundle(String reference, DependencyBundle.Content bundle)
            throws IOException, InterruptedException {
        for (byte[] blob : List.of(
                bundle.configBytes(), bundle.lockBytes(), bundle.compressedLayer())) {
            String digest = LocalStore.sha256Hex(blob);
            if (!blobExists(digest)) {
                pushBlob(digest, blob);
            }
        }
        pushManifest(reference, bundle.manifestBytes());
        return bundle.manifestDigest();
    }

    /** Pushes an OCI referrer by digest, with a deterministic tag fallback. */
    public String pushReferrer(OciReferrer.Content referrer)
            throws IOException, InterruptedException {
        for (byte[] blob : List.of(OciReferrer.emptyConfig(), referrer.document())) {
            String digest = LocalStore.sha256Hex(blob);
            if (!blobExists(digest)) {
                pushBlob(digest, blob);
            }
        }
        try {
            pushManifest(referrer.manifestDigest(), referrer.manifest());
        } catch (IOException unsupportedDigestPut) {
            LOG.fine("Registry rejected digest-addressed referrer PUT; using tag fallback");
        }
        // Also publish a per-referrer deterministic tag: a registry may accept
        // digest PUTs while not implementing the OCI 1.1 referrers endpoint.
        pushManifest(fallbackReferrerTag(
                referrer.model().getSubject().getDigest(),
                referrer.model().getArtifactType(), referrer.manifestDigest()),
                referrer.manifest());
        return referrer.manifestDigest();
    }

    /**
     * Discovers OCI 1.1 referrers. Registries without the API use Brewlet's
     * deterministic subject/artifact-type tag convention.
     */
    public List<OciDescriptor> discoverReferrers(String subjectDigest, String artifactType)
            throws IOException, InterruptedException {
        URI next = registryUri("/v2/" + repository + "/referrers/" + subjectDigest
                + "?artifactType=" + URLEncoder.encode(artifactType, StandardCharsets.UTF_8));
        Set<URI> visited = new HashSet<>();
        List<OciDescriptor> nativeReferrers = new ArrayList<>();
        int nativeEntries = 0;
        boolean firstPage = true;
        while (next != null) {
            if (!visited.add(next)) {
                throw new IOException("Registry referrer pagination contains a cycle");
            }
            HttpRequest request = authedRequest(next, HttpRequest.newBuilder(next).GET()
                    .header("Accept", MediaTypes.OCI_INDEX_MEDIA_TYPE));
            HttpResponse<byte[]> response = send(
                    request, HttpResponse.BodyHandlers.ofByteArray());
            if (response.statusCode() == 401) {
                negotiate(response);
                response = send(authedRequest(next, HttpRequest.newBuilder(next).GET()
                                .header("Accept", MediaTypes.OCI_INDEX_MEDIA_TYPE)),
                        HttpResponse.BodyHandlers.ofByteArray());
            }
            if (response.statusCode() != 200) {
                if (firstPage && (response.statusCode() == 400
                        || response.statusCode() == 404
                        || response.statusCode() == 405
                        || response.statusCode() == 406)) {
                    return discoverFallbackReferrers(subjectDigest, artifactType);
                }
                throw new IOException("Registry referrers discovery failed: HTTP "
                        + response.statusCode());
            }
            JsonNode indexNode = MAPPER.readTree(response.body());
            if (!indexNode.isObject()
                    || !indexNode.path("schemaVersion").isIntegralNumber()
                    || indexNode.path("schemaVersion").asInt(-1) != 2
                    || !MediaTypes.OCI_INDEX_MEDIA_TYPE.equals(
                            indexNode.path("mediaType").asText())
                    || !indexNode.path("manifests").isArray()) {
                throw new IOException("Registry returned an invalid OCI referrer index");
            }
            OciIndex index = MAPPER.treeToValue(indexNode, OciIndex.class);
            nativeEntries += index.getManifests().size();
            index.getManifests().stream()
                    .filter(d -> d != null && artifactType.equals(d.getArtifactType()))
                    .forEach(nativeReferrers::add);
            String link = nextLinkTarget(response.headers().allValues("Link"));
            next = link == null ? null : resolvePaginationLink(next, link);
            firstPage = false;
        }
        if (!nativeReferrers.isEmpty()) {
            return nativeReferrers;
        }
        List<OciDescriptor> fallbackReferrers =
                discoverFallbackReferrers(subjectDigest, artifactType);
        if (!fallbackReferrers.isEmpty()) {
            return fallbackReferrers;
        }
        if (nativeEntries != 0) {
            throw new IOException("Registry returned " + nativeEntries
                    + " native referrer descriptor(s), but none matched "
                    + artifactType);
        }
        return List.of();
    }

    private List<OciDescriptor> discoverFallbackReferrers(
            String subjectDigest, String artifactType)
            throws IOException, InterruptedException {
        String prefix = fallbackTag(subjectDigest, artifactType);
        List<OciDescriptor> descriptors = new ArrayList<>();
        int matchingTags = 0;
        for (String tag : listAllTags()) {
            if (!tag.startsWith(prefix + ".")) {
                continue;
            }
            matchingTags++;
            try {
                byte[] manifest = get("/v2/" + repository + "/manifests/" + tag,
                        MediaTypes.OCI_MANIFEST_MEDIA_TYPE);
                OciManifest model = MAPPER.readValue(manifest, OciManifest.class);
                if (!artifactType.equals(model.getArtifactType())
                        || model.getSubject() == null
                        || !subjectDigest.equals(model.getSubject().getDigest())) {
                    continue;
                }
                OciDescriptor descriptor = new OciDescriptor(
                        MediaTypes.OCI_MANIFEST_MEDIA_TYPE,
                        LocalStore.sha256Hex(manifest), manifest.length);
                descriptor.setArtifactType(artifactType);
                descriptors.add(descriptor);
            } catch (IOException invalidCandidate) {
                LOG.fine("Ignoring invalid referrer fallback tag " + tag + ": "
                        + invalidCandidate.getMessage());
            }
        }
        if (!descriptors.isEmpty()) {
            return descriptors;
        }
        if (matchingTags != 0) {
            throw new IOException("Found " + matchingTags + " referrer fallback tag(s) for "
                    + artifactType + ", but none contained a valid manifest");
        }
        return List.of();
    }

    private List<String> listAllTags() throws IOException, InterruptedException {
        URI next = registryUri("/v2/" + repository + "/tags/list?n=100");
        LinkedHashSet<String> tags = new LinkedHashSet<>();
        Set<URI> visited = new HashSet<>();
        while (next != null) {
            if (!visited.add(next)) {
                throw new IOException("Registry tag pagination contains a cycle");
            }
            HttpRequest request = authedRequest(next, HttpRequest.newBuilder(next).GET()
                    .header("Accept", "application/json"));
            HttpResponse<byte[]> response = send(
                    request, HttpResponse.BodyHandlers.ofByteArray());
            if (response.statusCode() == 401) {
                negotiate(response);
                response = send(authedRequest(next, HttpRequest.newBuilder(next).GET()
                                .header("Accept", "application/json")),
                        HttpResponse.BodyHandlers.ofByteArray());
            }
            if (response.statusCode() != 200) {
                throw new IOException("Registry tag discovery failed: HTTP "
                        + response.statusCode());
            }
            JsonNode values = MAPPER.readTree(response.body()).path("tags");
            if (values.isArray()) {
                values.forEach(value -> tags.add(value.asText()));
            }
            String link = nextLinkTarget(response.headers().allValues("Link"));
            next = link == null ? null : resolvePaginationLink(next, link);
        }
        return List.copyOf(tags);
    }

    private static String nextLinkTarget(List<String> headers) throws IOException {
        for (String header : headers) {
            for (String link : header.split("\\s*,\\s*(?=<)")) {
                int start = link.indexOf('<');
                int end = link.indexOf('>', start + 1);
                if (start < 0 || end < 0) {
                    throw new IOException("Registry returned an invalid pagination Link header");
                }
                for (String parameter : link.substring(end + 1).split(";")) {
                    String[] parts = parameter.trim().split("=", 2);
                    if (parts.length != 2 || !"rel".equalsIgnoreCase(parts[0].trim())) {
                        continue;
                    }
                    String relations = parts[1].trim().replaceAll("^\"|\"$", "");
                    for (String relation : relations.split("\\s+")) {
                        if ("next".equalsIgnoreCase(relation)) {
                            return link.substring(start + 1, end);
                        }
                    }
                }
            }
        }
        return null;
    }

    private URI resolvePaginationLink(URI current, String link) throws IOException {
        URI resolved = current.resolve(link);
        if (!RegistryTrustPolicy.sameOrigin(registryUri("/"), resolved)) {
            throw new IOException("Registry pagination Link points to a different origin");
        }
        return resolved;
    }

    /** Pulls the verified, single document layer from a referrer descriptor. */
    public byte[] pullReferrerDocument(OciDescriptor descriptor)
            throws IOException, InterruptedException {
        return pullReferrerDocument(descriptor, null, -1);
    }

    public byte[] pullReferrerDocument(OciDescriptor descriptor, String expectedSubject,
                                       long expectedSubjectSize)
            throws IOException, InterruptedException {
        if (descriptor == null || !isDigest(descriptor.getDigest())) {
            throw new IOException("Referrer manifest descriptor has an invalid digest");
        }
        byte[] manifestBytes = get("/v2/" + repository + "/manifests/"
                + descriptor.getDigest(), MediaTypes.OCI_MANIFEST_MEDIA_TYPE);
        if (!MediaTypes.OCI_MANIFEST_MEDIA_TYPE.equals(descriptor.getMediaType())
                || manifestBytes.length != descriptor.getSize()
                || !LocalStore.sha256Hex(manifestBytes).equals(descriptor.getDigest())) {
            throw new IOException("Referrer manifest media type, digest, or size mismatch");
        }
        OciManifest manifest = OciReferrer.parseManifest(manifestBytes);
        if (manifest == null
                || manifest.getSchemaVersion() != 2
                || !MediaTypes.OCI_MANIFEST_MEDIA_TYPE.equals(manifest.getMediaType())
                || !java.util.Objects.equals(
                        descriptor.getArtifactType(), manifest.getArtifactType())
                || manifest.getSubject() == null
                || (expectedSubject != null
                && (!expectedSubject.equals(manifest.getSubject().getDigest())
                || !MediaTypes.OCI_MANIFEST_MEDIA_TYPE.equals(
                        manifest.getSubject().getMediaType())
                || expectedSubjectSize != manifest.getSubject().getSize()))) {
            throw new IOException("Referrer manifest contract or subject mismatch");
        }
        OciDescriptor config = manifest.getConfig();
        if (config == null
                || !MediaTypes.OCI_EMPTY_CONFIG_MEDIA_TYPE.equals(config.getMediaType())
                || !LocalStore.sha256Hex(OciReferrer.emptyConfig()).equals(config.getDigest())
                || config.getSize() != OciReferrer.emptyConfig().length) {
            throw new IOException("Referrer must use the OCI empty config");
        }
        if (manifest.getLayers() == null || manifest.getLayers().size() != 1) {
            throw new IOException("Referrer must contain exactly one layer");
        }
        OciDescriptor layer = manifest.getLayers().get(0);
        if (layer == null || !isDigest(layer.getDigest())) {
            throw new IOException("Referrer document layer has an invalid digest");
        }
        String expectedLayerType = MediaTypes.CYCLONEDX_ARTIFACT_TYPE.equals(
                manifest.getArtifactType()) ? MediaTypes.CYCLONEDX_LAYER_MEDIA_TYPE
                : MediaTypes.DSSE_LAYER_MEDIA_TYPE;
        if (!expectedLayerType.equals(layer.getMediaType())) {
            throw new IOException("Referrer document layer media type mismatch");
        }
        byte[] document = get("/v2/" + repository + "/blobs/" + layer.getDigest(), null);
        if (!LocalStore.sha256Hex(document).equals(layer.getDigest())
                || document.length != layer.getSize()) {
            throw new IOException("Referrer document digest or size mismatch");
        }
        return document;
    }

    private static boolean isDigest(String value) {
        return value != null && value.matches("sha256:[0-9a-f]{64}");
    }

    public byte[] pullManifest(String reference) throws IOException, InterruptedException {
        return get("/v2/" + repository + "/manifests/" + reference,
                MediaTypes.OCI_MANIFEST_MEDIA_TYPE + ", " + MediaTypes.OCI_INDEX_MEDIA_TYPE);
    }

    static String fallbackTag(String subjectDigest, String artifactType) {
        String typeHash = LocalStore.sha256Hex(
                artifactType.getBytes(StandardCharsets.UTF_8)).substring(7, 19);
        return subjectDigest.replace(':', '-') + "." + typeHash;
    }

    static String fallbackReferrerTag(
            String subjectDigest, String artifactType, String referrerDigest) {
        return fallbackTag(subjectDigest, artifactType) + "."
                + referrerDigest.substring(7, 31);
    }

    /** Pulls and cryptographically verifies a managed dependency bundle. */
    public DependencyBundle.Content pullDependencyBundle(String reference)
            throws IOException, InterruptedException {
        byte[] manifest = get("/v2/" + repository + "/manifests/" + reference,
                MediaTypes.OCI_MANIFEST_MEDIA_TYPE + ", "
                        + MediaTypes.DEPENDENCY_BUNDLE_ARTIFACT_TYPE);
        JsonNode root = MAPPER.readTree(manifest);
        Map<String, byte[]> blobs = new HashMap<>();
        JsonNode config = root.get("config");
        if (config != null && config.hasNonNull("digest")) {
            String digest = config.get("digest").asText();
            blobs.put(digest, get("/v2/" + repository + "/blobs/" + digest, null));
        }
        JsonNode layers = root.get("layers");
        if (layers != null) {
            for (JsonNode layer : layers) {
                String digest = layer.path("digest").asText();
                blobs.put(digest, get("/v2/" + repository + "/blobs/" + digest, null));
            }
        }
        return DependencyBundle.parse(manifest, digest -> {
            byte[] bytes = blobs.get(digest);
            if (bytes == null) {
                throw new IOException("Bundle references unavailable blob " + digest);
            }
            return bytes;
        });
    }

    // -----------------------------------------------------------------------
    // OCI Distribution Spec API helpers
    // -----------------------------------------------------------------------

    /** Returns {@code true} if the blob already exists in the registry. */
    boolean blobExists(String digest) throws IOException, InterruptedException {
        URI uri = registryUri("/v2/" + repository + "/blobs/" + digest);
        HttpRequest req = authedRequest(uri, HttpRequest.newBuilder(uri).method("HEAD", HttpRequest.BodyPublishers.noBody()));
        HttpResponse<Void> resp = send(req, HttpResponse.BodyHandlers.discarding());
        if (resp.statusCode() == 401) {
            negotiate(resp);
            req = authedRequest(uri, HttpRequest.newBuilder(uri).method("HEAD", HttpRequest.BodyPublishers.noBody()));
            resp = send(req, HttpResponse.BodyHandlers.discarding());
        }
        if (resp.statusCode() == 200) return true;
        if (resp.statusCode() == 404) return false;
        throw new IOException("Failed to check blob " + digest + ": HTTP " + resp.statusCode());
    }

    /**
     * Pushes a blob using the monolithic upload flow:
     * POST /v2/{name}/blobs/uploads/ → then PUT with full content + digest.
     */
    void pushBlob(String digest, byte[] content) throws IOException, InterruptedException {
        // Initiate upload
        URI initiateUri = registryUri("/v2/" + repository + "/blobs/uploads/");
        HttpRequest initReq = authedRequest(initiateUri,
                HttpRequest.newBuilder(initiateUri)
                        .POST(HttpRequest.BodyPublishers.noBody()));
        HttpResponse<String> initResp = send(initReq, HttpResponse.BodyHandlers.ofString());
        if (initResp.statusCode() == 401) {
            negotiate(initResp);
            initResp = send(
                    authedRequest(initiateUri, HttpRequest.newBuilder(initiateUri)
                            .POST(HttpRequest.BodyPublishers.noBody())),
                    HttpResponse.BodyHandlers.ofString());
        }

        if (initResp.statusCode() != 202) {
            throw new IOException("Failed to initiate blob upload: HTTP " + initResp.statusCode()
                    + "\n" + initResp.body());
        }

        completeUpload(initResp, digest, content);
    }

    private void completeUpload(HttpResponse<?> initiation, String digest, byte[] content)
            throws IOException, InterruptedException {
        String location = initiation.headers().firstValue("Location")
                .orElseThrow(() -> new IOException("No Location header in upload initiation response"));
        URI uploadUri = resolveLocation(initiation.uri(), location, digest);

        // PUT the blob
        HttpRequest putReq = authedRequest(uploadUri,
                HttpRequest.newBuilder(uploadUri)
                        .PUT(HttpRequest.BodyPublishers.ofByteArray(content))
                        .header("Content-Type", "application/octet-stream"));
        HttpResponse<String> putResp = send(putReq, HttpResponse.BodyHandlers.ofString());
        if (putResp.statusCode() != 201) {
            throw new IOException("Failed to push blob " + digest + ": HTTP " + putResp.statusCode()
                    + "\n" + putResp.body());
        }
    }

    private byte[] get(String path, String accept) throws IOException, InterruptedException {
        URI uri = registryUri(path);
        HttpRequest.Builder builder = HttpRequest.newBuilder(uri).GET();
        if (accept != null) {
            builder.header("Accept", accept);
        }
        HttpResponse<byte[]> response = send(
                authedRequest(uri, builder), HttpResponse.BodyHandlers.ofByteArray());
        if (response.statusCode() == 401) {
            negotiate(response);
            builder = HttpRequest.newBuilder(uri).GET();
            if (accept != null) {
                builder.header("Accept", accept);
            }
            response = send(
                    authedRequest(uri, builder), HttpResponse.BodyHandlers.ofByteArray());
        }
        if (response.statusCode() != 200) {
            throw new IOException("Failed to pull OCI content " + path + ": HTTP "
                    + response.statusCode());
        }
        return response.body();
    }

    /** Pushes the OCI manifest for the given reference (tag or digest). */
    void pushManifest(String reference, byte[] manifestBytes)
            throws IOException, InterruptedException {
        pushManifest(reference, manifestBytes, MediaTypes.OCI_MANIFEST_MEDIA_TYPE);
    }

    /**
     * Pushes an OCI manifest or image index for the given reference (tag or
     * digest) with an explicit {@code Content-Type} — a runnable image pushes its
     * per-arch manifests with the image-manifest type and the tagged index with
     * the image-index type.
     */
    void pushManifest(String reference, byte[] manifestBytes, String contentType)
            throws IOException, InterruptedException {
        URI uri = registryUri("/v2/" + repository + "/manifests/" + reference);
        HttpRequest req = authedRequest(uri,
                HttpRequest.newBuilder(uri)
                        .PUT(HttpRequest.BodyPublishers.ofByteArray(manifestBytes))
                        .header("Content-Type", contentType));
        HttpResponse<String> resp = send(req, HttpResponse.BodyHandlers.ofString());
        if (resp.statusCode() == 401) {
            negotiate(resp);
            resp = send(
                    authedRequest(uri, HttpRequest.newBuilder(uri)
                            .PUT(HttpRequest.BodyPublishers.ofByteArray(manifestBytes))
                            .header("Content-Type", contentType)),
                    HttpResponse.BodyHandlers.ofString());
        }
        if (resp.statusCode() != 201) {
            throw new IOException("Failed to push manifest: HTTP " + resp.statusCode()
                    + "\n" + resp.body());
        }
    }

    // -----------------------------------------------------------------------
    // Authentication
    // -----------------------------------------------------------------------

    /**
     * Applies auth headers to the given request builder and builds it.
     *
     * <p>Credentials are attached only when {@code target} is same-origin with
     * the registry. Registry-supplied URLs (blob upload {@code Location},
     * pagination links, storage redirects) are therefore still usable, but they
     * never receive the registry bearer token or Basic credentials.
     */
    private HttpRequest authedRequest(URI target, HttpRequest.Builder builder) {
        if (!RegistryTrustPolicy.sameOrigin(registryUri("/"), target)) {
            return builder.build();
        }
        if (bearerToken != null) {
            builder.header("Authorization", "Bearer " + bearerToken);
        } else if (credential != null && credential.getUsername() != null
                && !credential.isIdentityToken()) {
            builder.header("Authorization", "Basic " + basicCredentials());
        }
        return builder.build();
    }

    /** Follow registry redirects with explicit transport and credential scoping. */
    private <T> HttpResponse<T> send(HttpRequest request, HttpResponse.BodyHandler<T> handler)
            throws IOException, InterruptedException {
        for (int redirects = 0; ; redirects++) {
            HttpResponse<T> response = httpClient.send(request, handler);
            int status = response.statusCode();
            if (status != 301 && status != 302 && status != 303 && status != 307 && status != 308) {
                return response;
            }
            if (redirects >= 10) {
                throw new IOException("Registry request exceeded 10 redirects");
            }
            String location = response.headers().firstValue("Location")
                    .orElseThrow(() -> new IOException("Registry redirect did not provide a Location"));
            URI target = resolveRegistryLocation(response.uri(), location);
            HttpRequest.Builder next = HttpRequest.newBuilder(request,
                    (name, value) -> !name.equalsIgnoreCase("Authorization")).uri(target);
            if ((status == 303 && !request.method().equals("HEAD"))
                    || ((status == 301 || status == 302) && request.method().equals("POST"))) {
                next.GET();
            }
            request = authedRequest(target, next);
        }
    }

    private String basicCredentials() {
        return Base64.getEncoder().encodeToString(
                (credential.getUsername() + ":" + credential.getPassword())
                        .getBytes(StandardCharsets.UTF_8));
    }

    /**
     * Parses the {@code WWW-Authenticate} header from a 401 response and
     * exchanges credentials for a ****** if needed.
     *
     * <p>The challenge realm is attacker-controlled input: a malicious or
     * compromised registry can point it at any origin. The realm is therefore
     * validated for scheme and shape before it is contacted, and registry
     * credentials are attached only when {@link RegistryTrustPolicy} approves
     * the realm origin. An untrusted realm fails closed rather than forwarding
     * credentials.
     */
    private void negotiate(HttpResponse<?> resp401) throws IOException, InterruptedException {
        String wwwAuth = resp401.headers().firstValue("WWW-Authenticate").orElse("");
        if (wwwAuth.startsWith("Bearer ")) {
            String realm = extractChallenge(wwwAuth, "realm");
            String service = extractChallenge(wwwAuth, "service");
            String scope = extractChallenge(wwwAuth, "scope");

            URI realmUri;
            try {
                realmUri = trustPolicy.validateTokenRealm(realm);
            } catch (IllegalArgumentException e) {
                throw new IOException(e.getMessage(), e);
            }

            boolean haveCredentials = credential != null && credential.getUsername() != null;
            if (haveCredentials && !trustPolicy.allowsCredentials(registryUri("/"), realmUri)) {
                throw new IOException("Registry " + registry + " requested a token from "
                        + realmUri.getScheme() + "://" + realmUri.getAuthority()
                        + ", which is not the registry origin. Registry credentials were "
                        + "not sent. Add that host to <allowedTokenRealms> "
                        + "(-Dbrewlet.allowedTokenRealms) only if you trust it with your "
                        + "registry credentials.");
            }

            HttpRequest.Builder tokenReqBuilder;
            if (haveCredentials && credential.isIdentityToken()) {
                // OAuth2 refresh-token grant (docker login identity tokens, e.g. ACR).
                String form = "grant_type=refresh_token"
                        + "&refresh_token=" + URLEncoder.encode(credential.getPassword(),
                                StandardCharsets.UTF_8)
                        + "&service=" + URLEncoder.encode(service, StandardCharsets.UTF_8)
                        + "&scope=" + URLEncoder.encode(scope, StandardCharsets.UTF_8)
                        + "&client_id=brewlet";
                tokenReqBuilder = HttpRequest.newBuilder(realmUri)
                        .header("Content-Type", "application/x-www-form-urlencoded")
                        .POST(HttpRequest.BodyPublishers.ofString(form));
            } else {
                String separator = realmUri.getRawQuery() == null ? "?" : "&";
                String tokenUrl = realmUri + separator
                        + "service=" + URLEncoder.encode(service, StandardCharsets.UTF_8)
                        + "&scope=" + URLEncoder.encode(scope, StandardCharsets.UTF_8);
                tokenReqBuilder = HttpRequest.newBuilder(URI.create(tokenUrl)).GET();
                if (haveCredentials) {
                    tokenReqBuilder.header("Authorization", "Basic " + basicCredentials());
                }
            }
            // Never follow token redirects: a 307/308 could replay a refresh
            // token body at a different origin, even if Authorization is stripped.
            HttpResponse<String> tokenResp = httpClient.send(tokenReqBuilder.build(),
                    HttpResponse.BodyHandlers.ofString());
            if (tokenResp.statusCode() != 200) {
                throw new IOException("Token exchange failed: HTTP " + tokenResp.statusCode());
            }
            JsonNode json = MAPPER.readTree(tokenResp.body());
            JsonNode token = json.hasNonNull("token") ? json.get("token")
                    : json.get("access_token");
            if (token == null || !token.isTextual() || token.asText().isEmpty()) {
                throw new IOException("Token exchange response did not contain a token");
            }
            bearerToken = token.asText();
        }
        // For "Basic" challenges, credentials are applied directly in authedRequest()
    }

    /** Extracts a named parameter value from a {@code Bearer} challenge string. */
    private static String extractChallenge(String challenge, String key) {
        int idx = challenge.indexOf(key + "=\"");
        if (idx < 0) return "";
        int start = idx + key.length() + 2;
        int end = challenge.indexOf('"', start);
        return end < 0 ? "" : challenge.substring(start, end);
    }

    // -----------------------------------------------------------------------
    // URI helpers
    // -----------------------------------------------------------------------

    /**
     * Returns {@code http} only for registries the {@link RegistryTrustPolicy}
     * accepts as insecure — exact loopback authorities and explicitly
     * configured insecure registries. Everything else uses HTTPS, so a
     * lookalike host such as {@code localhost.attacker.example} cannot downgrade
     * the transport that carries credentials.
     */
    private String scheme() {
        return trustPolicy.scheme(registry);
    }

    private URI registryUri(String path) {
        return URI.create(scheme() + "://" + registry + path);
    }

    /**
     * Appends the {@code ?digest=} query parameter to the upload Location URL,
     * handling whether the URL already has query parameters. A registry may
     * legitimately hand back a signed URL on storage infrastructure; such a URL
     * is followed, but {@link #authedRequest} withholds credentials from it.
     */
    private URI resolveLocation(URI base, String location, String digest) throws IOException {
        String encodedDigest = URLEncoder.encode(digest, StandardCharsets.UTF_8);
        URI resolved = resolveRegistryLocation(base, location);
        String separator = resolved.getRawQuery() == null ? "?" : "&";
        return URI.create(resolved + separator + "digest=" + encodedDigest);
    }

    private URI resolveRegistryLocation(URI base, String location) throws IOException {
        URI resolved;
        try {
            resolved = base.resolve(location);
        } catch (IllegalArgumentException e) {
            throw new IOException("Registry returned an invalid Location URL", e);
        }
        if (location.isBlank() || !resolved.isAbsolute() || resolved.getHost() == null
                || resolved.getFragment() != null
                || resolved.getUserInfo() != null
                || !("https".equalsIgnoreCase(resolved.getScheme())
                        || "http".equalsIgnoreCase(resolved.getScheme()))) {
            throw new IOException("Registry returned an unusable Location URL");
        }
        if ("http".equalsIgnoreCase(resolved.getScheme())
                && !trustPolicy.allowsPlaintext(resolved.getAuthority())) {
            throw new IOException("Registry Location uses plaintext HTTP: "
                    + resolved.getScheme() + "://" + resolved.getAuthority());
        }
        return resolved;
    }

    // -----------------------------------------------------------------------
    // Public helpers
    // -----------------------------------------------------------------------

    /**
     * Parses the registry hostname and repository from a full OCI image reference.
     * Supports {@code registry/name:tag} and {@code registry/name@digest} formats.
     *
     * @return two-element array: {@code [registry, repository]}
     */
    public static String[] splitRef(String imageRef) {
        // Remove tag or digest suffix first
        String withoutTag = imageRef.replaceFirst("[:@][^/]*$", "");
        int firstSlash = withoutTag.indexOf('/');
        if (firstSlash < 0 || !isRegistryHost(withoutTag.substring(0, firstSlash))) {
            // No explicit registry — default to docker.io. Use the tag/digest-free
            // repository so it never leaks into /v2/{repository}/... URLs.
            return new String[]{"registry-1.docker.io",
                    withoutTag.contains("/") ? withoutTag : "library/" + withoutTag};
        }
        String host = withoutTag.substring(0, firstSlash);
        String repository = withoutTag.substring(firstSlash + 1);
        if (host.equals("docker.io") || host.equals("index.docker.io")) {
            host = "registry-1.docker.io";
            if (!repository.contains("/")) {
                repository = "library/" + repository;
            }
        }
        return new String[]{host, repository};
    }

    /**
     * Validates a mutable publishing reference without requiring an explicit
     * registry (local bundle publication also uses this contract). Remote
     * application publishing separately requires {@link #hasExplicitRegistry}.
     */
    public static boolean isValidTaggedReference(String ref) {
        if (ref == null || ref.isEmpty() || ref.indexOf('@') >= 0) return false;
        int colon = ref.lastIndexOf(':');
        String name = colon > ref.lastIndexOf('/') ? ref.substring(0, colon) : ref;
        String host = "docker.io";
        String repository = name;
        int slash = name.indexOf('/');
        if (slash > 0 && isRegistryHost(name.substring(0, slash))) {
            host = name.substring(0, slash);
            repository = name.substring(slash + 1);
        }
        if (!RegistryTrustPolicy.isBareAuthority(host)) return false;
        if (host.equals("index.docker.io")) host = "docker.io";
        if (host.equals("docker.io") && !repository.contains("/")) {
            repository = "library/" + repository;
        }
        String component = "[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*";
        if (!repository.matches(component + "(?:/" + component + ")*")) return false;
        if (host.length() + 1 + repository.length() > 255) return false;
        return extractTag(ref).matches("[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}");
    }

    /**
     * Returns whether {@code imageRef} names its registry explicitly (e.g.
     * {@code myregistry.azurecr.io/app:1.0}) rather than relying on the
     * implicit Docker Hub default (e.g. {@code app:1.0} or {@code team/app}).
     */
    public static boolean hasExplicitRegistry(String imageRef) {
        if (imageRef == null) return false;
        String withoutTag = imageRef.replaceFirst("[:@][^/]*$", "");
        int firstSlash = withoutTag.indexOf('/');
        return firstSlash > 0 && isRegistryHost(withoutTag.substring(0, firstSlash));
    }

    private static boolean isRegistryHost(String component) {
        return component.contains(".") || component.contains(":") || component.equals("localhost");
    }

    /**
     * Extracts the tag or digest from a full OCI image reference.
     * Returns {@code "latest"} if no tag/digest is present.
     */
    public static String extractTag(String imageRef) {
        // Check for digest first
        int atIdx = imageRef.lastIndexOf('@');
        if (atIdx >= 0) return imageRef.substring(atIdx + 1);
        int colonIdx = imageRef.lastIndexOf(':');
        if (colonIdx >= 0 && !imageRef.substring(colonIdx).contains("/")) {
            return imageRef.substring(colonIdx + 1);
        }
        return "latest";
    }

    /** Returns whether the reference ends in a canonical sha256 digest. */
    public static boolean isDigestPinnedReference(String imageRef) {
        if (imageRef == null) return false;
        int atIdx = imageRef.lastIndexOf('@');
        return atIdx > 0 && isDigest(imageRef.substring(atIdx + 1));
    }

    /** Replaces a tag or existing digest with the supplied canonical digest. */
    public static String pinReference(String imageRef, String digest) {
        if (imageRef == null || imageRef.isBlank() || !isDigest(digest)) {
            throw new IllegalArgumentException("Image reference and canonical sha256 digest are required");
        }
        return imageRef.replaceFirst("[:@][^/]*$", "") + "@" + digest;
    }
}
