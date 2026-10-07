// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin.oci;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;
import org.junit.jupiter.api.io.TempDir;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.ValueSource;
import sh.brewlet.maven.plugin.model.DependencyBundleConfig;
import sh.brewlet.maven.plugin.model.DependencyLock;
import sh.brewlet.maven.plugin.model.Entry;
import sh.brewlet.maven.plugin.model.JvmConfig;
import sh.brewlet.maven.plugin.util.LayerBuilder;

import java.io.IOException;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.Base64;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.logging.Handler;
import java.util.logging.LogRecord;
import java.util.logging.Logger;

import static org.junit.jupiter.api.Assertions.*;
import static sh.brewlet.maven.plugin.oci.RegistryWireConformanceTest.query;

class RegistryClientMountTest {
    private static final ObjectMapper MAPPER = new ObjectMapper();
    private static final String SOURCE = "platform/approved-bundle";
    private static final String TARGET = "apps/orders";
    private static final String TARGET_SCOPE = "repository:" + TARGET + ":pull,push";
    private static final String MOUNT_SCOPE = TARGET_SCOPE + " repository:" + SOURCE + ":pull";
    private static final String UPLOADS = "/v2/" + TARGET + "/blobs/uploads/";
    private static final String BASIC = "Basic " + Base64.getEncoder()
            .encodeToString("publisher:password".getBytes(StandardCharsets.UTF_8));

    @TempDir
    Path root;

    @ParameterizedTest
    @ValueSource(strings = {"mounted", "declined-root", "declined-relative", "declined-absolute",
            "declined-foreign", "existing", "unsupported-400", "unsupported-404", "unsupported-405"})
    void publishesManagedImage(String scenario) throws Exception {
        exercise(scenario, true);
    }

    @ParameterizedTest
    @ValueSource(strings = {"mount-401", "mount-403", "mount-500", "missing-location",
            "invalid-location", "upload-401", "upload-500", "head-401", "head-403",
            "head-500", "manifest-500", "token-403"})
    void failsWithoutReportingAPublishedImage(String scenario) throws Exception {
        exercise(scenario, false);
    }

