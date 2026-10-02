// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import com.fasterxml.jackson.databind.JsonNode;
import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.plugin.MojoFailureException;
import org.apache.maven.plugins.annotations.Mojo;
import org.apache.maven.plugins.annotations.Parameter;
import org.apache.maven.plugins.annotations.ResolutionScope;

import java.io.ByteArrayOutputStream;
import java.io.File;
import java.io.IOException;
import java.io.InputStream;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;
import java.util.Objects;
import java.util.concurrent.TimeUnit;
import java.util.function.LongSupplier;

/**
 * <strong>brewlet:deploy</strong> — Push the application, generate its
 * {@code JavaApplication} manifest with the digest-pinned image, apply it with
 * {@code kubectl}, and wait until the rollout is Ready.
 *
 * <p>Example:
 * <pre>
 * mvn package brewlet:deploy -Dbrewlet.registry=myregistry.azurecr.io
 * </pre>
 *
 * <p>Uses the current {@code kubectl} context unless {@code <kubeconfig>} or
 * {@code <kubeContext>} are set.
 */
@Mojo(name = "deploy",
      requiresProject = true,
      requiresDependencyResolution = ResolutionScope.RUNTIME,
      threadSafe = true)
public class DeployMojo extends ManifestMojo {

    /** kubeconfig file for {@code kubectl}. Defaults to kubectl's own resolution. */
    @Parameter(property = "brewlet.kubeconfig")
    File kubeconfig;

    /** kubeconfig context for {@code kubectl}. Defaults to the current context. */
    @Parameter(property = "brewlet.kubeContext")
    String kubeContext;

    /** {@code kubectl} executable. */
    @Parameter(property = "brewlet.kubectl", defaultValue = "kubectl")
    String kubectl = "kubectl";

    /** Wait for the JavaApplication to report {@code Ready=True} after applying. */
    @Parameter(property = "brewlet.wait", defaultValue = "true")
    boolean waitForReady = true;

    /** Maximum time, in seconds, to wait for the JavaApplication to become Ready. */
    @Parameter(property = "brewlet.waitTimeout", defaultValue = "300")
    int waitTimeout = 300;

    static final long POLL_INTERVAL_MILLIS = 3_000;
    static final long HEARTBEAT_MILLIS = 30_000;

    /** Runs {@code kubectl} with the given arguments. */
    @FunctionalInterface
    interface KubectlRunner {
        Result run(List<String> args) throws IOException, InterruptedException;
    }

    record Result(int exitCode, String stdout, String stderr) {}

    @FunctionalInterface
    interface Sleeper {
        void sleep(long millis) throws InterruptedException;
    }

    KubectlRunner kubectlRunner = this::runKubectl;
    Sleeper sleeper = Thread::sleep;
    LongSupplier clock = System::currentTimeMillis;

    @Override
    protected void doExecute() throws MojoExecutionException, MojoFailureException {
        PushResult pushed = pushApplication("deploy");
        if (pushed == null) {
            getLog().info("Brewlet: dry-run mode — would write the JavaApplication manifest, "
                    + "apply it to namespace " + namespace
                    + (waitForReady ? " and wait for it to become Ready" : ""));
            return;
        }
        File manifest = writeManifest(pushed.deployImage());

        getLog().info("Brewlet: applying " + manifest.getName() + " to namespace " + namespace
                + describeTarget() + " ...");
        Result applied = kubectl("apply", "-f", manifest.getPath());
        if (applied.exitCode() != 0) {
            throw new MojoExecutionException("kubectl apply failed (exit " + applied.exitCode()
                    + "): " + firstNonBlank(applied.stderr(), applied.stdout()).trim());
        }
        for (String line : applied.stdout().split("\\R")) {
            if (!line.isBlank()) {
                getLog().info("  " + line.trim());
            }
        }

        if (!waitForReady) {
            getLog().info("Brewlet: not waiting for readiness (brewlet.wait=false). Check with: "
                    + "kubectl get javaapplication " + appName + " -n " + namespace);
            return;
        }
        awaitReady();
    }

    /**
     * Polls the JavaApplication until its {@code Ready} condition is True for the
     * current generation, logging each status change and a periodic heartbeat.
     */
    void awaitReady() throws MojoExecutionException, MojoFailureException {
        getLog().info("Brewlet: waiting up to " + waitTimeout + "s for JavaApplication "
                + namespace + "/" + appName + " to become Ready ...");
        long start = clock.getAsLong();
        long deadline = start + TimeUnit.SECONDS.toMillis(waitTimeout);
        long lastLog = start;
        String lastState = null;
        String lastDetail = "no status reported yet";
        while (true) {
            Result got = kubectl("get", "javaapplication", appName, "-o", "json");
            long now = clock.getAsLong();
            String elapsed = "[" + TimeUnit.MILLISECONDS.toSeconds(now - start) + "s]";
            if (got.exitCode() == 0) {
                Status status = Status.parse(got.stdout());
                if (status.ready()) {
                    getLog().info("  " + elapsed + " Ready: " + status.detail());
                    getLog().info("Brewlet: " + appName + " is Ready in namespace " + namespace
                            + (status.selectedJdk() != null ? " (JDK " + status.selectedJdk() + ")" : ""));
                    return;
                }
                lastDetail = status.detail();
                String state = status.state();
                if (!state.equals(lastState)) {
                    getLog().info("  " + elapsed + " " + state);
                    lastState = state;
                    lastLog = now;
                } else if (now - lastLog >= HEARTBEAT_MILLIS) {
                    getLog().info("  " + elapsed + " still waiting — " + state);
                    lastLog = now;
                }
            } else {
                lastDetail = firstNonBlank(got.stderr(), got.stdout()).trim();
                if (now - lastLog >= HEARTBEAT_MILLIS) {
                    getLog().info("  " + elapsed + " still waiting — " + lastDetail);
                    lastLog = now;
                }
            }
            if (now >= deadline) {
                throw new MojoFailureException("JavaApplication " + namespace + "/" + appName
                        + " was not Ready after " + waitTimeout + "s: " + lastDetail
                        + ". Inspect it with: kubectl describe javaapplication " + appName
                        + " -n " + namespace);
            }
            try {
                sleeper.sleep(POLL_INTERVAL_MILLIS);
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
                throw new MojoExecutionException("Interrupted while waiting for " + appName, e);
            }
        }
    }

