// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin.oci;

import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.Test;

import java.io.IOException;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.util.List;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicReference;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * Credential-forwarding tests: a malicious or compromised registry controls the
 * authentication challenge realm and the blob-upload {@code Location}, so no
 * request built from those values may carry registry credentials unless the
 * target origin is explicitly trusted.
 */
class RegistryClientAuthTest {

    private static final String MANIFEST = "{\"schemaVersion\":2}";

    private HttpServer registryServer;
    private HttpServer foreignServer;

    /** Requests observed by the second (foreign-origin) server. */
    private final AtomicInteger foreignRequests = new AtomicInteger();
    private final AtomicReference<String> foreignAuthorization = new AtomicReference<>();

    @AfterEach
    void stopServers() {
        if (registryServer != null) {
            registryServer.stop(0);
        }
        if (foreignServer != null) {
            foreignServer.stop(0);
        }
    }

    @Test
    void refusesToSendCredentialsToACrossOriginTokenRealm() throws Exception {
        foreignServer = server("127.0.0.1");
        foreignServer.createContext("/token", exchange -> {
            recordForeign(exchange);
            respond(exchange, 200, "{\"token\":\"stolen\"}", null);
        });
        foreignServer.start();

        registryServer = server("localhost");
        registryServer.createContext("/v2/repo/manifests/latest", exchange ->
                respond(exchange, 401, "{}", bearerChallenge(foreignOrigin() + "/token")));
        registryServer.start();

        RegistryClient client = client(new Credential("developer", "s3cret"),
                RegistryTrustPolicy.secureDefault());

        IOException failure = assertThrows(IOException.class,
                () -> client.pullManifest("latest"));

        assertTrue(failure.getMessage().contains("allowedTokenRealms"),
                "error should name the opt-in option: " + failure.getMessage());
        assertEquals(0, foreignRequests.get(),
                "no request may be sent to an untrusted token realm");
        assertNull(foreignAuthorization.get());
    }

    @Test
    void exchangesCredentialsWithASameOriginTokenRealm() throws Exception {
        AtomicReference<String> realmAuthorization = new AtomicReference<>();
        AtomicReference<String> manifestAuthorization = new AtomicReference<>();
        AtomicInteger manifestRequests = new AtomicInteger();

        registryServer = server("localhost");
        registryServer.createContext("/token", exchange -> {
            realmAuthorization.set(exchange.getRequestHeaders().getFirst("Authorization"));
            respond(exchange, 200, "{\"token\":\"issued-token\"}", null);
        });
        registryServer.createContext("/v2/repo/manifests/latest", exchange -> {
            if (manifestRequests.incrementAndGet() == 1) {
                respond(exchange, 401, "{}", bearerChallenge(registryOrigin() + "/token"));
                return;
            }
            manifestAuthorization.set(exchange.getRequestHeaders().getFirst("Authorization"));
            respond(exchange, 200, MANIFEST, null);
        });
        registryServer.start();

        RegistryClient client = client(new Credential("developer", "s3cret"),
                RegistryTrustPolicy.secureDefault());

        assertEquals(MANIFEST, new String(client.pullManifest("latest"), StandardCharsets.UTF_8));
        assertEquals("Basic " + basic("developer", "s3cret"), realmAuthorization.get());
        assertEquals("Bearer issued-token", manifestAuthorization.get());
    }

    @Test
    void sendsCredentialsToAnExplicitlyAllowlistedCrossOriginRealm() throws Exception {
        foreignServer = server("127.0.0.1");
        foreignServer.createContext("/token", exchange -> {
            recordForeign(exchange);
            respond(exchange, 200, "{\"token\":\"issued-token\"}", null);
        });
        foreignServer.start();

        AtomicInteger manifestRequests = new AtomicInteger();
        registryServer = server("localhost");
        registryServer.createContext("/v2/repo/manifests/latest", exchange -> {
            if (manifestRequests.incrementAndGet() == 1) {
                respond(exchange, 401, "{}", bearerChallenge(foreignOrigin() + "/token"));
                return;
            }
            respond(exchange, 200, MANIFEST, null);
        });
        registryServer.start();

        RegistryClient client = client(new Credential("developer", "s3cret"),
                RegistryTrustPolicy.of(List.of(), List.of(foreignAuthority())));

        assertEquals(MANIFEST, new String(client.pullManifest("latest"), StandardCharsets.UTF_8));
        assertEquals(1, foreignRequests.get());
        assertEquals("Basic " + basic("developer", "s3cret"), foreignAuthorization.get());
    }

