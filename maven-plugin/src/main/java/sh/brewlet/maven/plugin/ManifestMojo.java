// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.execution.MavenSession;
import org.apache.maven.plugin.AbstractMojo;
import org.apache.maven.plugins.annotations.Component;
import org.apache.maven.plugins.annotations.Mojo;
import org.apache.maven.plugins.annotations.Parameter;
import org.apache.maven.project.MavenProject;
import org.apache.maven.toolchain.ToolchainManager;
import sh.brewlet.maven.plugin.model.JvmConfig;
import sh.brewlet.maven.plugin.model.Port;
import sh.brewlet.maven.plugin.model.Probe;
import sh.brewlet.maven.plugin.model.Probes;
import sh.brewlet.maven.plugin.oci.RegistryClient;
import sh.brewlet.maven.plugin.util.FrameworkDetector;
import sh.brewlet.maven.plugin.util.JdkVersionResolver;

import java.io.File;
import java.io.IOException;
import java.io.PrintWriter;
import java.io.StringWriter;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.util.HexFormat;
import java.util.List;

/**
 * <strong>brewlet:manifest</strong> — Emit a {@code JavaApplication} custom
 * resource YAML to
 * {@code target/brewlet/} so developers can apply it with
 * {@code kubectl apply -f target/brewlet/javaapplication.yaml}.
 *
 * <p>The generated CR uses the JavaApplication schema and examples maintained in
 * https://github.com/microsoft/brewlet/tree/main/kubernetes.
 *
 * <p>Development-time output only. The image must come from {@code brewlet:build}
 * or {@code brewlet:push} for this project in the same Maven invocation.
 * No explicit image input, saved push result, publishing or cluster access.
 *
 * <p>Example:
 * <pre>
 * mvn package brewlet:push brewlet:manifest
 * kubectl apply -f target/brewlet/javaapplication.yaml
 * </pre>
 */
@Mojo(name = "manifest",
      requiresProject = true,
      threadSafe = true)
public class ManifestMojo extends AbstractMojo {

    @Parameter(defaultValue = "${project}", readonly = true, required = true)
    MavenProject project;

    @Parameter(defaultValue = "${session}", readonly = true, required = true)
    MavenSession session;

    @Component
    ToolchainManager toolchainManager;

    /** Directory for the generated development manifest. */
    @Parameter(defaultValue = "${project.build.directory}/brewlet")
    File outputDirectory;

    /** Skip generation. */
    @Parameter(property = "brewlet.skip", defaultValue = "false")
    boolean skip;

    /** Render to the build log without writing a manifest. */
    @Parameter(property = "brewlet.dryRun", defaultValue = "false")
    boolean dryRun;

    /** Development runtime feature, inferred from the main compiler settings. */
    @Parameter(property = "brewlet.jdkFeature")
    Integer jdkFeature;

    /** Optional development runtime distribution. */
    @Parameter(property = "brewlet.jdkDistribution")
    String jdkDistribution;

    /** Development launcher, defaulting to java. */
    @Parameter(property = "brewlet.launcher", defaultValue = "java")
    String launcher;

    /**
     * Kubernetes namespace for the generated manifest.
     * Defaults to {@code default}.
     */
    @Parameter(property = "brewlet.namespace", defaultValue = "default")
    String namespace;

    /**
     * Application name for the {@code JavaApplication} resource.
     * Defaults to {@code ${project.artifactId}}.
     */
    @Parameter(property = "brewlet.appName", defaultValue = "${project.artifactId}")
    String appName;

    /**
     * Number of replicas. Defaults to {@code 1}.
     */
    @Parameter(property = "brewlet.replicas", defaultValue = "1")
    private int replicas;

    /**
     * Deployment-side JVM tuning written to the descriptor's {@code jvm.args}
     * (heap, GC, container flags, agents, …). This is a DEPLOYMENT concern and
     * is intentionally separate from the artifact's app-intrinsic launch knobs
     * (enablePreview/addOpens/addExports/addModules/systemProperties). The
     * operator appends these after the artifact flags.
     */
    @Parameter(property = "brewlet.jvmArgs")
    private List<String> jvmArgs;

