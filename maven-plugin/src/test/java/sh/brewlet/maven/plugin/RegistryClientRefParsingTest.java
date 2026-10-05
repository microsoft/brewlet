// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import org.junit.jupiter.api.Test;
import sh.brewlet.maven.plugin.oci.RegistryClient;

import static org.junit.jupiter.api.Assertions.*;

/**
 * Tests the OCI image reference parsing utilities in {@link RegistryClient}.
 */
class RegistryClientRefParsingTest {

    @Test
    void splitRef_explicitRegistry() {
        String[] parts = RegistryClient.splitRef("registry.example.com/team/orders-api:1.0.0");
        assertEquals("registry.example.com", parts[0]);
        assertEquals("team/orders-api", parts[1]);
    }

    @Test
    void splitRef_localhost() {
        String[] parts = RegistryClient.splitRef("localhost:5000/myapp:latest");
        assertEquals("localhost:5000", parts[0]);
        assertEquals("myapp", parts[1]);
    }

    @Test
    void splitRef_noExplicitRegistry() {
        // No dots/colons in the first path segment → use docker.io
        String[] parts = RegistryClient.splitRef("myimage:latest");
        assertEquals("registry-1.docker.io", parts[0]);
        // Repository must not carry the tag, or it corrupts /v2/{repository}/... URLs
        assertEquals("library/myimage", parts[1]);
    }

    @Test
    void splitRef_dockerHubAliasesUseTheRegistryEndpoint() {
        assertEquals("registry-1.docker.io", RegistryClient.splitRef("docker.io/me/app:1")[0]);
        assertEquals("registry-1.docker.io", RegistryClient.splitRef("index.docker.io/me/app:1")[0]);
        assertEquals("me/app", RegistryClient.splitRef("docker.io/me/app:1")[1]);
    }

    @Test
    void splitRef_bareLocalhostIsARegistry() {
        String[] parts = RegistryClient.splitRef("localhost/app:1");
        assertEquals("localhost", parts[0]);
        assertEquals("app", parts[1]);
    }

    @Test
    void hasExplicitRegistry() {
        assertTrue(RegistryClient.hasExplicitRegistry("myregistry.azurecr.io/app:1.0"));
        assertTrue(RegistryClient.hasExplicitRegistry("localhost:5000/app"));
        assertTrue(RegistryClient.hasExplicitRegistry("localhost/app"));
        assertTrue(RegistryClient.hasExplicitRegistry("docker.io/me/app"));
        assertFalse(RegistryClient.hasExplicitRegistry("app:1.0"));
        assertFalse(RegistryClient.hasExplicitRegistry("showmyjvm-springboot:1.0.0-SNAPSHOT"));
        assertFalse(RegistryClient.hasExplicitRegistry("team/app@sha256:" + "a".repeat(64)));
        assertFalse(RegistryClient.hasExplicitRegistry(null));
    }

    @Test
    void extractTag_explicitTag() {
        assertEquals("1.0.0", RegistryClient.extractTag("registry.example.com/team/app:1.0.0"));
    }

    @Test
    void extractTag_latestWhenNoTag() {
        assertEquals("latest", RegistryClient.extractTag("registry.example.com/team/app"));
    }

    @Test
    void extractTag_digest() {
        String ref = "registry.example.com/team/app@sha256:abc123";
        assertEquals("sha256:abc123", RegistryClient.extractTag(ref));
    }

    @Test
    void pinnedSourcesKeepTheirDigestAndRegistryPort() {
        String digest = "sha256:" + "a".repeat(64);
        for (String name : new String[]{"localhost:5000/team/app",
                "localhost:5000/team/app:1.0"}) {
            String ref = name + "@" + digest;
            assertArrayEquals(new String[]{"localhost:5000", "team/app"},
                    RegistryClient.splitRef(ref));
            assertEquals(digest, RegistryClient.extractTag(ref));
            assertTrue(RegistryClient.isDigestPinnedReference(ref));
        }
    }

    @Test
    void extractTag_snapshotVersion() {
        assertEquals("1.0.0-SNAPSHOT",
                RegistryClient.extractTag("registry.example.com/team/app:1.0.0-SNAPSHOT"));
    }

    @Test
    void digestPinnedReference_requiresCanonicalSha256() {
        String digest = "sha256:" + "a".repeat(64);
        assertTrue(RegistryClient.isDigestPinnedReference(
                "registry.example.com/team/app:1.0@" + digest));
        assertFalse(RegistryClient.isDigestPinnedReference(
                "registry.example.com/team/app:1.0"));
        assertFalse(RegistryClient.isDigestPinnedReference(
                "registry.example.com/team/app@sha256:not-a-digest"));
    }

    @Test
    void pinReference_replacesTagOrDigest() {
        String digest = "sha256:" + "a".repeat(64);
        assertEquals("registry.example.com/team/app@" + digest,
                RegistryClient.pinReference("registry.example.com/team/app:1.0", digest));
        assertEquals("localhost:5000/team/app@" + digest,
                RegistryClient.pinReference(
                        "localhost:5000/team/app@sha256:" + "b".repeat(64), digest));
    }
}
