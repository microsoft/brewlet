// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import org.apache.maven.plugin.MojoExecutionException;
import org.junit.jupiter.api.DynamicTest;
import org.junit.jupiter.api.TestFactory;
import org.junit.jupiter.api.io.TempDir;
import sh.brewlet.maven.plugin.oci.RegistryClient;
import sh.brewlet.maven.plugin.oci.RegistryTrustPolicy;

import java.net.URI;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.List;
import java.util.Map;
import java.util.stream.Stream;

import static org.junit.jupiter.api.Assertions.*;
import static sh.brewlet.maven.plugin.RegistryConformanceFixtures.*;

class RegistryConformanceTest {
    @TempDir
    Path root;

    @TestFactory
    Stream<DynamicTest> references() throws Exception {
        return cases("references").map(tc -> DynamicTest.dynamicTest(tc.path("id").asText(), () -> {
            String ref = tc.path("input").asText();
            assertEquals(tc.path("explicit").asBoolean(), RegistryClient.hasExplicitRegistry(ref));
            if (tc.has("repository")) {
                assertArrayEquals(new String[]{tc.path("registry").asText(),
                        tc.path("repository").asText()}, RegistryClient.splitRef(ref));
                assertEquals(tc.path("reference").asText(), RegistryClient.extractTag(ref));
            }
            for (AbstractPushMojo mojo : List.of(new PushMojo(), new DeployMojo())) {
                TestApplications.configure(mojo, root.resolve("missing.jar"),
                        root.resolve(tc.path("id").asText()), false);
                mojo.image = ref;
                mojo.dryRun = true;
                if (!tc.path("publish").asBoolean()) {
                    // Validation must precede artifact preparation, credentials and network.
                    MojoExecutionException error = assertThrows(MojoExecutionException.class, mojo::execute);
                    assertTrue(error.getMessage().startsWith("Image "),
                            "expected reference rejection, got: " + error.getMessage());
                } else {
                    Path jar = root.resolve("app.jar");
                    Files.write(jar, TestApplications.zip(Map.of("META-INF/MANIFEST.MF",
                            "Manifest-Version: 1.0\nMain-Class: app.Main\n\n".getBytes(StandardCharsets.UTF_8))));
                    mojo.jarFile = jar.toFile();
                    if (mojo instanceof DeployMojo deploy) {
                        deploy.kubectlRunner = (args, timeout) -> {
                            fail("dry run must not invoke kubectl");
                            return null;
                        };
                    }
                    assertDoesNotThrow(mojo::execute);
                    assertEquals(ref, mojo.image);
                }
            }
        }));
    }

    @TestFactory
    Stream<DynamicTest> authorities() throws Exception {
        return cases("authorities").map(tc -> DynamicTest.dynamicTest(tc.path("id").asText(), () -> {
            String authority = tc.path("authority").asText();
            boolean valid = tc.path("valid").asBoolean();
            var policy = RegistryTrustPolicy.of(strings(tc.path("insecure")), List.of());
            assertEquals(valid, RegistryTrustPolicy.isBareAuthority(authority));
            assertEquals(tc.path("plaintext").asBoolean(), policy.allowsPlaintext(authority));
            if (valid) {
                assertDoesNotThrow(() -> RegistryTrustPolicy.of(List.of(authority), List.of()));
                assertDoesNotThrow(() -> RegistryTrustPolicy.of(List.of(), List.of(authority)));
            } else {
                assertThrows(IllegalArgumentException.class,
                        () -> RegistryTrustPolicy.of(List.of(authority), List.of()));
                assertThrows(IllegalArgumentException.class,
                        () -> RegistryTrustPolicy.of(List.of(), List.of(authority)));
            }
        }));
    }

    @TestFactory
    Stream<DynamicTest> realms() throws Exception {
        return cases("realms").map(tc -> DynamicTest.dynamicTest(tc.path("id").asText(), () -> {
            var policy = RegistryTrustPolicy.of(strings(tc.path("insecure")), strings(tc.path("allowed")));
            String realm = tc.path("realm").asText();
            if (!tc.path("valid").asBoolean()) {
                assertThrows(IllegalArgumentException.class, () -> policy.validateTokenRealm(realm));
                return;
            }
            URI target = policy.validateTokenRealm(realm);
            URI origin = URI.create(tc.path("origin").asText());
            assertEquals(tc.path("credentials").asBoolean(), policy.allowsCredentials(origin, target));
            assertEquals(tc.path("sameOrigin").asBoolean(), RegistryTrustPolicy.sameOrigin(origin, target));
        }));
    }
}
