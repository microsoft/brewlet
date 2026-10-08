// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.plugin.MojoFailureException;
import sh.brewlet.maven.plugin.model.JvmConfig;
import sh.brewlet.maven.plugin.model.ManagedDependencyEvidence;
import sh.brewlet.maven.plugin.oci.ArtifactLayer;
import sh.brewlet.maven.plugin.oci.ApplicationImage;
import sh.brewlet.maven.plugin.oci.Credential;
import sh.brewlet.maven.plugin.oci.DependencyBundle;
import sh.brewlet.maven.plugin.oci.LocalStore;
import sh.brewlet.maven.plugin.oci.MediaTypes;
import sh.brewlet.maven.plugin.oci.RegistryClient;
import sh.brewlet.maven.plugin.oci.RegistryTrustPolicy;
import sh.brewlet.maven.plugin.util.CredentialResolver;
import sh.brewlet.maven.plugin.util.JarInspector;
import sh.brewlet.maven.plugin.supplychain.BundleProvenance;
import sh.brewlet.maven.plugin.supplychain.ManagedProvenance;
import sh.brewlet.maven.plugin.oci.OciDescriptor;
import sh.brewlet.maven.plugin.oci.OciReferrer;
import sh.brewlet.maven.plugin.oci.RunnableImageBuilder;

import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.InvalidPathException;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.security.GeneralSecurityException;

/**
 * Shared input validation and invocation-local image assembly for build and push.
 */
public abstract class AbstractImageMojo extends AbstractBrewletMojo {