    /**
     * Container ports the application listens on, written to the descriptor's
     * {@code spec.ports}. This is a DEPLOYMENT concern (Kubernetes Service
     * wiring) and is intentionally not part of the artifact. Ports do not
     * declare health endpoints; configure {@code <probes>} for that. If empty
     * and the framework is Spring Boot or Quarkus, a default of
     * {@code 8080/http} is used with a warning.
     */
    @Parameter
    private List<Port> ports;

    /**
     * Readiness and liveness probes written to {@code spec.probes}. Each probe
     * is an HTTP GET when {@code <path>} is set, an exec probe when
     * {@code <command>} is set, and otherwise a TCP socket check. HTTP and TCP
     * probes target {@code <port>} (a port name or number), defaulting to the
     * first configured port. Probes are never inferred from ports or framework.
     */
    @Parameter
    private Probes probes;

    /**
     * Shorthand for an HTTP GET readiness probe on this path against the first
     * port, e.g. {@code -Dbrewlet.readinessPath=/healthz}. Ignored when
     * {@code <probes><readiness>} is configured.
     */
    @Parameter(property = "brewlet.readinessPath")
    private String readinessPath;

    /**
     * Shorthand for an HTTP GET liveness probe on this path against the first
     * port. Ignored when {@code <probes><liveness>} is configured.
     */
    @Parameter(property = "brewlet.livenessPath")
    private String livenessPath;

    /**
     * CPU request for the pod. Defaults to {@code "500m"}.
     */
    @Parameter(property = "brewlet.resources.cpuRequest", defaultValue = "500m")
    private String cpuRequest;

    /**
     * Memory request for the pod. Defaults to {@code "256Mi"}.
     */
    @Parameter(property = "brewlet.resources.memoryRequest", defaultValue = "256Mi")
    private String memoryRequest;

    /**
     * CPU limit for the pod. Defaults to {@code "2"}.
     */
    @Parameter(property = "brewlet.resources.cpuLimit", defaultValue = "2")
    private String cpuLimit;

    /**
     * Memory limit for the pod. Defaults to {@code "512Mi"}.
     */
    @Parameter(property = "brewlet.resources.memoryLimit", defaultValue = "512Mi")
    private String memoryLimit;

    @Override
    public void execute() throws MojoExecutionException {
        if (skip) {
            getLog().info("Brewlet: skipping (brewlet.skip=true)");
            return;
        }
        ApplicationBuildResult built = requireBuildResult();
        ApplicationAssembly assembly = ApplicationAssembly.get(project);
        if (assembly != null && !assembly.published) {
            getLog().warn("Brewlet: this image has not been published by this invocation. "
                    + "Publish or load the exact built image before applying the manifest.");
        }
        StringWriter rendered = new StringWriter();
        try (PrintWriter writer = new PrintWriter(rendered)) {
            writeJavaApplicationYaml(writer, built.config(), resolveDeployImage());
        } catch (IOException e) {
            throw new MojoExecutionException("Failed to render JavaApplication manifest", e);
        }
        if (dryRun) {
            getLog().info("Brewlet: dry-run development manifest (not written):\n" + rendered);
            return;
        }
        File outputFile = new File(outputDirectory, "javaapplication.yaml");
        try {
            Files.createDirectories(outputDirectory.toPath());
            Files.writeString(outputFile.toPath(), rendered.toString(), StandardCharsets.UTF_8);
        } catch (IOException e) {
            throw new MojoExecutionException("Failed to write JavaApplication manifest", e);
        }
        getLog().info("Brewlet: wrote development JavaApplication manifest -> " + outputFile.getPath());
        getLog().info("  Apply separately with: kubectl apply -f " + outputFile.getPath());
    }

    private ApplicationBuildResult requireBuildResult() throws MojoExecutionException {
        ApplicationBuildResult built = ApplicationBuildResult.get(project);
        if (built == null) {
            throw new MojoExecutionException("brewlet:manifest requires this project's image from "
                    + "brewlet:build or brewlet:push in the same Maven invocation. Run "
                    + "'mvn package brewlet:push brewlet:manifest' (or use brewlet:build). "
                    + "A dry-run push does not produce an image; remove -Dbrewlet.dryRun=true "
                    + "to publish, or use brewlet:build for a local image and manifest preview. "
                    + "Explicit images and saved push.json files are not inputs.");
        }
        if (!"image".equals(built.format())) {
            throw new MojoExecutionException("brewlet:manifest requires a runnable image; "
                    + "build or push with -Dbrewlet.format=image, not artifact.");
        }
        return built;
    }

