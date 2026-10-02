// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin.model;

import java.util.List;

/**
 * A readiness or liveness probe written to the {@code JavaApplication}'s
 * {@code spec.probes}. The probe kind follows from what is set: {@code path}
 * selects an HTTP GET, {@code command} an exec probe, and neither a TCP
 * socket check on {@code port}.
 *
 * <pre>
 * &lt;readiness&gt;
 *   &lt;path&gt;/healthz&lt;/path&gt;
 *   &lt;port&gt;http&lt;/port&gt;           &lt;!-- name or number; defaults to the first port --&gt;
 *   &lt;periodSeconds&gt;5&lt;/periodSeconds&gt;
 * &lt;/readiness&gt;
 * </pre>
 */
public class Probe {

    private String path;
    private String port;
    private String scheme;
    private List<String> command;
    private Integer initialDelaySeconds;
    private Integer periodSeconds;
    private Integer timeoutSeconds;
    private Integer failureThreshold;

    public Probe() {}

    public static Probe httpGet(String path) {
        Probe probe = new Probe();
        probe.path = path;
        return probe;
    }

    public String getPath() { return path; }
    public void setPath(String path) { this.path = path; }

    public String getPort() { return port; }
    public void setPort(String port) { this.port = port; }

    public String getScheme() { return scheme; }
    public void setScheme(String scheme) { this.scheme = scheme; }

    public List<String> getCommand() { return command; }
    public void setCommand(List<String> command) { this.command = command; }

    public Integer getInitialDelaySeconds() { return initialDelaySeconds; }
    public void setInitialDelaySeconds(Integer initialDelaySeconds) { this.initialDelaySeconds = initialDelaySeconds; }

    public Integer getPeriodSeconds() { return periodSeconds; }
    public void setPeriodSeconds(Integer periodSeconds) { this.periodSeconds = periodSeconds; }

    public Integer getTimeoutSeconds() { return timeoutSeconds; }
    public void setTimeoutSeconds(Integer timeoutSeconds) { this.timeoutSeconds = timeoutSeconds; }

    public Integer getFailureThreshold() { return failureThreshold; }
    public void setFailureThreshold(Integer failureThreshold) { this.failureThreshold = failureThreshold; }
}