    protected ApplicationAssembly assembleApplication(boolean preview)
            throws MojoExecutionException, MojoFailureException {
        java.io.File jar = resolveJarFile();
        VerifiedBundle verifiedBundle = null;
        DependencyBundle.Content managedBundle = null;
        String expectedBundleSigner = trustedSignerIdentity;
        String applicationBuilder = builderIdentity;
        if (dependencyBundle != null && !dependencyBundle.isBlank()) {
            if ((signingKey == null) !=
                    (applicationBuilder == null || applicationBuilder.isBlank())) {
                throw new MojoExecutionException(
                        "signingKey and builderIdentity must be configured together");
            }
            if (!"image".equals(format)) {
                throw new MojoExecutionException("Managed dependency bundles require "
                        + "<format>image</format> so the approved OCI layer is reused unchanged.");
            }
            layered = true;
            entryMode = "classpath";
            try {
                List<String> embedded = JarInspector.embeddedJars(jar);
                if (!embedded.isEmpty() || JarInspector.hasNestedApplicationClasses(jar)) {
                    throw new MojoExecutionException("Managed dependency bundle mode requires a "
                            + "thin application JAR, but found "
                            + (embedded.isEmpty() ? "nested application classes" : "embedded JAR " + embedded.get(0))
                            + ". Disable fat-JAR repackaging.");
                }
                verifiedBundle = resolveDependencyBundle(
                        dependencyBundle, expectedBundleSigner, registryTrustPolicy());
                managedBundle = verifiedBundle.bundle();
                DependencyBundle.verifyGraph(managedBundle.lock(),
                        collectRuntimeDependencyLock());
                List<Integer> compatibleJdks = managedBundle.config().getCompatibleJdks();
                if (compatibleJdks != null && !compatibleJdks.isEmpty()
                        && !compatibleJdks.contains(resolveJdkFeature())) {
                    throw new MojoExecutionException("Dependency bundle supports JDKs "
                            + compatibleJdks + " but this project requests JDK "
                            + resolveJdkFeature());
                }
            } catch (IOException | GeneralSecurityException e) {
                throw new MojoExecutionException("Invalid dependency bundle "
                        + dependencyBundle + ": " + e.getMessage(), e);
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
                throw new MojoExecutionException("Interrupted while pulling dependency bundle "
                        + dependencyBundle, e);
            }
        }
        JvmConfig cfg = buildConfig();
        jar = prepareApplication().jar();
        if (managedBundle != null
                && (cfg.getEntry().getMainClass() == null
                || cfg.getEntry().getMainClass().isBlank())) {
            throw new MojoExecutionException("Managed dependency bundle mode requires mainClass "
                    + "for thin classpath launch. Configure <mainClass>.");
        }
        java.io.File resolvedCdsArchive = applyCdsArchive(cfg);
        java.io.File resolvedAotCache = applyAotCache(cfg);
        validateFinalConfig(cfg);
        validateCdsPairing(cfg, resolvedCdsArchive);
        validateAotPairing(cfg, resolvedAotCache);
        // At most one of the two is set (applyAotCache and validate enforce it).
        java.io.File startupArchive = resolvedAotCache != null ? resolvedAotCache : resolvedCdsArchive;

        boolean runnable = "image".equals(format);
        if (!"artifact".equals(format) && !runnable) {
            throw new MojoExecutionException("Invalid <format> \"" + format
                    + "\": expected \"artifact\" or \"image\".");
        }

        Map<String, String> annotations = buildAnnotations();
        annotations.put(MediaTypes.ANNOTATION_REF_NAME, image);
        if (managedBundle != null) {
            try {
                ManagedDependencyEvidence evidence = new ManagedDependencyEvidence(
                        1, true,
                        LocalStore.sha256Hex(Files.readAllBytes(jar.toPath())),
                        managedBundle.manifestDigest(),
                        managedBundle.config().getLayerDigest(),
                        managedBundle.config().getLockDigest(),
                        verifiedBundle.sbomDigest(),
                        managedBundle.config().getSourceBom(),
                        signingKey == null ? null : applicationBuilder);
                annotations.put(MediaTypes.MANAGED_DEPENDENCY_EVIDENCE_ANNOTATION,
                        DependencyBundle.canonicalJson(evidence));
            } catch (IOException e) {
                throw new MojoExecutionException("Failed to serialize managed dependency evidence", e);
            }
        }

        List<ArtifactLayer> layers = managedBundle == null
                ? new ArrayList<>(buildArtifactLayers(cfg.getEntry().getMode())) : new ArrayList<>();
        if (!runnable) {
            ArtifactLayer startup = startupArchiveLayer(cfg, startupArchive);
            if (startup != null) layers.add(startup);
        }
        try {
            Map<String, String> identityAnnotations = new java.util.TreeMap<>(annotations);
            identityAnnotations.remove(MediaTypes.ANNOTATION_CREATED);
            List<Object> inputs = new ArrayList<>();
            inputs.add(image);
            inputs.add(format);
            inputs.add(MAPPER.readTree(MAPPER.writeValueAsBytes(cfg)));
            inputs.add(identityAnnotations);
            inputs.add(LocalStore.sha256Hex(Files.readAllBytes(jar.toPath())));
            inputs.add(startupArchive == null ? null
                    : LocalStore.sha256Hex(Files.readAllBytes(startupArchive.toPath())));
            for (ArtifactLayer layer : layers) {
                inputs.add(List.of(layer.name(), layer.mediaType(), LocalStore.sha256Hex(layer.tar())));
            }
            if (managedBundle != null) {
                inputs.add(managedBundle.manifestDigest());
                inputs.add(signingKey == null ? null
                        : LocalStore.sha256Hex(Files.readAllBytes(signingKey.toPath())));
            }
            String fingerprint = LocalStore.sha256Hex(MAPPER.writeValueAsBytes(inputs));
            ApplicationAssembly existing = ApplicationAssembly.get(project);
            if (existing != null && !existing.fingerprint.equals(fingerprint)) {
                throw new MojoExecutionException("Conflicting build/push inputs in the same Maven invocation. "
                        + "Use the same image reference, application bytes and image configuration for both goals, "
                        + "or run distinct builds in separate Maven invocations.");
            }
            if (preview) {
                getLog().info("Brewlet: dry-run mode — validated " + image + " (format=" + format
                        + "); no image assembled or published.");
                getLog().info(MAPPER.writeValueAsString(cfg));
                return null;
            }
            if (existing != null) {
                getLog().info("Brewlet: reusing assembled image " + existing.result.digest());
                return existing;
            }
            ApplicationImage assembled;
            if (runnable) {
                Path cds = startupArchive == null ? null : startupArchive.toPath();
                RunnableImageBuilder.Result result = managedBundle == null
                        ? RunnableImageBuilder.build(cfg, jar.toPath(), layers, cds, annotations)
                        : RunnableImageBuilder.buildWithManagedDependencyLayer(cfg, jar.toPath(),
                                new RunnableImageBuilder.ManagedDependencyLayer(
                                        managedBundle.compressedLayer(), managedBundle.config().getLayerDigest(),
                                        managedBundle.config().getLayerDiffId(), "dependencies"), cds, annotations);
                assembled = ApplicationImage.runnable(result);
            } else {
                assembled = ApplicationImage.artifact(cfg, Files.readAllBytes(jar.toPath()), layers, annotations);
            }
            OciReferrer.Content attestation = null;
            if (managedBundle != null && signingKey != null) {
                attestation = ManagedProvenance.create(
                        image, assembled.root().digest(), assembled.root().data().length,
                        LocalStore.sha256Hex(Files.readAllBytes(jar.toPath())),
                        managedBundle.manifestDigest(), managedBundle.config().getLayerDigest(),
                        managedBundle.config().getLockDigest(), verifiedBundle.sbomDigest(),
                        managedBundle.config().getSourceBom(), applicationBuilder, signingKey.toPath());
            }
            ApplicationAssembly assembly = new ApplicationAssembly(fingerprint, assembled,
                    new ApplicationBuildResult(image, assembled.root().digest(), format,
                            MAPPER.readValue(MAPPER.writeValueAsBytes(cfg), JvmConfig.class)),
                    managedBundle == null ? null : managedBundle.config().getLayerDigest(), attestation);
            assembly.save(project);
            getLog().info("Brewlet: assembled " + format + " " + assembly.result.digest());
            return assembly;
        } catch (IOException | GeneralSecurityException e) {
            throw new MojoExecutionException("Failed to assemble application image: " + e.getMessage(), e);
        }
    }

