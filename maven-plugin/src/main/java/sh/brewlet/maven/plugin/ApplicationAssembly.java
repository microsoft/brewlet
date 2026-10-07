// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import org.apache.maven.project.MavenProject;
import sh.brewlet.maven.plugin.oci.ApplicationImage;
import sh.brewlet.maven.plugin.oci.OciReferrer;

/** Invocation-local assembly; publishing and local writes never replace its bytes. */
final class ApplicationAssembly {
    private static final String KEY = ApplicationAssembly.class.getName();

    final String fingerprint;
    final ApplicationImage image;
    final ApplicationBuildResult result;
    final String managedLayerDigest;
    final OciReferrer.Content attestation;
    boolean published;

    ApplicationAssembly(String fingerprint, ApplicationImage image, ApplicationBuildResult result,
                        String managedLayerDigest, OciReferrer.Content attestation) {
        this.fingerprint = fingerprint;
        this.image = image;
        this.result = result;
        this.managedLayerDigest = managedLayerDigest;
        this.attestation = attestation;
    }

    static ApplicationAssembly get(MavenProject project) {
        Object value = project.getContextValue(KEY);
        return value instanceof ApplicationAssembly assembly ? assembly : null;
    }

    void save(MavenProject project) {
        project.setContextValue(KEY, this);
    }
}
