// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.plugin.logging.SystemStreamLog;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.ValueSource;
import sh.brewlet.maven.plugin.oci.LocalStore;
import sh.brewlet.maven.plugin.oci.MediaTypes;

import java.nio.file.Files;
import java.nio.file.Path;

import static org.junit.jupiter.api.Assertions.*;

/** brewlet:inspect must apply the same CDS resolution as brewlet:build / brewlet:push (#211). */
class InspectMojoCdsTest {
    @TempDir Path root;

    @ParameterizedTest
    @ValueSource(strings = {"artifact", "image"})
    void cdsArchiveAppearsInPreview(String format) throws Exception {
        Path archive = root.resolve("app.jsa");
        Files.write(archive, new byte[] {1, 2, 3, 4});
        String digest = LocalStore.sha256Hex(Files.readAllBytes(archive));

        InspectMojo inspect = mojo(format);
        inspect.cdsArchive = archive.toFile();
        String out = run(inspect);

        assertTrue(out.contains("\"cds\""), out);
        assertTrue(out.contains("\"archive\" : \"app.jsa\"") || out.contains("\"archive\": \"app.jsa\""), out);
        assertTrue(out.contains("cds archive: app.jsa (mounted /app/app.jsa"), out);
        assertTrue(out.contains(digest), out);
        if ("artifact".equals(format)) {
            assertTrue(out.contains("cds layer: app.jsa: " + MediaTypes.CDS_LAYER_MEDIA_TYPE), out);
        } else {
            assertTrue(out.contains("cds: app.jsa folded into app layer"), out);
        }
    }

    @ParameterizedTest
    @ValueSource(strings = {"artifact", "image"})
    void aotCacheAppearsInPreview(String format) throws Exception {
        Path cache = root.resolve("app.aot");
        Files.write(cache, new byte[] {5, 6, 7});
        String digest = LocalStore.sha256Hex(Files.readAllBytes(cache));

        InspectMojo inspect = mojo(format);
        inspect.aotCache = cache.toFile();
        String out = run(inspect);

        assertTrue(out.contains("\"aot\""), out);
        assertTrue(out.contains("aot cache: app.aot (mounted /app/app.aot"), out);
        assertTrue(out.contains(digest), out);
        assertFalse(out.contains("cds archive:"), out);
        if ("artifact".equals(format)) {
            assertTrue(out.contains("aot layer: app.aot: " + MediaTypes.AOT_LAYER_MEDIA_TYPE), out);
        } else {
            assertTrue(out.contains("aot: app.aot folded into app layer"), out);
        }
    }

    @ParameterizedTest
    @ValueSource(strings = {"artifact", "image"})
    void bothArchivesAppearInPreview(String format) throws Exception {
        Path archive = root.resolve("app.jsa");
        Files.write(archive, new byte[] {1, 2, 3, 4});
        Path cache = root.resolve("app.aot");
        Files.write(cache, new byte[] {5, 6, 7});

        InspectMojo inspect = mojo(format);
        inspect.cdsArchive = archive.toFile();
        inspect.aotCache = cache.toFile();
        String out = run(inspect);

        assertTrue(out.contains("cds archive: app.jsa (mounted /app/app.jsa"), out);
        assertTrue(out.contains("aot cache: app.aot (mounted /app/app.aot"), out);
        if ("artifact".equals(format)) {
            assertTrue(out.contains("cds layer: app.jsa: " + MediaTypes.CDS_LAYER_MEDIA_TYPE), out);
            assertTrue(out.contains("aot layer: app.aot: " + MediaTypes.AOT_LAYER_MEDIA_TYPE), out);
        } else {
            assertTrue(out.contains("cds: app.jsa folded into app layer"), out);
            assertTrue(out.contains("aot: app.aot folded into app layer"), out);
        }
    }

    @Test
    void missingCdsArchiveFailsLikeBuild() throws Exception {
        InspectMojo inspect = mojo("artifact");
        inspect.cdsArchive = root.resolve("missing.jsa").toFile();
        assertThrows(MojoExecutionException.class, () -> run(inspect));
    }

    @Test
    void noCdsArchiveLeavesPreviewUnchanged() throws Exception {
        String out = run(mojo("artifact"));
        assertFalse(out.contains("\"cds\""), out);
        assertFalse(out.contains("cds archive:"), out);
    }

    private InspectMojo mojo(String format) throws Exception {
        var boot = TestApplications.boot(root.resolve("src"));
        InspectMojo inspect = new InspectMojo();
        TestApplications.configure(inspect, boot.source(), root.resolve("inspect"), true);
        inspect.format = format;
        return inspect;
    }

    private static String run(InspectMojo inspect) throws Exception {
        StringBuilder log = new StringBuilder();
        inspect.setLog(new SystemStreamLog() {
            @Override public void info(CharSequence content) { log.append(content).append('\n'); }
        });
        inspect.execute();
        return log.toString();
    }
}
