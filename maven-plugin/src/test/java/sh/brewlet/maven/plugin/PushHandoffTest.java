// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import com.sun.net.httpserver.HttpServer;
import org.apache.maven.plugin.MojoExecutionException;
import org.junit.jupiter.api.io.TempDir;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.ValueSource;

import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.List;
import java.util.Map;
import java.util.concurrent.atomic.AtomicInteger;

import static org.junit.jupiter.api.Assertions.*;

class PushHandoffTest {
    @TempDir Path root;

    @ParameterizedTest
    @ValueSource(strings = {"image", "artifact"})
    void dryRunPreservesLastPushForReleaseTooling(String format) throws Exception {
        AbstractPushMojo mojo = mojo(format);
        byte[] original = writeLastPush(mojo);
        mojo.dryRun = true;
        new ApplicationBuildResult(mojo.image, "sha256:" + "1".repeat(64), format,
                new sh.brewlet.maven.plugin.model.JvmConfig()).save(mojo.project);

        mojo.execute();

        assertNull(ApplicationBuildResult.get(mojo.project), "Dry-run push must invalidate an earlier build result");
        assertArrayEquals(original, Files.readAllBytes(handoff()));
        assertFalse(Files.exists(root.resolve("brewlet/javaapplication.yaml")));
        var result = AbstractBrewletMojo.MAPPER.readValue(handoff().toFile(), AbstractPushMojo.PushResult.class);
        assertEquals(pinnedImage(mojo), result.deployImage());
        assertEquals(format, result.format());
    }

    @ParameterizedTest
    @ValueSource(strings = {"image", "artifact"})
    void dryRunWithoutLastPushDoesNotInventHandoff(String format) throws Exception {
        AbstractPushMojo mojo = mojo(format);
        mojo.dryRun = true;

        mojo.execute();

        assertFalse(Files.exists(handoff()));
        assertNull(ApplicationBuildResult.get(mojo.project));
        assertFalse(Files.exists(root.resolve("brewlet/javaapplication.yaml")));
    }

    @ParameterizedTest
    @ValueSource(booleans = {true, false})
    void validationFailurePreservesHandoffOnlyInDryRun(boolean dryRun) throws Exception {
        AbstractPushMojo mojo = mojo("image");
        byte[] original = writeLastPush(mojo);
        mojo.dryRun = dryRun;
        mojo.entryMode = "invalid";

        MojoExecutionException error = assertThrows(MojoExecutionException.class, mojo::execute);

        assertTrue(error.getMessage().contains("Invalid <entryMode>"), error.getMessage());
        if (dryRun) {
            assertArrayEquals(original, Files.readAllBytes(handoff()));
        } else {
            assertFalse(Files.exists(handoff()));
        }
    }

    @ParameterizedTest
    @ValueSource(strings = {"image", "artifact"})
    void failedRealPushRemovesStaleHandoff(String format) throws Exception {
        HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        AtomicInteger requests = new AtomicInteger();
        server.createContext("/", exchange -> {
            try (exchange) {
                requests.incrementAndGet();
                exchange.sendResponseHeaders(500, -1);
            }
        });
        server.start();
        try {
            AbstractPushMojo mojo = mojo(format);
            String registry = "127.0.0.1:" + server.getAddress().getPort();
            mojo.image = registry + "/app:1";
            mojo.insecureRegistries = List.of(registry);
            writeLastPush(mojo);

            MojoExecutionException error = assertThrows(MojoExecutionException.class, mojo::execute);

            assertTrue(error.getMessage().contains("Failed to push"), error.getMessage());
            assertTrue(requests.get() > 0, "the push must reach the registry");
            assertFalse(Files.exists(handoff()));
        } finally {
            server.stop(0);
        }
    }

    private AbstractPushMojo mojo(String format) throws Exception {
        AbstractPushMojo mojo = new PushMojo();
        Path jar = root.resolve("app.jar");
        Files.write(jar, TestApplications.zip(Map.of("META-INF/MANIFEST.MF",
                "Manifest-Version: 1.0\nMain-Class: example.Main\n\n".getBytes(StandardCharsets.UTF_8))));
        TestApplications.configure(mojo, jar, root.resolve("brewlet"), false);
        mojo.format = format;
        return mojo;
    }

    private byte[] writeLastPush(AbstractPushMojo mojo) throws Exception {
        Files.createDirectories(mojo.outputDirectory.toPath());
        byte[] bytes = ("""
                { "format" : "%s", "image" : "%s",
                  "deployImage" : "%s", "digest" : "sha256:%s" }

                """.formatted(mojo.format, mojo.image, pinnedImage(mojo), "1".repeat(64)))
                .getBytes(StandardCharsets.UTF_8);
        Files.write(handoff(), bytes);
        return bytes;
    }

    private static String pinnedImage(AbstractPushMojo mojo) {
        return mojo.image.substring(0, mojo.image.lastIndexOf(':')) + "@sha256:" + "1".repeat(64);
    }

    private Path handoff() {
        return root.resolve("brewlet").resolve(AbstractPushMojo.PUSH_RESULT_FILE);
    }
}