    private VerifiedBundle resolveDependencyBundle(String reference, String expectedBundleSigner,
                                                  RegistryTrustPolicy trustPolicy)
            throws IOException, InterruptedException, GeneralSecurityException, MojoExecutionException {
        try {
            Path path = Path.of(reference);
            if (Files.isDirectory(path)) {
                DependencyBundle.Content bundle = DependencyBundle.loadLayout(path);
                LocalStore store = new LocalStore(path);
                List<OciDescriptor> sbomRefs = store.referrers(bundle.manifestDigest(),
                        MediaTypes.CYCLONEDX_ARTIFACT_TYPE);
                List<OciDescriptor> provenanceRefs = store.referrers(bundle.manifestDigest(),
                        MediaTypes.DSSE_ARTIFACT_TYPE);
                return verifyBundleReferrers(bundle, sbomRefs, provenanceRefs,
                        store::readReferrerDocument, expectedBundleSigner);
            }
        } catch (InvalidPathException ignored) {
            // A registry reference is not required to be a valid local path.
        }
        String[] parts = RegistryClient.splitRef(reference);
        Credential bundleCredential = CredentialResolver.resolve(parts[0], settings, settingsDecrypter);
        RegistryClient client = new RegistryClient(parts[0], parts[1], bundleCredential,
                trustPolicy);
        DependencyBundle.Content bundle =
                client.pullDependencyBundle(RegistryClient.extractTag(reference));
        List<OciDescriptor> sbomRefs = client.discoverReferrers(bundle.manifestDigest(),
                MediaTypes.CYCLONEDX_ARTIFACT_TYPE);
        List<OciDescriptor> provenanceRefs = client.discoverReferrers(bundle.manifestDigest(),
                MediaTypes.DSSE_ARTIFACT_TYPE);
        return verifyBundleReferrers(bundle, sbomRefs, provenanceRefs,
                descriptor -> client.pullReferrerDocument(
                        descriptor, bundle.manifestDigest(),
                        bundle.manifestBytes().length), expectedBundleSigner);
    }

    private VerifiedBundle verifyBundleReferrers(DependencyBundle.Content bundle,
                                                  List<OciDescriptor> sbomRefs,
                                                  List<OciDescriptor> provenanceRefs,
                                                  ReferrerReader reader,
                                                  String expectedBundleSigner)
            throws IOException, InterruptedException, GeneralSecurityException {
        if (sbomRefs.size() != 1) {
            throw new GeneralSecurityException(
                    "Managed bundle requires exactly one SBOM referrer");
        }
        byte[] sbom = reader.read(sbomRefs.get(0));
        if (provenanceRefs.isEmpty()) {
            return new VerifiedBundle(bundle, BundleProvenance.validateSbom(bundle, sbom));
        }
        if (trustedPublicKey == null || expectedBundleSigner == null
                || expectedBundleSigner.isBlank()) {
            throw new GeneralSecurityException("Bundle provenance is present; "
                    + "trustedPublicKey and trustedSignerIdentity are required");
        }
        GeneralSecurityException rejection = null;
        for (OciDescriptor provenanceRef : provenanceRefs) {
            try {
                byte[] envelope = reader.read(provenanceRef);
                String digest = BundleProvenance.verify(bundle, sbom, envelope,
                        trustedPublicKey.toPath(), expectedBundleSigner);
                return new VerifiedBundle(bundle, digest);
            } catch (GeneralSecurityException | IOException e) {
                rejection = new GeneralSecurityException(
                        "Rejected dependency-bundle provenance referrer "
                                + provenanceRef.getDigest(), e);
            }
        }
        throw new GeneralSecurityException(
                "No dependency-bundle provenance referrer was signed by the trusted signer",
                rejection);
    }

    private record VerifiedBundle(DependencyBundle.Content bundle, String sbomDigest) {}

    @FunctionalInterface
    private interface ReferrerReader {
        byte[] read(OciDescriptor descriptor) throws IOException, InterruptedException;
    }

    protected static String managedBundleSourceRepository(String reference,
                                                        String targetRegistry) {
        try {
            if (Files.isDirectory(Path.of(reference))) {
                return null;
            }
        } catch (InvalidPathException ignored) {
            // Continue parsing the registry reference.
        }
        String[] parts = RegistryClient.splitRef(reference);
        return targetRegistry.equals(parts[0]) ? parts[1] : null;
    }

}
