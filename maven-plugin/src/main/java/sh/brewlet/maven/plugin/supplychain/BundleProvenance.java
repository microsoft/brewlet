// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin.supplychain;

import com.fasterxml.jackson.databind.JsonNode;
import sh.brewlet.maven.plugin.oci.DependencyBundle;
import sh.brewlet.maven.plugin.oci.LocalStore;
import sh.brewlet.maven.plugin.oci.MediaTypes;
import sh.brewlet.maven.plugin.oci.OciReferrer;

import java.io.IOException;
import java.nio.file.Path;
import java.security.GeneralSecurityException;
import java.util.Iterator;
import java.util.Set;

/** Creates and validates the SBOM and signed provenance attached to a bundle. */
public final class BundleProvenance {
    private BundleProvenance() {}

    public record Materials(byte[] sbom, String sbomDigest,
                            OciReferrer.Content sbomReferrer,
                            byte[] envelope, OciReferrer.Content provenanceReferrer) {}

    public static Materials create(DependencyBundle.Content bundle, Path signingKey,
                                   String signerIdentity)
            throws IOException, GeneralSecurityException {
        byte[] sbom = CycloneDx.generate(bundle.lock(), bundle.config().getName(),
                bundle.config().getVersion());
        String sbomDigest = LocalStore.sha256Hex(sbom);
        OciReferrer.Content sbomReferrer = OciReferrer.build(
                bundle.manifestDigest(), bundle.manifestBytes().length,
                MediaTypes.OCI_MANIFEST_MEDIA_TYPE, MediaTypes.CYCLONEDX_ARTIFACT_TYPE,
                MediaTypes.CYCLONEDX_LAYER_MEDIA_TYPE, sbom, null);
        if (signingKey == null) {
            return new Materials(sbom, sbomDigest, sbomReferrer, null, null);
        }
        requireIdentity(signerIdentity);
        Predicates.Bundle predicate = new Predicates.Bundle(
                bundle.manifestDigest(), bundle.config().getLayerDigest(),
                bundle.config().getLockDigest(), sbomDigest,
                bundle.config().getSourceBom(), signerIdentity);
        InToto.Statement statement = new InToto.Statement(bundle.config().getName(),
                bundle.manifestDigest(), MediaTypes.DEPENDENCY_BUNDLE_PREDICATE_TYPE, predicate);
        byte[] envelope = CanonicalJson.bytes(Dsse.sign(CanonicalJson.bytes(statement), signingKey));
        OciReferrer.Content provenanceReferrer = OciReferrer.build(
                bundle.manifestDigest(), bundle.manifestBytes().length,
                MediaTypes.OCI_MANIFEST_MEDIA_TYPE, MediaTypes.DSSE_ARTIFACT_TYPE,
                MediaTypes.DSSE_LAYER_MEDIA_TYPE, envelope,
                MediaTypes.DEPENDENCY_BUNDLE_PREDICATE_TYPE);
        return new Materials(sbom, sbomDigest, sbomReferrer, envelope, provenanceReferrer);
    }

    public static String verify(DependencyBundle.Content bundle, byte[] sbom, byte[] envelope,
                                Path trustedPublicKey, String signerIdentity)
            throws IOException, GeneralSecurityException {
        requireIdentity(signerIdentity);
        if (trustedPublicKey == null) {
            throw new GeneralSecurityException("trustedPublicKey is required");
        }
        CycloneDx.validate(sbom, bundle.lock(), bundle.config().getName(),
                bundle.config().getVersion());
        String sbomDigest = LocalStore.sha256Hex(sbom);
        JsonNode statement = Dsse.verifyStatement(envelope, trustedPublicKey,
                bundle.manifestDigest(), MediaTypes.DEPENDENCY_BUNDLE_PREDICATE_TYPE,
                signerIdentity);
        JsonNode predicate = statement.path("predicate");
        requireKnownFields(predicate);
        if (predicate.path("schemaVersion").asInt() != 1) {
            throw new GeneralSecurityException("Unsupported bundle provenance schemaVersion");
        }
        require(predicate, "dependencyBundleDigest", bundle.manifestDigest());
        require(predicate, "dependencyLayerDigest", bundle.config().getLayerDigest());
        require(predicate, "dependencyLockDigest", bundle.config().getLockDigest());
        require(predicate, "sbomDigest", sbomDigest);
        require(predicate, "sourceBom", bundle.config().getSourceBom());
        return sbomDigest;
    }

    public static String validateSbom(DependencyBundle.Content bundle, byte[] sbom)
            throws IOException, GeneralSecurityException {
        CycloneDx.validate(sbom, bundle.lock(), bundle.config().getName(),
                bundle.config().getVersion());
        return LocalStore.sha256Hex(sbom);
    }

    /**
     * The exact field set of the dependency-bundle predicate, mirroring the Go
     * {@code attest.BundleProvenance} struct.
     */
    private static final Set<String> BUNDLE_PREDICATE_FIELDS = Set.of(
            "schemaVersion", "dependencyBundleDigest", "dependencyLayerDigest",
            "dependencyLockDigest", "sbomDigest", "sourceBom", "builderIdentity");

    /**
     * Rejects a predicate carrying any field outside {@link #BUNDLE_PREDICATE_FIELDS}.
     *
     * <p>The Go verifier decodes this predicate with {@code DisallowUnknownFields}.
     * Reading only the fields it knows about would make the Maven plugin accept a
     * bundle that the Go verifier rejects at admission — a publish-time versus
     * admission-time divergence, where a signed artifact passes every check the
     * publisher runs and is then refused in the cluster. Both sides must agree on
     * the exact field set.
     */
    private static void requireKnownFields(JsonNode predicate) throws GeneralSecurityException {
        if (!predicate.isObject()) {
            throw new GeneralSecurityException("Bundle provenance predicate must be an object");
        }
        Iterator<String> names = predicate.fieldNames();
        while (names.hasNext()) {
            String name = names.next();
            if (!BUNDLE_PREDICATE_FIELDS.contains(name)) {
                throw new GeneralSecurityException(
                        "Bundle provenance predicate has unknown field " + name);
            }
        }
    }

    private static void require(JsonNode predicate, String field, String expected)
            throws GeneralSecurityException {
        if (!expected.equals(predicate.path(field).asText())) {
            throw new GeneralSecurityException("Bundle provenance " + field + " mismatch");
        }
    }

    private static void requireIdentity(String identity) throws GeneralSecurityException {
        if (identity == null || identity.isBlank()) {
            throw new GeneralSecurityException("signerIdentity is required");
        }
    }
}
