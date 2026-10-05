// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import com.fasterxml.jackson.databind.JsonNode;
import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.plugin.MojoFailureException;
import org.apache.maven.plugins.annotations.Mojo;
import org.apache.maven.plugins.annotations.Parameter;
import org.apache.maven.plugins.annotations.ResolutionScope;

import java.io.File;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import java.util.Objects;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.TimeoutException;
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

    /** Maximum seconds for apply, and separately for readiness including kubectl calls. */
    @Parameter(property = "brewlet.waitTimeout", defaultValue = "300")
    int waitTimeout = 300;

    static final long POLL_INTERVAL_MILLIS = 3_000;
    static final long HEARTBEAT_MILLIS = 30_000;

    /** Runs {@code kubectl} with the given arguments. */
    @FunctionalInterface
    interface KubectlRunner {
        Result run(List<String> args, long timeoutMillis)
                throws IOException, InterruptedException, TimeoutException;
    }

    record Result(int exitCode, String stdout, String stderr) {}

    @FunctionalInterface
    interface Sleeper {
        void sleep(long millis) throws InterruptedException;
    }

    KubectlRunner kubectlRunner = this::runKubectl;
    Sleeper sleeper = Thread::sleep;
    LongSupplier clock = () -> TimeUnit.NANOSECONDS.toMillis(System.nanoTime());

    @Override
    protected void doExecute() throws MojoExecutionException, MojoFailureException {
        validateTimeout();
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
        applyManifest(manifest);

        if (!waitForReady) {
            getLog().info("Brewlet: not waiting for readiness (brewlet.wait=false). Check with: "
                    + "kubectl get javaapplication " + appName + " -n " + namespace);
            return;
        }
        awaitReady();
    }

    void applyManifest(File manifest) throws MojoExecutionException {
        validateTimeout();
        Result applied;
        try {
            applied = kubectl(TimeUnit.SECONDS.toMillis(waitTimeout), "apply", "-f", manifest.getPath());
        } catch (TimeoutException e) {
            throw new MojoExecutionException("kubectl apply timed out after " + waitTimeout
                    + "s. The manifest may have been partially applied; inspect namespace "
                    + namespace + " before retrying. Adjust -Dbrewlet.waitTimeout if needed.", e);
        }
        if (applied.exitCode() != 0) {
            throw new MojoExecutionException("kubectl apply failed (exit " + applied.exitCode()
                    + "): " + firstNonBlank(applied.stderr(), applied.stdout()).trim());
        }
        for (String line : applied.stdout().split("\\R")) {
            if (!line.isBlank()) {
                getLog().info("  " + line.trim());
            }
        }
    }

    /**
     * Polls the JavaApplication until its {@code Ready} condition is True for the
     * current generation, logging each status change and a periodic heartbeat.
     */
    void awaitReady() throws MojoExecutionException, MojoFailureException {
        validateTimeout();
        getLog().info("Brewlet: waiting up to " + waitTimeout + "s for JavaApplication "
                + namespace + "/" + appName + " to become Ready ...");
        long start = clock.getAsLong();
        long deadline = start + TimeUnit.SECONDS.toMillis(waitTimeout);
        long lastLog = start;
        String lastState = null;
        String lastDetail = "no status reported yet";
        while (true) {
            long remaining = deadline - clock.getAsLong();
            if (remaining <= 0) {
                throw readinessTimeout(lastDetail);
            }
            Result got;
            try {
                got = kubectl(remaining, "get", "javaapplication", appName, "-o", "json");
            } catch (TimeoutException e) {
                throw readinessTimeout(lastDetail + "; kubectl get timed out");
            }
            long now = clock.getAsLong();
            if (now >= deadline) {
                throw readinessTimeout(lastDetail);
            }
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
            try {
                sleeper.sleep(Math.min(POLL_INTERVAL_MILLIS,
                        Math.max(0, deadline - clock.getAsLong())));
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
                throw new MojoExecutionException("Interrupted while waiting for " + appName, e);
            }
        }
    }

    private void validateTimeout() throws MojoExecutionException {
        if (waitTimeout <= 0) {
            throw new MojoExecutionException("brewlet.waitTimeout must be greater than zero (seconds).");
        }
    }

    private MojoFailureException readinessTimeout(String detail) {
        return new MojoFailureException("JavaApplication " + namespace + "/" + appName
                + " was not Ready after " + waitTimeout + "s: " + detail
                + ". Inspect it with: kubectl describe javaapplication " + appName
                + " -n " + namespace);
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

    private Result kubectl(long timeoutMillis, String... args)
            throws MojoExecutionException, TimeoutException {
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
            return kubectlRunner.run(command, timeoutMillis);
        } catch (IOException e) {
            throw new MojoExecutionException("Failed to run " + kubectl + ": " + e.getMessage()
                    + ". Install kubectl or set -Dbrewlet.kubectl=/path/to/kubectl.", e);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            throw new MojoExecutionException("Interrupted while running " + kubectl, e);
        }
    }

    Result runKubectl(List<String> args, long timeoutMillis)
            throws IOException, InterruptedException, TimeoutException {
        List<String> command = new ArrayList<>();
        command.add(kubectl);
        command.addAll(args);
        long deadline = System.nanoTime() + TimeUnit.MILLISECONDS.toNanos(timeoutMillis);
        Path output = Files.createTempDirectory("brewlet-kubectl-");
        Path stdout = output.resolve("stdout");
        Path stderr = output.resolve("stderr");
        try {
            // Files avoid pipe backpressure and EOF waits on credential-plugin descendants.
            Process process = new ProcessBuilder(command)
                    .redirectOutput(stdout.toFile()).redirectError(stderr.toFile()).start();
            try {
                process.getOutputStream().close();
                if (!process.waitFor(Math.max(0, deadline - System.nanoTime()), TimeUnit.NANOSECONDS)) {
                    throw new TimeoutException("kubectl exceeded its execution deadline");
                }
                return new Result(process.exitValue(), Files.readString(stdout, StandardCharsets.UTF_8),
                        Files.readString(stderr, StandardCharsets.UTF_8));
            } finally {
                terminate(process);
            }
        } finally {
            Files.deleteIfExists(stdout);
            Files.deleteIfExists(stderr);
            Files.deleteIfExists(output);
        }
    }

    private static void terminate(Process process) throws IOException {
        if (!process.isAlive()) return;
        boolean interrupted = Thread.interrupted();
        try {
            process.descendants().forEach(ProcessHandle::destroyForcibly);
            process.destroyForcibly();
            long deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(5);
            while (process.isAlive() && System.nanoTime() < deadline) {
                try {
                    process.waitFor(Math.max(0, deadline - System.nanoTime()), TimeUnit.NANOSECONDS);
                } catch (InterruptedException e) {
                    interrupted = true;
                }
            }
            if (process.isAlive()) {
                throw new IOException("Could not terminate kubectl process " + process.pid());
            }
        } finally {
            if (interrupted) Thread.currentThread().interrupt();
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
