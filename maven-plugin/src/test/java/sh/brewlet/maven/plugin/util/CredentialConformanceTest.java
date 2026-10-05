// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin.util;

import org.junit.jupiter.api.DynamicTest;
import org.junit.jupiter.api.TestFactory;
import org.junit.jupiter.api.io.TempDir;

import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import java.util.stream.Stream;

import static org.junit.jupiter.api.Assertions.*;
import static sh.brewlet.maven.plugin.RegistryConformanceFixtures.*;

class CredentialConformanceTest {
    @TempDir
    Path root;

    @TestFactory
    Stream<DynamicTest> credentialSelection() throws Exception {
        return cases("credentials").map(tc -> DynamicTest.dynamicTest(tc.path("id").asText(), () -> {
            Path config = Files.createDirectory(root.resolve(tc.path("id").asText()));
            Files.writeString(config.resolve("config.json"), tc.required("config").toString());
            List<String> calls = new ArrayList<>();
            var credential = CredentialResolver.resolve(tc.path("registry").asText(), null, null,
                    message -> {}, key -> key.equals("DOCKER_CONFIG") ? config.toString()
                            : tc.path("env").path(key).asText(null),
                    (helper, server) -> {
                        calls.add(helper + "|" + server);
                        if (tc.path("helperError").asBoolean()) {
                            throw new IOException("fixture helper failure");
                        }
                        var output = tc.path("helpers").path(helper);
                        return output.isMissingNode() ? null : output.toString();
                    });
            assertEquals(strings(tc.required("calls")), calls);
            if (!tc.has("username")) {
                assertNull(credential);
            } else {
                assertNotNull(credential);
                assertEquals(tc.path("username").asText(), credential.getUsername());
                assertEquals(tc.path("secret").asText(), credential.getPassword());
                assertEquals(tc.path("identity").asBoolean(), credential.isIdentityToken());
            }
        }));
    }
}
