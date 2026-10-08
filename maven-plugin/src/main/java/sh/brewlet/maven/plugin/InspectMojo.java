// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.plugin.MojoFailureException;
import org.apache.maven.plugins.annotations.Mojo;
import org.apache.maven.plugins.annotations.ResolutionScope;
import sh.brewlet.maven.plugin.model.JvmConfig;
import sh.brewlet.maven.plugin.oci.ArtifactLayer;
import sh.brewlet.maven.plugin.oci.LocalStore;
import sh.brewlet.maven.plugin.oci.MediaTypes;

import java.io.File;
import java.io.IOException;
import java.nio.file.Files;
import java.util.List;

/**
 * <strong>brewlet:inspect</strong> — Print the fully-resolved launch config and
 * OCI artifact descriptor that <em>would</em> be pushed, without actually
 * pushing anything (dry-run equivalent).
 *
 * <p>Useful for verifying inference (JDK version, main class, entry mode)
 * before committing to {@code brewlet:push}.
 *
 * <p>Example:
 * <pre>
 * mvn brewlet:inspect
 * </pre>
 */
@Mojo(name = "inspect",
      requiresProject = true,
      requiresDependencyResolution = ResolutionScope.RUNTIME,
      threadSafe = true)
public class InspectMojo extends AbstractBrewletMojo {

    @Override
    protected void doExecute() throws MojoExecutionException, MojoFailureException {
        JvmConfig cfg = buildConfig();
        File jar = prepareApplication().jar();
        // Same CDS resolution and validation as brewlet:build / brewlet:push so
        // the preview matches what is actually published.
        File resolvedCdsArchive = applyCdsArchive(cfg);
        File resolvedAotCache = applyAotCache(cfg);
        validateFinalConfig(cfg);
        validateCdsPairing(cfg, resolvedCdsArchive);
        validateAotPairing(cfg, resolvedAotCache);
        boolean runnable = "image".equals(format);

        getLog().info("== Brewlet inspect ==");
        String resolvedImage = resolveImage();
        getLog().info("  image: " + (resolvedImage != null ? resolvedImage : "(not set)"));
        getLog().info("  jar: " + jar.getAbsolutePath() + " (" + jar.length() + " bytes)");
        getLog().info("  format: " + format
                + (runnable ? " (standard, kubelet-pullable OCI image)"
                            : " (native Brewlet OCI artifact, custom media types)"));
        if (runnable) {
            getLog().info("  kind: runnable OCI image");
            getLog().info("  configMediaType: " + sh.brewlet.maven.plugin.oci.MediaTypes.OCI_IMAGE_CONFIG_MEDIA_TYPE);
            getLog().info("  layerMediaType: " + sh.brewlet.maven.plugin.oci.MediaTypes.OCI_LAYER_GZIP_MEDIA_TYPE);
            getLog().info("  launchConfigAnnotation: " + sh.brewlet.maven.plugin.oci.MediaTypes.JVM_CONFIG_ANNOTATION);
            getLog().info("  platforms: "
                    + sh.brewlet.maven.plugin.oci.RunnableImageBuilder.targetArches(cfg));
        } else {
            getLog().info("  artifactType: " + sh.brewlet.maven.plugin.oci.MediaTypes.ARTIFACT_TYPE);
            getLog().info("  configMediaType: " + sh.brewlet.maven.plugin.oci.MediaTypes.CONFIG_MEDIA_TYPE);
            getLog().info("  layerMediaType: " + sh.brewlet.maven.plugin.oci.MediaTypes.JAR_LAYER_MEDIA_TYPE);
        }

        if (layered) {
            String mode = cfg.getEntry().getMode();
            List<ArtifactLayer> layers = buildArtifactLayers(mode);
            String kind = "module".equals(mode) ? "module-path" : "class-path";
            getLog().info("  layered: true (" + layers.size() + " " + kind + " layer(s))");
            for (ArtifactLayer layer : layers) {
                getLog().info("    - " + layer.name() + ": " + layer.mediaType()
                        + " (" + layer.tar().length + " bytes)");
            }
        }

        if (resolvedCdsArchive != null) {
            String name = cfg.getCds().getArchive();
            if (runnable) {
                getLog().info("  cds: " + name + " folded into app layer ("
                        + resolvedCdsArchive.length() + " bytes, " + sha256(resolvedCdsArchive) + ")");
            } else {
                ArtifactLayer cdsLayer = startupArchiveLayer(resolvedCdsArchive, MediaTypes.CDS_LAYER_MEDIA_TYPE);
                getLog().info("  cds layer: " + cdsLayer.name() + ": " + cdsLayer.mediaType()
                        + " (" + cdsLayer.tar().length + " bytes, "
                        + LocalStore.sha256Hex(cdsLayer.tar()) + ")");
            }
            getLog().info("  cds archive: " + name + " (mounted /app/" + name
                    + "; -Xshare:auto, best-effort)");
        }

        if (resolvedAotCache != null) {
            String name = cfg.getAot().getCache();
            if (runnable) {
                getLog().info("  aot: " + name + " folded into app layer ("
                        + resolvedAotCache.length() + " bytes, " + sha256(resolvedAotCache) + ")");
            } else {
                ArtifactLayer aotLayer = startupArchiveLayer(resolvedAotCache, MediaTypes.AOT_LAYER_MEDIA_TYPE);
                getLog().info("  aot layer: " + aotLayer.name() + ": " + aotLayer.mediaType()
                        + " (" + aotLayer.tar().length + " bytes, "
                        + LocalStore.sha256Hex(aotLayer.tar()) + ")");
            }
            getLog().info("  aot cache: " + name + " (mounted /app/" + name
                    + "; -XX:AOTCache, JDK 24+, best-effort)");
        }

        try {
            getLog().info("\n== jvm-config.json ==\n" + MAPPER.writeValueAsString(cfg));
        } catch (IOException e) {
            throw new MojoExecutionException("Failed to serialize config", e);
        }
    }

    private static String sha256(File file) throws MojoExecutionException {
        try {
            return LocalStore.sha256Hex(Files.readAllBytes(file.toPath()));
        } catch (IOException e) {
            throw new MojoExecutionException("Failed to read startup archive " + file.getAbsolutePath(), e);
        }
    }
}