    String resolveDeployImage() throws MojoExecutionException {
        ApplicationBuildResult built = requireBuildResult();
        if (!RegistryClient.hasExplicitRegistry(built.reference())) {
            throw new MojoExecutionException("The built image has no registry host. Configure "
                    + "<registry> or a registry-qualified publishing <image> on brewlet:build/brewlet:push.");
        }
        String pinned = RegistryClient.pinReference(built.reference(), built.digest());
        if (!RegistryClient.isDigestPinnedReference(pinned)) {
            throw new MojoExecutionException("The current build did not produce a valid image digest.");
        }
        return pinned;
    }

    void writeJavaApplicationYaml(PrintWriter w, JvmConfig cfg) throws IOException, MojoExecutionException {
        writeJavaApplicationYaml(w, cfg, resolveDeployImage());
    }

    private void writeJavaApplicationYaml(PrintWriter w, JvmConfig cfg, String deployImage)
            throws IOException, MojoExecutionException {
        int feature = JdkVersionResolver.resolve(project, session, toolchainManager, getLog(), jdkFeature);
        List<Port> resolvedPorts = resolvePorts();
        Probe readiness = effectiveProbe(probes == null ? null : probes.getReadiness(), readinessPath);
        Probe liveness = effectiveProbe(probes == null ? null : probes.getLiveness(), livenessPath);
        validateProbe("readiness", readiness, resolvedPorts);
        validateProbe("liveness", liveness, resolvedPorts);
        w.println("# Generated by brewlet-maven-plugin " + yamlString(project.getVersion()));
        w.println("# Development starting point; review before applying.");
        w.println("# Apply with: kubectl apply -f javaapplication.yaml");
        w.println("---");
        w.println("apiVersion: apps.brewlet.sh/v1alpha1");
        w.println("kind: JavaApplication");
        w.println("metadata:");
        w.println("  name: " + yamlString(appName));
        w.println("  namespace: " + yamlString(namespace));
        w.println("  labels:");
        w.println("    app: " + yamlString(appName));
        w.println("    app.kubernetes.io/name: " + yamlString(appName));
        w.println("    app.kubernetes.io/version: " + yamlString(project.getVersion()));
        w.println("    app.kubernetes.io/managed-by: brewlet-maven-plugin");
        w.println("spec:");
        w.println("  artifact:");
        w.println("    image: " + yamlString(deployImage));
        w.println("    pullPolicy: IfNotPresent");
        w.println("  replicas: " + replicas);
        w.println("  resources:");
        w.println("    requests:");
        w.println("      cpu: " + yamlString(cpuRequest));
        w.println("      memory: " + yamlString(memoryRequest));
        w.println("    limits:");
        w.println("      cpu: " + yamlString(cpuLimit));
        w.println("      memory: " + yamlString(memoryLimit));
        w.println("  jvm:");
        w.println("    version: " + feature);
        String resolvedDistribution = isBlank(jdkDistribution) ? null : jdkDistribution.trim();
        if (resolvedDistribution != null) {
            w.println("    distribution: " + yamlString(resolvedDistribution));
        }
        String resolvedLauncher = isBlank(launcher) ? "java" : launcher.trim();
        if (!"java".equals(resolvedLauncher)) {
            w.println("    launcher: " + yamlString(resolvedLauncher));
        }

        if (jvmArgs != null && !jvmArgs.isEmpty()) {
            w.println("    args:");
            for (String arg : jvmArgs) {
                w.println("      - " + yamlString(arg));
            }
        }

        if (cfg.getEnv() != null && !cfg.getEnv().isEmpty()) {
            w.println("  env:");
            for (var envVar : cfg.getEnv()) {
                w.println("    - name: " + yamlString(envVar.getName()));
                w.println("      value: " + yamlString(envVar.getValue()));
            }
        }

        if (resolvedPorts != null && !resolvedPorts.isEmpty()) {
            w.println("  ports:");
            for (Port p : resolvedPorts) {
                w.println("    - name: " + yamlString(portName(p)));
                w.println("      containerPort: " + p.getContainerPort());
            }
            w.println("  service:");
            w.println("    enabled: true");
            w.println("    type: ClusterIP");
        }
        if (readiness != null || liveness != null) {
            w.println("  probes:");
            writeProbe(w, "readiness", readiness, resolvedPorts);
            writeProbe(w, "liveness", liveness, resolvedPorts);
        }
        if (readiness == null && resolvedPorts != null && !resolvedPorts.isEmpty()) {
            warnMissingReadiness();
        }
        if (w.checkError()) {
            throw new IOException("Failed to write JavaApplication YAML");
        }
    }