    /** Readiness view of a JavaApplication's status. */
    record Status(boolean ready, String reason, String message, long readyReplicas,
                  String selectedJdk) {

        static Status parse(String json) throws MojoExecutionException {
            JsonNode root;
            try {
                root = MAPPER.readTree(json);
            } catch (IOException e) {
                throw new MojoExecutionException("Unexpected kubectl output: " + e.getMessage(), e);
            }
            long generation = root.path("metadata").path("generation").asLong(0);
            JsonNode status = root.path("status");
            long observed = status.path("observedGeneration").asLong(0);
            long readyReplicas = status.path("readyReplicas").asLong(0);
            String selectedJdk = status.hasNonNull("selectedJdk")
                    ? status.get("selectedJdk").asText() : null;
            if (observed < generation) {
                return new Status(false, "Pending", "waiting for the Brewlet operator to "
                        + "reconcile generation " + generation, readyReplicas, selectedJdk);
            }
            for (JsonNode condition : status.path("conditions")) {
                if (!"Ready".equals(condition.path("type").asText())) {
                    continue;
                }
                boolean current = condition.path("observedGeneration").asLong(observed) >= generation;
                boolean ready = current && "True".equals(condition.path("status").asText());
                return new Status(ready, condition.path("reason").asText("Unknown"),
                        condition.path("message").asText(""), readyReplicas, selectedJdk);
            }
            return new Status(false, "Pending", "no Ready condition reported yet",
                    readyReplicas, selectedJdk);
        }

        String detail() {
            String text = message.isBlank() ? reason : reason + ": " + message;
            return text + " (ready replicas: " + readyReplicas + ")";
        }

        /** Change-detection key: reason and message, without the replica count. */
        String state() {
            return message.isBlank() ? reason : reason + ": " + message;
        }
    }

    private Result kubectl(String... args) throws MojoExecutionException {
        List<String> command = new ArrayList<>();
        if (kubeconfig != null) {
            command.add("--kubeconfig");
            command.add(kubeconfig.getPath());
        }
        if (kubeContext != null && !kubeContext.isBlank()) {
            command.add("--context");
            command.add(kubeContext);
        }
        command.add("--namespace");
        command.add(namespace);
        command.addAll(List.of(args));
        try {
            return kubectlRunner.run(command);
        } catch (IOException e) {
            throw new MojoExecutionException("Failed to run " + kubectl + ": " + e.getMessage()
                    + ". Install kubectl or set -Dbrewlet.kubectl=/path/to/kubectl.", e);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            throw new MojoExecutionException("Interrupted while running " + kubectl, e);
        }
    }

    private Result runKubectl(List<String> args) throws IOException, InterruptedException {
        List<String> command = new ArrayList<>();
        command.add(kubectl);
        command.addAll(args);
        Process process = new ProcessBuilder(command).start();
        process.getOutputStream().close();
        ByteArrayOutputStream stderr = new ByteArrayOutputStream();
        Thread errReader = new Thread(() -> drain(process.getErrorStream(), stderr),
                "kubectl-stderr");
        errReader.setDaemon(true);
        errReader.start();
        ByteArrayOutputStream stdout = new ByteArrayOutputStream();
        drain(process.getInputStream(), stdout);
        int exit = process.waitFor();
        errReader.join(TimeUnit.SECONDS.toMillis(5));
        return new Result(exit, stdout.toString(StandardCharsets.UTF_8),
                stderr.toString(StandardCharsets.UTF_8));
    }

    private static void drain(InputStream in, ByteArrayOutputStream out) {
        try (in) {
            in.transferTo(out);
        } catch (IOException ignored) {
            // The exit status reports the failure.
        }
    }

    private String describeTarget() {
        StringBuilder target = new StringBuilder();
        if (kubeContext != null && !kubeContext.isBlank()) {
            target.append(" (context ").append(kubeContext).append(')');
        }
        if (kubeconfig != null) {
            target.append(" (kubeconfig ").append(kubeconfig.getPath()).append(')');
        }
        return target.toString();
    }

    private static String firstNonBlank(String a, String b) {
        return a != null && !a.isBlank() ? a : Objects.requireNonNullElse(b, "");
    }
}