    private void exercise(String scenario, boolean success) throws Exception {
        Path dependency = Files.writeString(root.resolve("lib.jar"), "dependency");
        DependencyLock lock = new DependencyLock();
        lock.setDependencies(List.of(new DependencyLock.Entry("test", "lib", "1", "jar", null,
                "runtime", "lib.jar", LocalStore.sha256Hex(Files.readAllBytes(dependency)).substring(7))));
        DependencyBundleConfig config = new DependencyBundleConfig();
        config.setName("approved");
        config.setVersion("1");
        config.setSourceBom("test:platform:1");
        var bundle = DependencyBundle.build(config, lock, LayerBuilder.buildBundle(
                List.of(new LayerBuilder.Dep("lib.jar", dependency, false))));
        String digest = bundle.config().getLayerDigest();
        Path jar = Files.writeString(root.resolve("app.jar"), "thin application");
        JvmConfig cfg = new JvmConfig();
        cfg.setMainJar("app.jar");
        Entry entry = new Entry("classpath", List.of("app.jar", "lib/*"));
        entry.setMainClass("app.Main");
        cfg.setEntry(entry);
        var expected = RunnableImageBuilder.buildWithManagedDependencyLayer(cfg, jar,
                new RunnableImageBuilder.ManagedDependencyLayer(bundle.compressedLayer(), digest,
                        bundle.config().getLayerDiffId(), "dependencies"), null, Map.of());

        Map<String, byte[]> blobs = new ConcurrentHashMap<>();
        for (var blob : expected.blobs) {
            if (!blob.digest().equals(digest) || scenario.equals("existing")) {
                blobs.put(blob.digest(), blob.data());
            }
        }
        Map<String, byte[]> manifests = new ConcurrentHashMap<>();
        List<String> manifestOrder = new CopyOnWriteArrayList<>();
        List<String> scopes = new CopyOnWriteArrayList<>();
        List<Throwable> failures = new CopyOnWriteArrayList<>();
        AtomicInteger mounts = new AtomicInteger();
        AtomicInteger starts = new AtomicInteger();
        AtomicInteger puts = new AtomicInteger();
        AtomicInteger manifestAttempts = new AtomicInteger();
        HttpServer registry = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        HttpServer storage = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        String authority = "127.0.0.1:" + registry.getAddress().getPort();
        String origin = "http://" + authority;
        String storageOrigin = "http://127.0.0.1:" + storage.getAddress().getPort();

        com.sun.net.httpserver.HttpHandler handler = exchange -> {
            try (exchange) {
                String path = exchange.getRequestURI().getPath();
                String auth = exchange.getRequestHeaders().getFirst("Authorization");
                String method = exchange.getRequestMethod();
                Map<String, String> params = query(exchange.getRequestURI().getRawQuery());
                if (path.equals("/token")) {
                    assertEquals(BASIC, auth);
                    assertEquals("GET", method);
                    assertEquals("registry", params.get("service"));
                    scopes.add(params.get("scope"));
                    if (scenario.equals("token-403")) {
                        reply(exchange, 403);
                        return;
                    }
                    String token = params.get("scope").equals(MOUNT_SCOPE) ? "mount" : "target";
                    byte[] body = ("{\"token\":\"" + token + "\"}").getBytes(StandardCharsets.UTF_8);
                    exchange.sendResponseHeaders(200, body.length);
                    exchange.getResponseBody().write(body);
                } else if (method.equals("HEAD")) {
                    if (scenario.startsWith("head-")) {
                        reply(exchange, Integer.parseInt(scenario.substring(5)));
                    } else if (!"Bearer target".equals(auth) && !"Bearer mount".equals(auth)) {
                        challenge(exchange, origin, TARGET_SCOPE);
                    } else {
                        String requested = path.substring(path.lastIndexOf('/') + 1);
                        reply(exchange, blobs.containsKey(requested) ? 200 : 404);
                    }
                } else if (method.equals("POST") && path.equals(UPLOADS)) {
                    assertEquals(0, exchange.getRequestBody().readAllBytes().length,
                            "mount/initiation must not upload a body");
                    if (params.containsKey("mount")) {
                        assertEquals(Map.of("mount", digest, "from", SOURCE), params);
                        if (!"Bearer mount".equals(auth)) {
                            challenge(exchange, origin, MOUNT_SCOPE);
                            return;
                        }
                        mounts.incrementAndGet();
                        if (scenario.startsWith("mount-") || scenario.startsWith("unsupported-")) {
                            reply(exchange, Integer.parseInt(scenario.substring(scenario.lastIndexOf('-') + 1)));
                        } else if (scenario.equals("mounted")) {
                            blobs.put(digest, bundle.compressedLayer());
                            reply(exchange, 201);
                        } else {
                            if (!scenario.equals("missing-location")) {
                                String location = switch (scenario) {
                                    case "declined-relative" -> "mount-session?state=opaque";
                                    case "declined-absolute" -> origin + UPLOADS + "mount-session?state=opaque";
                                    case "declined-foreign" -> storageOrigin + UPLOADS + "mount-session?state=opaque";
                                    case "invalid-location" -> "file:///upload";
                                    default -> UPLOADS + "mount-session?state=opaque";
                                };
                                exchange.getResponseHeaders().set("Location", location);
                            }
                            reply(exchange, 202);
                        }
                    } else {
                        starts.incrementAndGet();
                        assertTrue(scenario.startsWith("unsupported-"),
                                "202 mount response must reuse its upload session, not start another");
                        exchange.getResponseHeaders().set("Location", UPLOADS + "fresh-session?state=opaque");
                        reply(exchange, 202);
                    }
                } else if (method.equals("PUT") && path.startsWith(UPLOADS)) {
                    puts.incrementAndGet();
                    assertEquals(scenario.startsWith("unsupported-") ? UPLOADS + "fresh-session"
                            : UPLOADS + "mount-session", path);
                    assertEquals(Map.of("state", "opaque", "digest", digest), params);
                    assertEquals(scenario.equals("declined-foreign") ? null : "Bearer mount", auth);
                    assertEquals("application/octet-stream",
                            exchange.getRequestHeaders().getFirst("Content-Type"));
                    byte[] body = exchange.getRequestBody().readAllBytes();
                    assertArrayEquals(bundle.compressedLayer(), body);
                    assertEquals(digest, LocalStore.sha256Hex(body));
                    if (scenario.startsWith("upload-")) {
                        reply(exchange, Integer.parseInt(scenario.substring(7)));
                    } else {
                        blobs.put(digest, body);
                        reply(exchange, 201);
                    }
                } else if (method.equals("PUT") && path.startsWith("/v2/" + TARGET + "/manifests/")) {
                    manifestAttempts.incrementAndGet();
                    if (scenario.equals("manifest-500")) {
                        reply(exchange, 500);
                        return;
                    }
                    String reference = path.substring(path.lastIndexOf('/') + 1);
                    byte[] body = exchange.getRequestBody().readAllBytes();
                    var manifest = MAPPER.readTree(body);
                    if (reference.equals("latest")) {
                        assertEquals(MediaTypes.OCI_INDEX_MEDIA_TYPE,
                                exchange.getRequestHeaders().getFirst("Content-Type"));
                        assertArrayEquals(expected.indexBytes, body);
                        for (var descriptor : manifest.path("manifests")) {
                            assertTrue(manifests.containsKey(descriptor.path("digest").asText()));
                        }
                    } else {
                        assertEquals(LocalStore.sha256Hex(body), reference);
                        assertEquals(MediaTypes.OCI_MANIFEST_MEDIA_TYPE,
                                exchange.getRequestHeaders().getFirst("Content-Type"));
                        assertTrue(blobs.containsKey(manifest.path("config").path("digest").asText()));
                        for (var layer : manifest.path("layers")) {
                            assertTrue(blobs.containsKey(layer.path("digest").asText()));
                        }
                    }
                    manifests.put(reference, body);
                    manifestOrder.add(reference);
                    reply(exchange, 201);
                } else {
                    fail("unexpected request: " + method + " " + path);
                }
            } catch (AssertionError e) {
                failures.add(e);
            }
        };
        registry.createContext("/", handler);
        storage.createContext("/", handler);
        registry.start();
        storage.start();
        List<String> logs = new CopyOnWriteArrayList<>();
        Logger logger = Logger.getLogger(RegistryClient.class.getName());
        Handler capture = new Handler() {
            @Override public void publish(LogRecord record) { logs.add(record.getMessage()); }
            @Override public void flush() {}
            @Override public void close() {}
        };
        logger.addHandler(capture);
        try {
            RegistryClient client = new RegistryClient(authority, TARGET, new Credential("publisher", "password"));
            if (success) {
                assertEquals(expected.indexDigest,
                        client.pushApplicationImage("latest", ApplicationImage.runnable(expected), digest, SOURCE));
                assertEquals(expected.manifests.size() + 1, manifestOrder.size());
                assertEquals("latest", manifestOrder.get(manifestOrder.size() - 1));
                assertEquals(scenario.equals("existing") ? 0 : 1, mounts.get());
                assertEquals(Set.of("mounted", "existing").contains(scenario) ? 0 : 1, puts.get());
                assertEquals(scenario.startsWith("unsupported-") ? 1 : 0, starts.get());
                assertEquals(scenario.equals("existing") ? List.of(TARGET_SCOPE)
                        : List.of(TARGET_SCOPE, MOUNT_SCOPE), scopes);
                String progress = String.join("\n", logs);
                assertTrue(progress.contains(scenario.equals("existing")
                        ? "blob " + digest + " already exists in registry (skipping upload)"
                        : "Pushing " + MediaTypes.OCI_LAYER_GZIP_MEDIA_TYPE + " blob " + digest), progress);
                assertTrue(progress.contains("Published " + MediaTypes.OCI_INDEX_MEDIA_TYPE
                        + ": " + expected.indexDigest), progress);
            } else {
                assertThrows(IOException.class,
                        () -> client.pushApplicationImage("latest", ApplicationImage.runnable(expected), digest, SOURCE));
                assertFalse(manifests.containsKey("latest"), "failure must not publish a root index");
                assertEquals(0, starts.get(), "errors must not silently restart an upload");
                assertEquals(scenario.startsWith("upload-") || scenario.equals("manifest-500") ? 1 : 0,
                        puts.get(), "failure must reach the intended upload stage, not fail earlier");
                assertEquals(scenario.equals("manifest-500") ? 1 : 0, manifestAttempts.get());
                if (scenario.startsWith("head-") || scenario.equals("token-403")) {
                    assertEquals(0, mounts.get(), "auth/check failure must stop before mounting");
                }
                assertFalse(logs.stream().anyMatch(message -> message.startsWith("Published ")), logs.toString());
            }
            assertTrue(failures.isEmpty(), failures.toString());
        } finally {
            logger.removeHandler(capture);
            registry.stop(0);
            storage.stop(0);
        }
    }

    private static void reply(HttpExchange exchange, int status) throws IOException {
        exchange.sendResponseHeaders(status, -1);
    }

    private static void challenge(HttpExchange exchange, String origin, String scope) throws IOException {
        exchange.getResponseHeaders().set("WWW-Authenticate", "Bearer realm=\"" + origin
                + "/token\",service=\"registry\",scope=\"" + scope + "\"");
        reply(exchange, 401);
    }
}