    private static String portName(Port p) {
        return p.getName() != null ? p.getName() : "http";
    }

    private static Probe effectiveProbe(Probe configured, String shorthandPath) {
        if (configured != null) {
            return configured;
        }
        return isBlank(shorthandPath) ? null : Probe.httpGet(shorthandPath.trim());
    }

    private static boolean isBlank(String s) {
        return s == null || s.isBlank();
    }

    private static void validateProbe(String kind, Probe probe, List<Port> resolvedPorts)
            throws MojoExecutionException {
        if (probe == null) {
            return;
        }
        String where = "<probes><" + kind + ">";
        boolean http = !isBlank(probe.getPath());
        boolean exec = probe.getCommand() != null && !probe.getCommand().isEmpty();
        if (http && exec) {
            throw new MojoExecutionException(where + " sets both <path> (HTTP GET) and <command> (exec); choose one.");
        }
        if (http && !probe.getPath().trim().startsWith("/")) {
            throw new MojoExecutionException(where + "<path> must start with '/': " + probe.getPath());
        }
        if (exec) {
            if (probe.getCommand().stream().anyMatch(ManifestMojo::isBlank)) {
                throw new MojoExecutionException(where + "<command> must not contain empty arguments.");
            }
            if (!isBlank(probe.getPort()) || !isBlank(probe.getScheme())) {
                throw new MojoExecutionException(where + " is an exec probe; <port> and <scheme> do not apply.");
            }
        } else {
            resolveProbePort(where, probe, resolvedPorts);
        }
        if (!isBlank(probe.getScheme())) {
            if (!http) {
                throw new MojoExecutionException(where + "<scheme> applies only to HTTP probes (set <path>).");
            }
            String scheme = probe.getScheme().trim().toUpperCase(java.util.Locale.ROOT);
            if (!scheme.equals("HTTP") && !scheme.equals("HTTPS")) {
                throw new MojoExecutionException(where + "<scheme> must be HTTP or HTTPS: " + probe.getScheme());
            }
        }
        requireAtLeast(where, "initialDelaySeconds", probe.getInitialDelaySeconds(), 0);
        requireAtLeast(where, "periodSeconds", probe.getPeriodSeconds(), 1);
        requireAtLeast(where, "timeoutSeconds", probe.getTimeoutSeconds(), 1);
        requireAtLeast(where, "failureThreshold", probe.getFailureThreshold(), 1);
    }

    private static void requireAtLeast(String where, String field, Integer value, int min)
            throws MojoExecutionException {
        if (value != null && value < min) {
            throw new MojoExecutionException(where + "<" + field + "> must be >= " + min + ": " + value);
        }
    }

    /** Returns the probe port as an Integer (number) or String (port name). */
    private static Object resolveProbePort(String where, Probe probe, List<Port> resolvedPorts)
            throws MojoExecutionException {
        String port = probe.getPort() == null ? null : probe.getPort().trim();
        if (isBlank(port)) {
            if (resolvedPorts == null || resolvedPorts.isEmpty()) {
                throw new MojoExecutionException(where + " needs a <port>, or configure <ports> so the "
                        + "probe can target the first one.");
            }
            return portName(resolvedPorts.get(0));
        }
        if (port.chars().allMatch(Character::isDigit)) {
            int number;
            try {
                number = Integer.parseInt(port);
            } catch (NumberFormatException e) {
                number = -1;
            }
            if (number < 1 || number > 65535) {
                throw new MojoExecutionException(where + "<port> must be 1-65535: " + port);
            }
            return number;
        }
        if (resolvedPorts == null || resolvedPorts.stream().noneMatch(p -> portName(p).equals(port))) {
            throw new MojoExecutionException(where + "<port> '" + port + "' does not match a configured "
                    + "port name" + (resolvedPorts == null || resolvedPorts.isEmpty() ? "" : " ("
                    + String.join(", ", resolvedPorts.stream().map(ManifestMojo::portName).toList()) + ")")
                    + "; use a configured name or a port number.");
        }
        return port;
    }

