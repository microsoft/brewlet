// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import com.sun.net.httpserver.HttpServer;
import org.apache.maven.plugin.MojoExecutionException;
import org.junit.jupiter.api.io.TempDir;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.CsvSource;

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
    @CsvSource({"push,image", "push,artifact", "deploy,image", "deploy,artifact"})
    void dryRunPreservesLastPushForSubsequentManifest(String goal, String format) throws Exception {
        AbstractPushMojo mojo = mojo(goal, format);
        byte[] original = writeLastPush(mojo);
        mojo.dryRun = true;

        mojo.execute();

        assertArrayEquals(original, Files.readAllBytes(handoff()));
        assertFalse(Files.exists(root.resolve("brewlet/javaapplication.yaml")));
        ManifestMojo manifest = manifest(mojo);
        manifest.execute();
        String yaml = Files.readString(root.resolve("brewlet/javaapplication.yaml"));
        assertTrue(yaml.contains("    image: \"" + pinnedImage(mojo) + "\"\n"), yaml);
        assertArrayEquals(original, Files.readAllBytes(handoff()));
    }

    @ParameterizedTest
    @CsvSource({"push,image", "push,artifact", "deploy,image", "deploy,artifact"})
    void dryRunWithoutLastPushDoesNotInventHandoff(String goal, String format) throws Exception {
        AbstractPushMojo mojo = mojo(goal, format);
        mojo.dryRun = true;

        mojo.execute();

        assertFalse(Files.exists(handoff()));
        assertFalse(Files.exists(root.resolve("brewlet/javaapplication.yaml")));
        MojoExecutionException error = assertThrows(MojoExecutionException.class,
                manifest(mojo)::execute);
        assertTrue(error.getMessage().contains("Run brewlet:push first"), error.getMessage());
    }

    @ParameterizedTest
    @CsvSource({"push,true", "deploy,true", "push,false", "deploy,false"})
    void validationFailurePreservesHandoffOnlyInDryRun(String goal, boolean dryRun) throws Exception {
        AbstractPushMojo mojo = mojo(goal, "image");
        byte[] original = writeLastPush(mojo);
        mojo.dryRun = dryRun;
        mojo.entryMode = "invalid";

        MojoExecutionException error = assertThrows(MojoExecutionException.class, mojo::execute);

        assertTrue(error.getMessage().contains("Invalid <entryMode>"), error.getMessage());
        if (dryRun) {
            assertArrayEquals(original, Files.readAllBytes(handoff()));
        } else {
            assertFalse(Files.exists(handoff()));
            assertThrows(MojoExecutionException.class, manifest(mojo)::execute);
        }
    }

    @ParameterizedTest
    @CsvSource({"push,image", "push,artifact", "deploy,image", "deploy,artifact"})
    void failedRealPushRemovesStaleHandoff(String goal, String format) throws Exception {
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
            AbstractPushMojo mojo = mojo(goal, format);
            String registry = "127.0.0.1:" + server.getAddress().getPort();
            mojo.image = registry + "/app:1";
            mojo.insecureRegistries = List.of(registry);
            writeLastPush(mojo);

            MojoExecutionException error = assertThrows(MojoExecutionException.class, mojo::execute);

            assertTrue(error.getMessage().contains("Failed to push"), error.getMessage());
            assertTrue(requests.get() > 0, "the push must reach the registry");
            assertFalse(Files.exists(handoff()));
            assertThrows(MojoExecutionException.class, manifest(mojo)::execute);
        } finally {
            server.stop(0);
        }
    }

    private AbstractPushMojo mojo(String goal, String format) throws Exception {
        AbstractPushMojo mojo;
        if ("deploy".equals(goal)) {
            DeployMojo deploy = new DeployMojo();
            deploy.kubectlRunner = (args, timeout) ->
                    fail("dry-run or failed push must not invoke kubectl");
            mojo = deploy;
        } else {
            mojo = new PushMojo();
        }
        Path jar = root.resolve("app.jar");
        Files.write(jar, TestApplications.zip(Map.of("META-INF/MANIFEST.MF",
                "Manifest-Version: 1.0\nMain-Class: example.Main\n\n".getBytes(StandardCharsets.UTF_8))));
        TestApplications.configure(mojo, jar, root.resolve("brewlet"), false);
        mojo.format = format;
        return mojo;
    }

    private ManifestMojo manifest(AbstractPushMojo push) throws Exception {
        ManifestMojo manifest = new ManifestMojo();
        TestApplications.configure(manifest, push.jarFile.toPath(), push.outputDirectory.toPath(), false);
        manifest.image = push.image;
        manifest.jdkFeature = 17;
        manifest.appName = "app";
        manifest.namespace = "default";
        TestApplications.set(manifest, "replicas", 1);
        return manifest;
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
