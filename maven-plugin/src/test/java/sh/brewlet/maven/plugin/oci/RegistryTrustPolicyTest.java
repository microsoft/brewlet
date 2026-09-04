// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin.oci;

import org.junit.jupiter.api.Test;

import java.net.URI;
import java.util.List;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

class RegistryTrustPolicyTest {

    private static final RegistryTrustPolicy SECURE = RegistryTrustPolicy.secureDefault();

    @Test
    void treatsOnlyExactLoopbackAuthoritiesAsInsecure() {
        assertEquals("http", SECURE.scheme("localhost"));
        assertEquals("http", SECURE.scheme("localhost:5000"));
        assertEquals("http", SECURE.scheme("LOCALHOST:5000"));
        assertEquals("http", SECURE.scheme("127.0.0.1"));
        assertEquals("http", SECURE.scheme("127.0.0.1:5000"));
        assertEquals("http", SECURE.scheme("127.1.2.3:5000"));
        assertEquals("http", SECURE.scheme("[::1]"));
        assertEquals("http", SECURE.scheme("[::1]:5000"));
    }

    @Test
    void keepsLookalikeHostsOnHttps() {
        assertEquals("https", SECURE.scheme("localhost.attacker.example"));
        assertEquals("https", SECURE.scheme("localhost.attacker.example:5000"));
        assertEquals("https", SECURE.scheme("notlocalhost"));
        assertEquals("https", SECURE.scheme("mylocalhost:5000"));
        assertEquals("https", SECURE.scheme("127.example.com"));
        assertEquals("https", SECURE.scheme("127.0.0.1.attacker.example"));
        assertEquals("https", SECURE.scheme("1270.0.0.1"));
        assertEquals("https", SECURE.scheme("registry.example.com"));
        assertEquals("https", SECURE.scheme("registry-1.docker.io"));
    }

    @Test
    void allowsPlaintextOnlyForExactlyConfiguredAuthorities() {
        RegistryTrustPolicy policy = RegistryTrustPolicy.of(
                List.of("registry.internal:5000"), List.of());

        assertEquals("http", policy.scheme("registry.internal:5000"));
        assertEquals("https", policy.scheme("registry.internal"));
        assertEquals("https", policy.scheme("registry.internal:5001"));
        assertEquals("https", policy.scheme("evil-registry.internal:5000"));
        assertEquals("https", policy.scheme("registry.internal:5000.attacker.example"));
    }

    @Test
    void rejectsMalformedRegistryAuthorities() {
        assertFalse(RegistryTrustPolicy.isBareAuthority("evil.example@real.example"));
        assertFalse(RegistryTrustPolicy.isBareAuthority("registry.example.com/path"));
        assertFalse(RegistryTrustPolicy.isBareAuthority("https://registry.example.com"));
        assertFalse(RegistryTrustPolicy.isBareAuthority("registry.example.com:notaport"));
        assertFalse(RegistryTrustPolicy.isBareAuthority("registry example.com"));
        assertFalse(RegistryTrustPolicy.isBareAuthority(""));
        assertFalse(RegistryTrustPolicy.isBareAuthority(null));

        assertTrue(RegistryTrustPolicy.isBareAuthority("registry.example.com"));
        assertTrue(RegistryTrustPolicy.isBareAuthority("registry.example.com:5000"));
        assertTrue(RegistryTrustPolicy.isBareAuthority("[::1]:5000"));
    }

    @Test
    void rejectsMalformedConfigurationEntries() {
        assertThrows(IllegalArgumentException.class,
                () -> RegistryTrustPolicy.of(List.of("http://registry.internal"), List.of()));
        assertThrows(IllegalArgumentException.class,
                () -> RegistryTrustPolicy.of(List.of(), List.of("*.docker.io")));
        assertThrows(IllegalArgumentException.class,
                () -> RegistryTrustPolicy.of(List.of(), List.of("auth.example.com/token")));
    }

    @Test
    void requiresAbsoluteHttpsRealms() {
        assertThrows(IllegalArgumentException.class, () -> SECURE.validateTokenRealm(""));
        assertThrows(IllegalArgumentException.class, () -> SECURE.validateTokenRealm("/token"));
        assertThrows(IllegalArgumentException.class,
                () -> SECURE.validateTokenRealm("file:///etc/passwd"));
        assertThrows(IllegalArgumentException.class,
                () -> SECURE.validateTokenRealm("https://user:pass@auth.example.com/token"));
        assertThrows(IllegalArgumentException.class,
                () -> SECURE.validateTokenRealm("http://registry.example.com/token"));

        assertEquals("auth.example.com",
                SECURE.validateTokenRealm("https://auth.example.com/token").getHost());
        assertEquals("127.0.0.1",
                SECURE.validateTokenRealm("http://127.0.0.1:5000/token").getHost());
    }

    @Test
    void allowsPlaintextRealmOnlyForInsecureEligibleAuthorities() {
        RegistryTrustPolicy policy = RegistryTrustPolicy.of(
                List.of("registry.internal:5000"), List.of());

        assertEquals("registry.internal",
                policy.validateTokenRealm("http://registry.internal:5000/token").getHost());
        assertThrows(IllegalArgumentException.class,
                () -> policy.validateTokenRealm("http://registry.internal:5001/token"));
    }

    @Test
    void sendsCredentialsToSameOriginRealmsOnly() {
        URI registry = URI.create("https://registry.example.com/");

        assertTrue(SECURE.allowsCredentials(registry,
                URI.create("https://registry.example.com/v2/token")));
        assertTrue(SECURE.allowsCredentials(registry,
                URI.create("https://registry.example.com:443/v2/token")));
        assertFalse(SECURE.allowsCredentials(registry,
                URI.create("https://auth.attacker.example/token")));
        assertFalse(SECURE.allowsCredentials(registry,
                URI.create("https://registry.example.com:8443/token")));
        assertFalse(SECURE.allowsCredentials(registry,
                URI.create("http://registry.example.com/token")));
    }

    @Test
    void trustsTheBuiltInDockerHubRealmOnlyForDockerHubRegistries() {
        assertTrue(SECURE.allowsCredentials(URI.create("https://registry-1.docker.io/"),
                URI.create("https://auth.docker.io/token")));
        assertTrue(SECURE.allowsCredentials(URI.create("https://index.docker.io/"),
                URI.create("https://auth.docker.io/token")));

        assertFalse(SECURE.allowsCredentials(URI.create("https://registry-1.docker.io/"),
                URI.create("https://auth.docker.io.attacker.example/token")));
        assertFalse(SECURE.allowsCredentials(URI.create("https://registry-1.docker.io/"),
                URI.create("http://auth.docker.io/token")));
        assertFalse(SECURE.allowsCredentials(URI.create("https://registry-1.docker.io.evil/"),
                URI.create("https://auth.docker.io/token")));
        assertFalse(SECURE.allowsCredentials(URI.create("https://registry.example.com/"),
                URI.create("https://auth.docker.io/token")));
    }

    @Test
    void honoursTheCrossOriginRealmAllowlist() {
        RegistryTrustPolicy policy = RegistryTrustPolicy.of(
                List.of(), List.of("auth.example.com"));

        assertTrue(policy.allowsCredentials(URI.create("https://registry.example.com/"),
                URI.create("https://auth.example.com/token")));
        assertFalse(policy.allowsCredentials(URI.create("https://registry.example.com/"),
                URI.create("https://auth.example.com.attacker.example/token")));
        assertFalse(policy.allowsCredentials(URI.create("https://registry.example.com/"),
                URI.create("https://other.example.com/token")));
    }
}
