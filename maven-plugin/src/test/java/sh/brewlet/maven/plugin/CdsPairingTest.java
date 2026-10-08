// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import org.apache.maven.plugin.MojoExecutionException;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;
import sh.brewlet.maven.plugin.model.Entry;
import sh.brewlet.maven.plugin.model.JvmConfig;

import java.io.File;
import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;

import static org.junit.jupiter.api.Assertions.assertDoesNotThrow;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

class CdsPairingTest {

    @TempDir
    Path tempDir;

    @Test
    void configCdsWithoutArchive_fails() {
        JvmConfig cfg = sampleConfig();
        cfg.setCds(new JvmConfig.Cds("app.jsa", "dynamic"));

        assertThrows(MojoExecutionException.class,
                () -> AbstractBrewletMojo.validateCdsPairing(cfg, null));
    }

    @Test
    void absentArchiveWithConfigCds_fails() {
        JvmConfig cfg = sampleConfig();
        cfg.setCds(new JvmConfig.Cds("app.jsa", "dynamic"));

        assertThrows(MojoExecutionException.class,
                () -> AbstractBrewletMojo.validateCdsPairing(
                        cfg, tempDir.resolve("missing.jsa").toFile()));
    }

    @Test
    void archiveWithoutConfigCds_fails() throws IOException {
        File archive = archive("app.jsa");

        assertThrows(MojoExecutionException.class,
                () -> AbstractBrewletMojo.validateCdsPairing(sampleConfig(), archive));
    }

    @Test
    void archiveBasenameMismatch_fails() throws IOException {
        JvmConfig cfg = sampleConfig();
        cfg.setCds(new JvmConfig.Cds("expected.jsa", "dynamic"));

        assertThrows(MojoExecutionException.class,
                () -> AbstractBrewletMojo.validateCdsPairing(cfg, archive("actual.jsa")));
    }

    @Test
    void archiveAndConfigMatch_ok() throws IOException {
        JvmConfig cfg = sampleConfig();
        cfg.setCds(new JvmConfig.Cds("app.jsa", "dynamic"));

        assertDoesNotThrow(
                () -> AbstractBrewletMojo.validateCdsPairing(cfg, archive("app.jsa")));
    }

    @Test
    void aotConfigWithoutCache_fails() {
        JvmConfig cfg = sampleConfig();
        cfg.setAot(new JvmConfig.Aot("app.aot"));

        MojoExecutionException e = assertThrows(MojoExecutionException.class,
                () -> AbstractBrewletMojo.validateAotPairing(cfg, null));
        assertTrue(e.getMessage().contains("aot.cache"), e.getMessage());
    }

    @Test
    void aotCacheWithoutConfig_fails() throws IOException {
        assertThrows(MojoExecutionException.class,
                () -> AbstractBrewletMojo.validateAotPairing(sampleConfig(), archive("app.aot")));
    }

    @Test
    void aotBasenameMismatch_fails() throws IOException {
        JvmConfig cfg = sampleConfig();
        cfg.setAot(new JvmConfig.Aot("expected.aot"));

        assertThrows(MojoExecutionException.class,
                () -> AbstractBrewletMojo.validateAotPairing(cfg, archive("actual.aot")));
    }

    @Test
    void aotCacheAndConfigMatch_ok() throws IOException {
        JvmConfig cfg = sampleConfig();
        cfg.setAot(new JvmConfig.Aot("app.aot"));

        assertDoesNotThrow(() -> AbstractBrewletMojo.validateAotPairing(cfg, archive("app.aot")));
    }

    @Test
    void cdsArchiveAndAotCache_bothSet_ok() throws IOException, MojoExecutionException {
        BuildMojo mojo = new BuildMojo();
        mojo.cdsArchive = archive("app.jsa");
        mojo.aotCache = archive("app.aot");
        JvmConfig cfg = sampleConfig();

        mojo.applyCdsArchive(cfg);
        mojo.applyAotCache(cfg);

        org.junit.jupiter.api.Assertions.assertEquals("app.jsa", cfg.getCds().getArchive());
        org.junit.jupiter.api.Assertions.assertEquals("app.aot", cfg.getAot().getCache());
        assertDoesNotThrow(cfg::validate);
    }

    @Test
    void applyAotCache_defaultsHintFromBasename() throws IOException, MojoExecutionException {
        BuildMojo mojo = new BuildMojo();
        mojo.aotCache = archive("app.aot");
        JvmConfig cfg = sampleConfig();

        mojo.applyAotCache(cfg);

        org.junit.jupiter.api.Assertions.assertEquals("app.aot", cfg.getAot().getCache());
    }

    private File archive(String name) throws IOException {
        Path archive = tempDir.resolve(name);
        Files.writeString(archive, "jsa");
        return archive.toFile();
    }

    private static JvmConfig sampleConfig() {
        JvmConfig cfg = new JvmConfig();
        cfg.setMainJar("app.jar");
        cfg.setEntry(new Entry("jar"));
        return cfg;
    }
}
