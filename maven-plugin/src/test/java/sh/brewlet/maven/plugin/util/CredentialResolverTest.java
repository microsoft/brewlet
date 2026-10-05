// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin.util;

import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.settings.Server;
import org.apache.maven.settings.Settings;
import org.apache.maven.settings.crypto.DefaultSettingsDecrypter;
import org.apache.maven.settings.crypto.SettingsDecrypter;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;
import org.sonatype.plexus.components.cipher.DefaultPlexusCipher;
import org.sonatype.plexus.components.sec.dispatcher.DefaultSecDispatcher;
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
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

class CredentialResolverTest {

    @TempDir
    Path dockerConfig;

    private final Map<String, String> env = new HashMap<>();
    private final List<String> helperCalls = new ArrayList<>();
    private final Map<String, String> helperOutput = new HashMap<>();
    private final List<String> diagnostics = new ArrayList<>();

    private SettingsDecrypter decrypter() {
        return new DefaultSettingsDecrypter(new DefaultSecDispatcher(
                new DefaultPlexusCipher(), Map.of(),
                dockerConfig.resolve("settings-security.xml").toString()));
    }

    private Credential resolve(String registry, Settings settings) throws MojoExecutionException {
        env.putIfAbsent("DOCKER_CONFIG", dockerConfig.toString());
        return CredentialResolver.resolve(registry, settings, decrypter(), diagnostics::add, env::get,
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
    void settingsServerWins() throws Exception {
        config("{\"credsStore\":\"desktop\"}");
        Settings settings = new Settings();
        Server server = new Server();
        server.setId("reg.example.com");
        server.setUsername("maven");
        server.setPassword("pw");
        settings.addServer(server);

        Credential credential = resolve("reg.example.com", settings);

        assertEquals("maven", credential.getUsername());
        assertEquals("pw", credential.getPassword());
        assertTrue(helperCalls.isEmpty());
    }

    @Test
    void encryptedSettingsPasswordIsDecryptedWithoutMutatingSettings() throws Exception {
        DefaultPlexusCipher cipher = new DefaultPlexusCipher();
        String encryptedMaster = cipher.encryptAndDecorate("test-master", "settings.security");
        Files.writeString(dockerConfig.resolve("settings-security.xml"),
                "<settingsSecurity><master>" + encryptedMaster + "</master></settingsSecurity>");
        String encryptedPassword = cipher.encryptAndDecorate("registry-secret", "test-master");
        Settings settings = settings(encryptedPassword);
        config("{\"credsStore\":\"desktop\"}");
        env.put("BREWLET_REGISTRY_USERNAME", "ci");

        Credential credential = resolve("reg.example.com", settings);

        assertEquals("maven", credential.getUsername());
        assertEquals("registry-secret", credential.getPassword());
        assertEquals(encryptedPassword, settings.getServer("reg.example.com").getPassword());
        assertTrue(helperCalls.isEmpty());
        assertTrue(diagnostics.isEmpty());
    }

    @Test
    void missingMasterPasswordFailsWithoutFallbackOrSecretDisclosure() throws Exception {
        String encryptedPassword = new DefaultPlexusCipher()
                .encryptAndDecorate("registry-secret", "test-master");
        assertDecryptionFailure(encryptedPassword);
    }

    @Test
    void invalidEncryptedPasswordFailsWithoutFallbackOrSecretDisclosure() throws Exception {
        assertDecryptionFailure("{not-valid-ciphertext}");
    }

    private void assertDecryptionFailure(String encryptedPassword) throws Exception {
        config("{\"credsStore\":\"desktop\"}");
        env.put("BREWLET_REGISTRY_USERNAME", "ci");
        env.put("BREWLET_REGISTRY_PASSWORD", "ci-secret");

        MojoExecutionException error = assertThrows(MojoExecutionException.class,
                () -> resolve("reg.example.com", settings(encryptedPassword)));

        assertTrue(error.getMessage().contains("reg.example.com"));
        assertTrue(error.getMessage().contains("settings-security.xml"));
        assertFalse(error.getMessage().contains(encryptedPassword));
        assertFalse(error.getMessage().contains("registry-secret"));
        assertNull(error.getCause());
        assertTrue(helperCalls.isEmpty());
        assertTrue(diagnostics.isEmpty());
    }

    @Test
    void unrelatedEncryptedServerIsNotDecrypted() throws Exception {
        config("{\"auths\":{\"other.example.com\":{\"auth\":\"" + auth("docker", "pw") + "\"}}}");

        Credential credential = resolve("other.example.com", settings("{invalid}"));

        assertEquals("docker", credential.getUsername());
        assertEquals("pw", credential.getPassword());
    }

    @Test
    void missingSettingsDecrypterFailsExplicitly() {
        MojoExecutionException error = assertThrows(MojoExecutionException.class,
                () -> CredentialResolver.resolve("reg.example.com", settings("pw"), null));

        assertTrue(error.getMessage().contains("SettingsDecrypter is unavailable"));
    }

    private static Settings settings(String password) {
        Settings settings = new Settings();
        Server server = new Server();
        server.setId("reg.example.com");
        server.setUsername("maven");
        server.setPassword(password);
        settings.addServer(server);
        return settings;
    }

    @Test
    void inlineAuthIsDecoded() throws Exception {
        config("{\"auths\":{\"reg.example.com\":{\"auth\":\"" + auth("user", "p:w") + "\"}}}");

        Credential credential = resolve("reg.example.com", null);

        assertEquals("user", credential.getUsername());
        assertEquals("p:w", credential.getPassword());
        assertFalse(credential.isIdentityToken());
    }

    @Test
    void inlineIdentityTokenIsUsedAsRefreshToken() throws Exception {
        config("{\"auths\":{\"myacr.azurecr.io\":{\"auth\":\"\",\"identitytoken\":\"refresh\"}}}");

        Credential credential = resolve("myacr.azurecr.io", null);

        assertTrue(credential.isIdentityToken());
        assertEquals("refresh", credential.getPassword());
    }

    @Test
    void credsStoreHelperIsQueriedWithTheRegistry() throws Exception {
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
    void helperTokenUsernameMeansIdentityToken() throws Exception {
        config("{\"credsStore\":\"desktop\"}");
        helperOutput.put("desktop reg.example.com", "{\"Username\":\"<token>\",\"Secret\":\"refresh\"}");

        Credential credential = resolve("reg.example.com", null);

        assertTrue(credential.isIdentityToken());
        assertEquals("refresh", credential.getPassword());
    }

    @Test
    void perRegistryCredHelperTakesPrecedenceOverInlineAuthAndCredsStore() throws Exception {
        config("{\"auths\":{\"reg.example.com\":{\"auth\":\"" + auth("inline", "x") + "\"}},"
                + "\"credHelpers\":{\"reg.example.com\":\"ecr-login\"},\"credsStore\":\"desktop\"}");
        helperOutput.put("ecr-login reg.example.com", "{\"Username\":\"AWS\",\"Secret\":\"pw\"}");

        Credential credential = resolve("reg.example.com", null);

        assertEquals("AWS", credential.getUsername());
        assertEquals(List.of("ecr-login reg.example.com"), helperCalls);
    }

    @Test
    void dockerHubUsesTheIndexDockerIoV1CredentialKey() throws Exception {
        config("{\"credsStore\":\"desktop\"}");
        helperOutput.put("desktop https://index.docker.io/v1/", "{\"Username\":\"me\",\"Secret\":\"pw\"}");

        Credential credential = resolve("registry-1.docker.io", null);

        assertEquals("me", credential.getUsername());
    }

    @Test
    void authsKeysMustMatchTheRegistryExactly() throws Exception {
        config("{\"auths\":{\"https://evil-reg.example.com.attacker.example/v1/\":{\"auth\":\""
                + auth("user", "pw") + "\"}}}");

        assertNull(resolve("reg.example.com", null));
    }

    @Test
    void urlStyleAuthsKeysMatchByAuthority() throws Exception {
        config("{\"auths\":{\"https://reg.example.com/v1/\":{\"auth\":\"" + auth("user", "pw") + "\"}}}");

        assertEquals("user", resolve("reg.example.com", null).getUsername());
    }

    @Test
    void missingHelperCredentialsFallBackToEnvironment() throws Exception {
        config("{\"credsStore\":\"desktop\"}");
        env.put("BREWLET_REGISTRY_USERNAME", "ci");
        env.put("BREWLET_REGISTRY_PASSWORD", "secret");

        Credential credential = resolve("reg.example.com", null);

        assertEquals("ci", credential.getUsername());
        assertEquals(List.of("desktop reg.example.com"), helperCalls);
    }

    @Test
    void helperFailureIsReportedWithoutSecrets() throws Exception {
        config("{\"credsStore\":\"broken\"}");
        Credential credential = CredentialResolver.resolve("reg.example.com", null, decrypter(), diagnostics::add,
                name -> "DOCKER_CONFIG".equals(name) ? dockerConfig.toString() : null,
                (helper, serverUrl) -> {
                    throw new IOException("Cannot run program \"docker-credential-broken\"");
                });

        assertNull(credential);
        assertEquals(1, diagnostics.size());
        assertTrue(diagnostics.get(0).contains("docker-credential-broken"), diagnostics.get(0));
    }

    @Test
    void invalidHelperNamesAreNeverExecuted() throws Exception {
        config("{\"credsStore\":\"../../bin/sh\"}");

        assertNull(resolve("reg.example.com", null));
        assertTrue(helperCalls.isEmpty());
    }

    @Test
    void toStringNeverRevealsSecrets() {
        assertFalse(Credential.identityToken("refresh-secret").toString().contains("refresh-secret"));
    }
}
