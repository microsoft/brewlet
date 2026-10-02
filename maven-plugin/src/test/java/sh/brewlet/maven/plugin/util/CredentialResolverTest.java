// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin.util;

import org.apache.maven.settings.Server;
import org.apache.maven.settings.Settings;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;
import sh.brewlet.maven.plugin.oci.Credential;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Base64;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

class CredentialResolverTest {

    @TempDir
    Path dockerConfig;

    private final Map<String, String> env = new HashMap<>();
    private final List<String> helperCalls = new ArrayList<>();
    private final Map<String, String> helperOutput = new HashMap<>();
    private final List<String> diagnostics = new ArrayList<>();

    private Credential resolve(String registry, Settings settings) {
        env.putIfAbsent("DOCKER_CONFIG", dockerConfig.toString());
        return CredentialResolver.resolve(registry, settings, diagnostics::add, env::get,
                (helper, serverUrl) -> {
                    helperCalls.add(helper + " " + serverUrl);
                    return helperOutput.get(helper + " " + serverUrl);
                });
    }

    private void config(String json) throws IOException {
        Files.writeString(dockerConfig.resolve("config.json"), json);
    }

    private static String auth(String user, String password) {
        return Base64.getEncoder().encodeToString(
                (user + ":" + password).getBytes(StandardCharsets.UTF_8));
    }

    @Test
    void settingsServerWins() throws IOException {
        config("{\"credsStore\":\"desktop\"}");
        Settings settings = new Settings();
        Server server = new Server();
        server.setId("reg.example.com");
        server.setUsername("maven");
        server.setPassword("pw");
        settings.addServer(server);

        Credential credential = resolve("reg.example.com", settings);

        assertEquals("maven", credential.getUsername());
        assertTrue(helperCalls.isEmpty());
    }

    @Test
    void inlineAuthIsDecoded() throws IOException {
        config("{\"auths\":{\"reg.example.com\":{\"auth\":\"" + auth("user", "p:w") + "\"}}}");

        Credential credential = resolve("reg.example.com", null);

        assertEquals("user", credential.getUsername());
        assertEquals("p:w", credential.getPassword());
        assertFalse(credential.isIdentityToken());
    }

    @Test
    void inlineIdentityTokenIsUsedAsRefreshToken() throws IOException {
        config("{\"auths\":{\"myacr.azurecr.io\":{\"auth\":\"\",\"identitytoken\":\"refresh\"}}}");

        Credential credential = resolve("myacr.azurecr.io", null);

        assertTrue(credential.isIdentityToken());
        assertEquals("refresh", credential.getPassword());
    }

    @Test
    void credsStoreHelperIsQueriedWithTheRegistry() throws IOException {
        config("{\"auths\":{\"myacr.azurecr.io\":{}},\"credsStore\":\"osxkeychain\"}");
        helperOutput.put("osxkeychain myacr.azurecr.io",
                "{\"ServerURL\":\"myacr.azurecr.io\",\"Username\":\"00000000-0000-0000-0000-000000000000\","
                        + "\"Secret\":\"refresh\"}");

        Credential credential = resolve("myacr.azurecr.io", null);

        assertEquals(List.of("osxkeychain myacr.azurecr.io"), helperCalls);
        assertEquals("00000000-0000-0000-0000-000000000000", credential.getUsername());
        assertEquals("refresh", credential.getPassword());
        assertFalse(credential.isIdentityToken());
    }

    @Test
    void helperTokenUsernameMeansIdentityToken() throws IOException {
        config("{\"credsStore\":\"desktop\"}");
        helperOutput.put("desktop reg.example.com", "{\"Username\":\"<token>\",\"Secret\":\"refresh\"}");

        Credential credential = resolve("reg.example.com", null);

        assertTrue(credential.isIdentityToken());
        assertEquals("refresh", credential.getPassword());
    }

    @Test
    void perRegistryCredHelperTakesPrecedenceOverInlineAuthAndCredsStore() throws IOException {
        config("{\"auths\":{\"reg.example.com\":{\"auth\":\"" + auth("inline", "x") + "\"}},"
                + "\"credHelpers\":{\"reg.example.com\":\"ecr-login\"},\"credsStore\":\"desktop\"}");
        helperOutput.put("ecr-login reg.example.com", "{\"Username\":\"AWS\",\"Secret\":\"pw\"}");

        Credential credential = resolve("reg.example.com", null);

        assertEquals("AWS", credential.getUsername());
        assertEquals(List.of("ecr-login reg.example.com"), helperCalls);
    }

    @Test
    void dockerHubUsesTheLegacyIndexKey() throws IOException {
        config("{\"credsStore\":\"desktop\"}");
        helperOutput.put("desktop https://index.docker.io/v1/", "{\"Username\":\"me\",\"Secret\":\"pw\"}");

        Credential credential = resolve("registry-1.docker.io", null);

        assertEquals("me", credential.getUsername());
    }

    @Test
    void authsKeysMustMatchTheRegistryExactly() throws IOException {
        config("{\"auths\":{\"https://evil-reg.example.com.attacker.example/v1/\":{\"auth\":\""
                + auth("user", "pw") + "\"}}}");

        assertNull(resolve("reg.example.com", null));
    }

    @Test
    void urlStyleAuthsKeysMatchByAuthority() throws IOException {
        config("{\"auths\":{\"https://reg.example.com/v1/\":{\"auth\":\"" + auth("user", "pw") + "\"}}}");

        assertEquals("user", resolve("reg.example.com", null).getUsername());
    }

    @Test
    void missingHelperCredentialsFallBackToEnvironment() throws IOException {
        config("{\"credsStore\":\"desktop\"}");
        env.put("BREWLET_REGISTRY_USERNAME", "ci");
        env.put("BREWLET_REGISTRY_PASSWORD", "secret");

        Credential credential = resolve("reg.example.com", null);

        assertEquals("ci", credential.getUsername());
        assertEquals(List.of("desktop reg.example.com"), helperCalls);
    }

    @Test
    void helperFailureIsReportedWithoutSecrets() throws IOException {
        config("{\"credsStore\":\"broken\"}");
        Credential credential = CredentialResolver.resolve("reg.example.com", null, diagnostics::add,
                name -> "DOCKER_CONFIG".equals(name) ? dockerConfig.toString() : null,
                (helper, serverUrl) -> {
                    throw new IOException("Cannot run program \"docker-credential-broken\"");
                });

        assertNull(credential);
        assertEquals(1, diagnostics.size());
        assertTrue(diagnostics.get(0).contains("docker-credential-broken"), diagnostics.get(0));
    }

    @Test
    void invalidHelperNamesAreNeverExecuted() throws IOException {
        config("{\"credsStore\":\"../../bin/sh\"}");

        assertNull(resolve("reg.example.com", null));
        assertTrue(helperCalls.isEmpty());
    }

    @Test
    void toStringNeverRevealsSecrets() {
        assertFalse(Credential.identityToken("refresh-secret").toString().contains("refresh-secret"));
    }
}