    private static void writeProbe(PrintWriter w, String kind, Probe probe, List<Port> resolvedPorts)
            throws IOException, MojoExecutionException {
        if (probe == null) {
            return;
        }
        w.println("    " + kind + ":");
        if (probe.getCommand() != null && !probe.getCommand().isEmpty()) {
            w.println("      exec:");
            w.println("        command:");
            for (String arg : probe.getCommand()) {
                w.println("          - " + yamlString(arg));
            }
        } else {
            Object port = resolveProbePort("<probes><" + kind + ">", probe, resolvedPorts);
            String portValue = port instanceof Integer ? port.toString() : yamlString((String) port);
            if (!isBlank(probe.getPath())) {
                w.println("      httpGet:");
                w.println("        path: " + yamlString(probe.getPath().trim()));
                w.println("        port: " + portValue);
                if (!isBlank(probe.getScheme())) {
                    w.println("        scheme: " + yamlString(
                            probe.getScheme().trim().toUpperCase(java.util.Locale.ROOT)));
                }
            } else {
                w.println("      tcpSocket:");
                w.println("        port: " + portValue);
            }
        }
        writeOptionalInt(w, "initialDelaySeconds", probe.getInitialDelaySeconds());
        writeOptionalInt(w, "periodSeconds", probe.getPeriodSeconds());
        writeOptionalInt(w, "timeoutSeconds", probe.getTimeoutSeconds());
        writeOptionalInt(w, "failureThreshold", probe.getFailureThreshold());
    }

    private static void writeOptionalInt(PrintWriter w, String field, Integer value) {
        if (value != null) {
            w.println("      " + field + ": " + value);
        }
    }

    private void warnMissingReadiness() {
        String[] suggested = FrameworkDetector.suggestedHealthPaths(project);
        if (suggested != null) {
            getLog().warn("No readiness probe configured. This project exposes " + suggested[0]
                    + "; add -Dbrewlet.readinessPath=" + suggested[0]
                    + " or <probes><readiness><path>" + suggested[0] + "</path></readiness></probes>"
                    + " (liveness: " + suggested[1] + ").");
        } else {
            getLog().warn("No readiness probe configured; pods receive traffic as soon as the JVM starts. "
                    + "Add <probes><readiness><path>/your/health</path></readiness></probes> "
                    + "or -Dbrewlet.readinessPath=/your/health.");
        }
    }

    static String yamlString(String value) throws IOException {
        String json = AbstractBrewletMojo.MAPPER.writeValueAsString(String.valueOf(value));
        StringBuilder scalar = new StringBuilder(json.length());
        // YAML also folds Unicode line breaks and disallows raw C1 controls.
        // Keep supplementary Unicode intact: YAML does not accept surrogate escapes.
        for (int i = 0; i < json.length(); i++) {
            char c = json.charAt(i);
            if ((c >= 0x7f && c <= 0x9f) || c == 0x2028 || c == 0x2029 || c >= 0xfffe) {
                scalar.append("\\u").append(HexFormat.of().toHexDigits(c));
            } else {
                scalar.append(c);
            }
        }
        return scalar.toString();
    }

    /**
     * Resolves the container ports for the descriptor: the configured
     * {@code <ports>} if any, otherwise a framework-inferred default
     * ({@code 8080/http} for Spring Boot / Quarkus) with a warning.
     */
    private List<Port> resolvePorts() {
        if (ports != null && !ports.isEmpty()) {
            return ports;
        }
        FrameworkDetector.Framework framework = FrameworkDetector.detect(project);
        int defaultPort = FrameworkDetector.defaultPort(framework);
        if (defaultPort > 0) {
            getLog().warn("No <ports> configured; using default port " + defaultPort
                    + " for " + framework.label() + " application.");
            return List.of(new Port("http", defaultPort, "TCP"));
        }
        getLog().warn("No <ports> configured. Add <ports><port>"
                + "<name>http</name><containerPort>8080</containerPort></port></ports> "
                + "to the plugin configuration.");
        return null;
    }
}
