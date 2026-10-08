// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.plugin.MojoFailureException;
import org.apache.maven.plugins.annotations.Mojo;
import org.apache.maven.plugins.annotations.Parameter;
import org.apache.maven.plugins.annotations.ResolutionScope;
import sh.brewlet.maven.plugin.model.JvmConfig;
import sh.brewlet.maven.plugin.oci.LocalStore;
import sh.brewlet.maven.plugin.oci.OciDescriptor;

import java.io.File;
import java.io.IOException;

/**
 * <strong>brewlet:build</strong> — Assemble the Brewlet OCI payload into a
 * local OCI image-layout directory ({@code target/brewlet/oci}) without pushing
 * to a registry. Useful for inspection, air-gapped flows, or integration tests
 * against a local registry.
 * Reuses an image already assembled by build/push in the same invocation.
 *
 * <p>The resulting layout matches the CLI's {@code --store} format and can be
 * read with {@code brewlet inspect} or pushed with {@code oras push}.
 *
 * <p>Example:
 * <pre>
 * mvn brewlet:build
 * ls target/brewlet/oci/blobs/sha256/
 * </pre>
 */
@Mojo(name = "build",
      requiresProject = true,
      requiresDependencyResolution = ResolutionScope.RUNTIME,
      threadSafe = true)
public class BuildMojo extends AbstractImageMojo {

    /**
     * OCI image-layout output directory.
     * Defaults to {@code ${project.build.directory}/brewlet/oci}.
     */
    @Parameter(defaultValue = "${project.build.directory}/brewlet/oci")
    private File ociOutputDirectory;

    @Override
    protected void doExecute() throws MojoExecutionException, MojoFailureException {
        ApplicationBuildResult.clear(project);
        image = resolveImage();
        if (image == null) {
            throw new MojoExecutionException(
                    "brewlet:build requires <image> (or <registry>) to be set (used as the OCI ref name).");
        }

        if (dryRun) {
            getLog().info("Brewlet: build writes the local OCI layout even with brewlet.dryRun=true; "
                    + "it never publishes.");
        }
        ApplicationAssembly assembly = assembleApplication(false);
        LocalStore store = new LocalStore(ociOutputDirectory.toPath());
        boolean runnable = "image".equals(format);
        OciDescriptor resultDesc;
        try {
            resultDesc = store.pushApplicationImage(image, assembly.image);
            if (assembly.attestation != null) store.pushReferrer(assembly.attestation);
        } catch (IOException e) {
            throw new MojoExecutionException("Failed to write local OCI layout", e);
        }

        assembly.result.save(project);
        getLog().info("Brewlet: wrote OCI image-layout → " + ociOutputDirectory.getPath());
        getLog().info("  " + (runnable ? "index" : "manifest") + ": " + resultDesc.getDigest()
                + " (" + resultDesc.getSize() + " bytes)");
        getLog().info("  format: " + format);
        getLog().info("  ref: " + image);
        JvmConfig cfg = assembly.result.config();
        if (cfg.getCds() != null) {
            getLog().info("  cds archive: " + cfg.getCds().getArchive()
                    + " (mounted /app/" + cfg.getCds().getArchive()
                    + "; -Xshare:auto, best-effort)");
        }
        if (cfg.getAot() != null) {
            getLog().info("  aot cache: " + cfg.getAot().getCache()
                    + " (mounted /app/" + cfg.getAot().getCache()
                    + "; -XX:AOTCache, JDK 24+, best-effort)");
        }
    }
}