    @Test
    void completesAnAnonymousCrossOriginTokenExchangeWithoutCredentials() throws Exception {
        foreignServer = server("127.0.0.1");
        foreignServer.createContext("/token", exchange -> {
            recordForeign(exchange);
            respond(exchange, 200, "{\"token\":\"anonymous-token\"}", null);
        });
        foreignServer.start();

        AtomicInteger manifestRequests = new AtomicInteger();
        registryServer = server("localhost");
        registryServer.createContext("/v2/repo/manifests/latest", exchange -> {
            if (manifestRequests.incrementAndGet() == 1) {
                respond(exchange, 401, "{}", bearerChallenge(foreignOrigin() + "/token"));
                return;
            }
            respond(exchange, 200, MANIFEST, null);
        });
        registryServer.start();

        RegistryClient client = client(null, RegistryTrustPolicy.secureDefault());

        assertEquals(MANIFEST, new String(client.pullManifest("latest"), StandardCharsets.UTF_8));
        assertEquals(1, foreignRequests.get());
        assertNull(foreignAuthorization.get(), "anonymous flows must not gain credentials");
    }

    @Test
    void doesNotCredentialACrossOriginBlobUploadLocation() throws Exception {
        byte[] blob = "brewlet".getBytes(StandardCharsets.UTF_8);
        String digest = LocalStore.sha256Hex(blob);

        foreignServer = server("127.0.0.1");
        foreignServer.createContext("/upload", exchange -> {
            recordForeign(exchange);
            respond(exchange, 201, "", null);
        });
        foreignServer.start();

        AtomicReference<String> initiateAuthorization = new AtomicReference<>();
        registryServer = server("localhost");
        registryServer.createContext("/v2/repo/blobs/uploads/", exchange -> {
            initiateAuthorization.set(exchange.getRequestHeaders().getFirst("Authorization"));
            exchange.getResponseHeaders().set("Location", foreignOrigin() + "/upload");
            respond(exchange, 202, "", null);
        });
        registryServer.start();

        RegistryClient client = client(new Credential("developer", "s3cret"),
                RegistryTrustPolicy.secureDefault());
        client.pushBlob(digest, blob);

        assertNotNull(initiateAuthorization.get(),
                "the registry's own origin still receives credentials");
        assertEquals(1, foreignRequests.get());
        assertNull(foreignAuthorization.get(),
                "a registry-supplied cross-origin upload URL must not receive credentials");
    }

    @Test
    void rejectsAPlaintextTokenRealmOnANonLoopbackHost() throws Exception {
        registryServer = server("localhost");
        registryServer.createContext("/v2/repo/manifests/latest", exchange ->
                respond(exchange, 401, "{}",
                        bearerChallenge("http://registry.attacker.example/token")));
        registryServer.start();

        RegistryClient client = client(new Credential("developer", "s3cret"),
                RegistryTrustPolicy.secureDefault());

        IOException failure = assertThrows(IOException.class,
                () -> client.pullManifest("latest"));
        assertTrue(failure.getMessage().contains("plaintext HTTP"), failure.getMessage());
    }

    @Test
    void rejectsRegistryAuthoritiesThatCouldRetargetRequests() {
        assertThrows(IllegalArgumentException.class,
                () -> new RegistryClient("attacker.example@registry.example.com", "repo", null));
        assertThrows(IllegalArgumentException.class,
                () -> new RegistryClient("https://registry.example.com", "repo", null));
    }

    private RegistryClient client(Credential credential, RegistryTrustPolicy policy) {
        return new RegistryClient("localhost:" + registryServer.getAddress().getPort(),
                "repo", credential, policy);
    }

    private static HttpServer server(String host) throws IOException {
        return HttpServer.create(new InetSocketAddress(host, 0), 0);
    }

    private String registryOrigin() {
        return "http://localhost:" + registryServer.getAddress().getPort();
    }

    private String foreignOrigin() {
        return "http://" + foreignAuthority();
    }

    private String foreignAuthority() {
        return "127.0.0.1:" + foreignServer.getAddress().getPort();
    }

    private void recordForeign(HttpExchange exchange) {
        foreignRequests.incrementAndGet();
        foreignAuthorization.compareAndSet(null,
                exchange.getRequestHeaders().getFirst("Authorization"));
    }

    private static String bearerChallenge(String realm) {
        return "Bearer realm=\"" + realm + "\",service=\"registry\","
                + "scope=\"repository:repo:pull\"";
    }

    private static String basic(String username, String password) {
        return java.util.Base64.getEncoder().encodeToString(
                (username + ":" + password).getBytes(StandardCharsets.UTF_8));
    }

    private static void respond(HttpExchange exchange, int status, String body,
                                String challenge) throws IOException {
        byte[] bytes = body.getBytes(StandardCharsets.UTF_8);
        exchange.getResponseHeaders().set("Content-Type", "application/json");
        if (challenge != null) {
            exchange.getResponseHeaders().set("WWW-Authenticate", challenge);
        }
        exchange.sendResponseHeaders(status, bytes.length == 0 ? -1 : bytes.length);
        if (bytes.length > 0) {
            exchange.getResponseBody().write(bytes);
        }
        exchange.close();
    }
}
