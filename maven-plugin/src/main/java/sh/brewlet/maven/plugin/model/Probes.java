// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin.model;

/** The {@code <probes>} configuration: optional readiness and liveness probes. */
public class Probes {

    private Probe readiness;
    private Probe liveness;

    public Probes() {}

    public Probes(Probe readiness, Probe liveness) {
        this.readiness = readiness;
        this.liveness = liveness;
    }

    public Probe getReadiness() { return readiness; }
    public void setReadiness(Probe readiness) { this.readiness = readiness; }

    public Probe getLiveness() { return liveness; }
    public void setLiveness(Probe liveness) { this.liveness = liveness; }
}
