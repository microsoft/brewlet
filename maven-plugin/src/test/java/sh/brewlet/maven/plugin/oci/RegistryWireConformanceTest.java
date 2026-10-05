// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin.oci;

import com.sun.net.httpserver.HttpServer;
import org.junit.jupiter.api.DynamicTest;
import org.junit.jupiter.api.TestFactory;

import java.net.InetSocketAddress;
import java.net.URLDecoder;
import java.nio.charset.StandardCharsets;
import java.util.Base64;
import java.util.List;
import java.util.Map;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.stream.Collectors;
import java.util.stream.Stream;

import static org.junit.jupiter.api.Assertions.*;
import static sh.brewlet.maven.plugin.RegistryConformanceFixtures.cases;

class RegistryWireConformanceTest {
    private static final byte[] MANIFEST = "{\"schemaVersion\":2}".getBytes(StandardCharsets.UTF_8);
    private static final String SCOPE = "repository:team/app:pull,push";
    private static final String SECRET = "refresh/+==";
    private static final String BASIC = "Basic " + Base64.getEncoder()
            .encodeToString(("user:" + SECRET).getBytes(StandardCharsets.UTF_8));

    @TestFactory
    Stream<DynamicTest> wire() throws Exception {
        return cases("wire").map(tc -> DynamicTest.dynamicTest(tc.path("id").asText(), () -> {
            boolean identity = tc.path("identity").asBoolean();
            boolean foreign = tc.path("foreign").asBoolean();
            boolean success = tc.path("success").asBoolean();
            String operation = tc.path("operation").asText();
            HttpServer registry = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
            HttpServer other = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
            String origin = "http://127.0.0.1:" + registry.getAddress().getPort();
            String foreignOrigin = "http://127.0.0.1:" + other.getAddress().getPort();
            AtomicInteger realmCalls = new AtomicInteger();
            AtomicInteger landingCalls = new AtomicInteger();
            AtomicInteger manifestCalls = new AtomicInteger();
            List<Throwable> failures = new CopyOnWriteArrayList<>();
            com.sun.net.httpserver.HttpHandler handler = exchange -> {
                try (exchange) {
                    String authorization = exchange.getRequestHeaders().getFirst("Authorization");
                    String path = exchange.getRequestURI().getPath();
                    if (path.equals("/token")) {
                        realmCalls.incrementAndGet();
                        String form = identity
                                ? new String(exchange.getRequestBody().readAllBytes(), StandardCharsets.UTF_8)
                                : exchange.getRequestURI().getRawQuery();
                        Map<String, String> params = query(form);
                        assertEquals(SCOPE, params.get("scope"));
                        assertEquals("registry", params.get("service"));
                        if (identity) {
                            assertEquals("POST", exchange.getRequestMethod());
                            assertNull(authorization);
                            assertEquals("refresh_token", params.get("grant_type"));
                            assertEquals(SECRET, params.get("refresh_token"));
                        } else {
                            assertEquals("GET", exchange.getRequestMethod());
                            assertEquals(BASIC, authorization);
                        }
                        if (operation.equals("tokenRedirect")) {
                            exchange.getResponseHeaders().set("Location", foreignOrigin + "/landing");
                            exchange.sendResponseHeaders(307, -1);
                        } else {
                            byte[] token = "{\"token\":\"issued\"}".getBytes(StandardCharsets.UTF_8);
                            exchange.sendResponseHeaders(200, token.length);
                            exchange.getResponseBody().write(token);
                        }
                    } else if (path.equals("/landing")) {
                        landingCalls.incrementAndGet();
                        assertEquals(foreign ? null : BASIC, authorization);
                        assertEquals("PUT", exchange.getRequestMethod());
                        assertArrayEquals(MANIFEST, exchange.getRequestBody().readAllBytes());
                        assertEquals(MediaTypes.OCI_INDEX_MEDIA_TYPE,
                                exchange.getRequestHeaders().getFirst("Content-Type"));
                        exchange.sendResponseHeaders(201, -1);
                    } else {
                        manifestCalls.incrementAndGet();
                        if (operation.equals("redirect")) {
                            exchange.getResponseHeaders().set("Location",
                                    (foreign ? foreignOrigin : origin) + "/landing");
                            exchange.sendResponseHeaders(307, -1);
                        } else if ("Bearer issued".equals(authorization)) {
                            exchange.sendResponseHeaders(201, -1);
                        } else {
                            if (identity) assertNull(authorization);
                            String realm = operation.equals("realm") && foreign ? foreignOrigin : origin;
                            exchange.getResponseHeaders().set("WWW-Authenticate",
                                    "Bearer realm=\"" + realm + "/token\",service=\"registry\",scope=\"" + SCOPE + "\"");
                            exchange.sendResponseHeaders(401, -1);
                        }
                    }
                } catch (AssertionError e) {
                    failures.add(e);
                }
            };
            registry.createContext("/", handler);
            other.createContext("/", handler);
            registry.start();
            other.start();
            try {
                var policy = RegistryTrustPolicy.of(List.of(), tc.path("allowed").asBoolean()
                        ? List.of(other.getAddress().getHostString() + ":" + other.getAddress().getPort())
                        : List.of());
                var client = new RegistryClient("127.0.0.1:" + registry.getAddress().getPort(),
                        "team/app", identity ? Credential.identityToken(SECRET) : new Credential("user", SECRET),
                        policy);
                if (success) {
                    assertDoesNotThrow(() -> client.pushManifest("latest", MANIFEST, MediaTypes.OCI_INDEX_MEDIA_TYPE));
                } else {
                    assertThrows(java.io.IOException.class,
                            () -> client.pushManifest("latest", MANIFEST, MediaTypes.OCI_INDEX_MEDIA_TYPE));
                }
                assertTrue(failures.isEmpty(), failures.toString());
                if (operation.equals("redirect")) {
                    assertEquals(1, landingCalls.get());
                    assertEquals(1, manifestCalls.get());
                    assertEquals(0, realmCalls.get());
                } else {
                    assertEquals(success ? 2 : 1, manifestCalls.get());
                    assertEquals(!success && operation.equals("realm") ? 0 : 1, realmCalls.get());
                    assertEquals(0, landingCalls.get(), "token exchanges must not follow redirects");
                }
            } finally {
                registry.stop(0);
                other.stop(0);
            }
        }));
    }

    static Map<String, String> query(String query) {
        if (query == null || query.isEmpty()) return Map.of();
        return Stream.of(query.split("&")).map(part -> part.split("=", 2))
                .collect(Collectors.toMap(part -> URLDecoder.decode(part[0], StandardCharsets.UTF_8),
                        part -> URLDecoder.decode(part[1], StandardCharsets.UTF_8)));
    }
}
