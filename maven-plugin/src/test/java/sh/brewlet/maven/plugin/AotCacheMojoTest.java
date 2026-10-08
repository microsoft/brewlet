// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import org.apache.maven.plugin.MojoExecutionException;
import org.junit.jupiter.api.Assumptions;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.Map;

import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

class AotCacheMojoTest {
    @TempDir Path root;

    @Test
    void belowJdk25Fails() throws Exception {
        Assumptions.assumeTrue(Runtime.version().feature() < 25, "AOT cache training is supported on JDK 25+");
        AotCacheMojo mojo = mojo();
        MojoExecutionException failure = assertThrows(MojoExecutionException.class, mojo::execute);
        assertTrue(failure.getMessage().contains("JDK 25"), failure.getMessage());
    }

    @Test
    void invalidReadinessMessageNamesAotcacheProperty() throws Exception {
        AotCacheMojo mojo = mojo();
        TestApplications.set(mojo, "mode", "signal");
        MojoExecutionException failure = assertThrows(MojoExecutionException.class, mojo::execute);
        assertTrue(failure.getMessage().contains("brewlet.aotcache.readyLog"), failure.getMessage());
    }

    private AotCacheMojo mojo() throws Exception {
        Path classes = TestApplications.compile(root.resolve("server"), "TrainingServer", TestApplications.SERVER);
        Path jar = root.resolve("server.jar");
        Files.write(jar, TestApplications.zip(Map.of(
                "META-INF/MANIFEST.MF", "Manifest-Version: 1.0\nMain-Class: TrainingServer\n\n".getBytes(StandardCharsets.UTF_8),
                "TrainingServer.class", Files.readAllBytes(classes.resolve("TrainingServer.class")))));
        AotCacheMojo mojo = new AotCacheMojo();
        TestApplications.configure(mojo, jar, root.resolve("output"), false);
        TestApplications.set(mojo, "aotCacheOutput", root.resolve("app.aot").toFile());
        TestApplications.set(mojo, "timeoutSeconds", 5);
        TestApplications.set(mojo, "trainingArgs", java.util.List.of(root.resolve("server.pid").toString()));
        return mojo;
    }
}
