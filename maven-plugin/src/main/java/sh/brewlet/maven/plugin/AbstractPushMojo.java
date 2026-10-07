// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.plugin.MojoFailureException;
import sh.brewlet.maven.plugin.oci.Credential;
import sh.brewlet.maven.plugin.oci.MediaTypes;
import sh.brewlet.maven.plugin.oci.RegistryClient;
import sh.brewlet.maven.plugin.oci.RunnableImageBuilder;
import sh.brewlet.maven.plugin.util.CredentialResolver;

import java.io.File;
import java.io.IOException;
import java.nio.file.Files;

/** Publication and immutable result handoff for {@code brewlet:push}. */
public abstract class AbstractPushMojo extends AbstractImageMojo {
    static final String PUSH_RESULT_FILE = "push.json";

    public record PushResult(String image, String digest, String deployImage, String format) {}

    protected PushResult pushApplication(String goal) throws MojoExecutionException, MojoFailureException {
        ApplicationBuildResult.clear(project);
        image = requirePushImage(goal);
        File pushResultFile = new File(outputDirectory, PUSH_RESULT_FILE);
        if (!dryRun && pushResultFile.exists() && !pushResultFile.delete()) {
            throw new MojoExecutionException("Failed to delete stale " + pushResultFile);
        }
        String[] parts = RegistryClient.splitRef(image);
        Credential credential = CredentialResolver.resolve(parts[0], settings, settingsDecrypter,
                message -> getLog().warn(message));
        RegistryClient client = new RegistryClient(parts[0], parts[1], credential, registryTrustPolicy());
        ApplicationAssembly assembly = assembleApplication(dryRun);
        if (assembly == null) return null;

        getLog().info("Brewlet: pushing " + format + " to " + image + " ...");
        getLog().info("  auth: " + (credential == null ? "anonymous" : "credentials resolved for " + parts[0]));
        try {
            String sourceRepository = assembly.managedLayerDigest == null ? null
                    : managedBundleSourceRepository(dependencyBundle, parts[0]);
            String digest = client.pushApplicationImage(RegistryClient.extractTag(image),
                    assembly.image, assembly.managedLayerDigest, sourceRepository);
            if (assembly.attestation != null) {
                client.pushReferrer(assembly.attestation);
                getLog().info("  managed dependency attestation: " + assembly.attestation.manifestDigest());
            }
            PushResult result = new PushResult(image, digest, RegistryClient.pinReference(image, digest), format);
            Files.createDirectories(outputDirectory.toPath());
            MAPPER.writerWithDefaultPrettyPrinter().writeValue(pushResultFile, result);
            assembly.published = true;
            assembly.result.save(project);
            getLog().info("Brewlet: pushed " + image);
            if ("image".equals(format)) {
                getLog().info("  index: " + digest);
                getLog().info("  platforms: " + RunnableImageBuilder.targetArches(assembly.result.config()));
            } else {
                getLog().info("  manifest: " + digest);
                getLog().info("  artifactType: " + MediaTypes.ARTIFACT_TYPE);
            }
            getLog().info("  deploy image: " + result.deployImage());
            getLog().info("  developer shipped ONLY the JAR; no Dockerfile, no base image.");
            return result;
        } catch (IOException | InterruptedException e) {
            if (e instanceof InterruptedException) Thread.currentThread().interrupt();
            throw new MojoExecutionException("Failed to push " + format + " to " + image, e);
        }
    }

    private String requirePushImage(String goal) throws MojoExecutionException {
        String ref = resolveImage();
        if (ref == null) {
            throw new MojoExecutionException("brewlet:" + goal + " requires a target registry. "
                    + "Configure <registry>myregistry.example.com</registry> (the image becomes "
                    + "<registry>/" + project.getArtifactId() + ":" + project.getVersion()
                    + ") or a full <image>, e.g. -Dbrewlet.registry=myregistry.azurecr.io");
        }
        if (!RegistryClient.hasExplicitRegistry(ref)) {
            throw new MojoExecutionException("Image \"" + ref + "\" has no registry host, so it "
                    + "would be pushed to Docker Hub. Prefix it with your registry "
                    + "(e.g. myregistry.azurecr.io/" + ref + "), set <registry>, or use "
                    + "docker.io/<user>/... to target Docker Hub explicitly.");
        }
        requirePushTag(ref);
        return ref;
    }
}
