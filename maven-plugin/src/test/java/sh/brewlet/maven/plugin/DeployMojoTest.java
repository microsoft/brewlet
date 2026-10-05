// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.plugin.MojoFailureException;
import org.apache.maven.plugin.logging.SystemStreamLog;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

import java.io.File;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Deque;
import java.util.List;
import java.util.ArrayDeque;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.TimeoutException;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.concurrent.atomic.AtomicReference;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

class DeployMojoTest {

    @TempDir Path root;
    private final List<List<String>> calls = new ArrayList<>();
    private final List<Long> timeouts = new ArrayList<>();
    private final Deque<DeployMojo.Result> responses = new ArrayDeque<>();
    private final List<String> logged = new ArrayList<>();
    private long now;

    private DeployMojo mojo() {
        DeployMojo mojo = new DeployMojo();
        mojo.namespace = "apps";
        mojo.appName = "orders";
        mojo.waitTimeout = 30;
        mojo.kubectlRunner = (args, timeout) -> {
            calls.add(args);
            timeouts.add(timeout);
            return responses.isEmpty() ? new DeployMojo.Result(0, status(1, 1, "False", "Progressing", "x"), "")
                    : responses.removeFirst();
        };
        mojo.sleeper = millis -> now += millis;
        mojo.clock = () -> now;
        mojo.setLog(new SystemStreamLog() {
            @Override
            public void info(CharSequence content) {
                logged.add(content.toString());
            }
        });
        return mojo;
    }

    private static String status(long generation, long observed, String ready, String reason,
                                 String message) {
        return "{\"metadata\":{\"generation\":" + generation + "},\"status\":{\"observedGeneration\":"
                + observed + ",\"readyReplicas\":1,\"selectedJdk\":\"temurin-21\",\"conditions\":[{"
                + "\"type\":\"Ready\",\"status\":\"" + ready + "\",\"reason\":\"" + reason
                + "\",\"message\":\"" + message + "\",\"observedGeneration\":" + observed + "}]}}";
    }

    private void respond(String json) {
        responses.add(new DeployMojo.Result(0, json, ""));
    }

    @Test
    void waitsThroughProgressUntilReadyForTheCurrentGeneration() throws Exception {
        DeployMojo mojo = mojo();
        respond("{\"metadata\":{\"generation\":2},\"status\":{\"observedGeneration\":1}}");
        respond(status(2, 2, "True", "Reconciled", "old")
                .replace("\"observedGeneration\":2}]", "\"observedGeneration\":1}]"));
        respond(status(2, 2, "False", "Progressing", "Rollout 0/1 ready"));
        respond(status(2, 2, "False", "Progressing", "Rollout 0/1 ready"));
        respond(status(2, 2, "True", "Reconciled", "Rollout complete"));

        mojo.awaitReady();

        assertEquals(5, calls.size());
        assertEquals(List.of("--namespace", "apps", "get", "javaapplication", "orders", "-o", "json"),
                calls.get(0));
        assertTrue(logged.stream().anyMatch(l -> l.contains("Pending")), logged.toString());
        assertEquals(1, logged.stream().filter(l -> l.contains("Progressing: Rollout 0/1 ready")).count(),
                "unchanged status is logged once: " + logged);
        assertTrue(logged.stream().anyMatch(l -> l.contains("is Ready") && l.contains("temurin-21")),
                logged.toString());
    }

    @Test
    void timesOutWithTheLastStatusAndADescribeHint() {
        DeployMojo mojo = mojo();
        mojo.waitTimeout = 10;

        MojoFailureException failure = assertThrows(MojoFailureException.class, mojo::awaitReady);

        assertTrue(failure.getMessage().contains("Progressing: x"), failure.getMessage());
        assertTrue(failure.getMessage().contains("kubectl describe javaapplication orders -n apps"),
                failure.getMessage());
        assertEquals(10_000, now);
        assertEquals(List.of(10_000L, 7_000L, 4_000L, 1_000L), timeouts);
    }

    @Test
    void reportsAHeartbeatWhileStatusIsUnchanged() {
        DeployMojo mojo = mojo();
        mojo.waitTimeout = 70;

        assertThrows(MojoFailureException.class, mojo::awaitReady);

        assertTrue(logged.stream().anyMatch(l -> l.contains("still waiting")), logged.toString());
    }

    @Test
    void kubectlFailuresDuringWaitAreRetried() throws Exception {
        DeployMojo mojo = mojo();
        responses.add(new DeployMojo.Result(1, "", "Error from server (NotFound)"));
        respond(status(1, 1, "True", "Reconciled", "done"));

        mojo.awaitReady();

        assertEquals(2, calls.size());
    }

    @Test
    void kubeconfigAndContextArePassedToKubectl() throws Exception {
        DeployMojo mojo = mojo();
        mojo.kubeconfig = new File("/tmp/kubeconfig.yaml");
        mojo.kubeContext = "aks";
        respond(status(1, 1, "True", "Reconciled", "done"));

        mojo.awaitReady();

        List<String> args = calls.get(0);
        assertEquals(List.of("--kubeconfig", "/tmp/kubeconfig.yaml", "--context", "aks",
                "--namespace", "apps"), args.subList(0, 6));
        assertFalse(args.contains("--server"));
    }

    @Test
    void reconcileErrorsAreReportedAndWaitingContinues() throws Exception {
        DeployMojo mojo = mojo();
        respond(status(1, 1, "False", "ReconcileError", "no node offers temurin-25"));
        respond(status(1, 1, "True", "Reconciled", "done"));

        mojo.awaitReady();

        assertTrue(logged.stream().anyMatch(l -> l.contains("ReconcileError: no node offers temurin-25")),
                logged.toString());
    }

    @Test
    void rejectsNonpositiveTimeoutBeforePublishingEvenWithoutReadinessWait() {
        for (int value : List.of(0, -1)) {
            DeployMojo mojo = mojo();
            mojo.waitTimeout = value;
            mojo.waitForReady = false;

            MojoExecutionException error = assertThrows(MojoExecutionException.class, mojo::doExecute);

            assertTrue(error.getMessage().contains("brewlet.waitTimeout must be greater than zero"));
            assertTrue(calls.isEmpty());
        }
    }

    @Test
    void doesNotAcceptReadyAfterTheDeadline() {
        DeployMojo mojo = mojo();
        mojo.kubectlRunner = (args, timeout) -> {
            now += timeout;
            return new DeployMojo.Result(0, status(1, 1, "True", "Reconciled", "done"), "");
        };

        assertThrows(MojoFailureException.class, mojo::awaitReady);
        assertFalse(logged.stream().anyMatch(line -> line.contains("is Ready")));
    }

    @Test
    void hungGetReportsTimeoutAndPreservesTheLastStatus() {
        DeployMojo mojo = mojo();
        mojo.kubectlRunner = (args, timeout) -> {
            if (now == 0) {
                now += 2_000;
                return new DeployMojo.Result(0, status(1, 1, "False", "Progressing", "pending"), "");
            }
            assertEquals(25_000, timeout);
            throw new TimeoutException();
        };

        MojoFailureException error = assertThrows(MojoFailureException.class, mojo::awaitReady);

        assertTrue(error.getMessage().contains("Progressing: pending"));
        assertTrue(error.getMessage().contains("kubectl get timed out"));
    }

    @Test
    void applyUsesItsOwnBudgetEvenWithoutReadinessWait() throws Exception {
        DeployMojo mojo = mojo();
        mojo.waitForReady = false;
        mojo.applyManifest(new File("app.yaml"));

        assertEquals(List.of(30_000L), timeouts);
        assertEquals(List.of("--namespace", "apps", "apply", "-f", "app.yaml"), calls.get(0));
    }

    @Test
    void realHungApplyIsTerminatedAndReportsPartialApplication() throws Exception {
        DeployMojo mojo = hungKubectl();
        long start = System.nanoTime();
        try {
            MojoExecutionException error = assertThrows(MojoExecutionException.class,
                    () -> mojo.applyManifest(new File("app.yaml")));

            assertTrue(error.getMessage().contains("kubectl apply timed out after 2s"));
            assertTrue(error.getMessage().contains("partially applied"));
            assertReapedWithinBudget(start);
        } finally {
            killFixture();
        }
    }

    @Test
    void realHungGetCannotBlockReadinessDeadline() throws Exception {
        DeployMojo mojo = hungKubectl();
        long start = System.nanoTime();
        try {
            MojoFailureException error = assertThrows(MojoFailureException.class, mojo::awaitReady);

            assertTrue(error.getMessage().contains("kubectl get timed out"));
            assertReapedWithinBudget(start);
        } finally {
            killFixture();
        }
    }

    @Test
    void interruptionReapsKubectlAndPreservesInterruptFlag() throws Exception {
        DeployMojo mojo = hungKubectl();
        mojo.waitTimeout = 60;
        AtomicReference<Throwable> failure = new AtomicReference<>();
        AtomicBoolean interrupted = new AtomicBoolean();
        Thread worker = new Thread(() -> {
            try {
                mojo.awaitReady();
            } catch (Throwable e) {
                failure.set(e);
            } finally {
                interrupted.set(Thread.currentThread().isInterrupted());
            }
        });
        worker.start();
        try {
            long deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(5);
            while (!Files.exists(root.resolve("kubectl.pid")) && System.nanoTime() < deadline) {
                Thread.sleep(10);
            }
            assertTrue(Files.exists(root.resolve("kubectl.pid")), "fixture did not start");
            worker.interrupt();
            worker.join(10_000);
            assertFalse(worker.isAlive(), "interrupted kubectl wait did not finish");
            assertTrue(failure.get() instanceof MojoExecutionException, String.valueOf(failure.get()));
            assertTrue(interrupted.get());
            assertFalse(fixtureAlive());
        } finally {
            worker.interrupt();
            killFixture();
            worker.join(10_000);
        }
    }

    @Test
    void realCommandPreservesLargeStdoutStderrAndExitCode() throws Exception {
        Path classes = TestApplications.compile(root, "Output", """
                public class Output {
                    public static void main(String[] args) {
                        System.out.print("o".repeat(100000));
                        System.err.print("e".repeat(100000));
                        System.exit(7);
                    }
                }
                """);
        DeployMojo mojo = new DeployMojo();
        mojo.kubectl = javaExecutable();

        DeployMojo.Result result = mojo.runKubectl(List.of("-cp", classes.toString(), "Output"), 5_000);

        assertEquals(7, result.exitCode());
        assertEquals("o".repeat(100000), result.stdout());
        assertEquals("e".repeat(100000), result.stderr());
    }

    @Test
    void timeoutAlsoTerminatesRunningCredentialPlugin() throws Exception {
        TestApplications.compile(root, "TrainingServer", TestApplications.SERVER);
        Path classes = TestApplications.compile(root, "Kubectl", """
                public class Kubectl {
                    public static void main(String[] args) throws Exception {
                        var child = new ProcessBuilder(args[0], "-cp", args[1],
                            "TrainingServer", args[2]).inheritIO().start();
                        java.nio.file.Files.writeString(java.nio.file.Path.of(args[3]),
                            Long.toString(ProcessHandle.current().pid()));
                        child.waitFor();
                    }
                }
                """);
        Path childPid = root.resolve("credential.pid");
        DeployMojo mojo = new DeployMojo();
        mojo.kubectl = javaExecutable();
        long start = System.nanoTime();
        try {
            assertThrows(TimeoutException.class, () -> mojo.runKubectl(
                    List.of("-cp", classes.toString(), "Kubectl", javaExecutable(), classes.toString(),
                            childPid.toString(), root.resolve("kubectl.pid").toString()), 2_000));

            assertReapedWithinBudget(start);
            long pid = Long.parseLong(Files.readString(childPid));
            long deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(2);
            while (ProcessHandle.of(pid).map(ProcessHandle::isAlive).orElse(false)
                    && System.nanoTime() < deadline) {
                Thread.sleep(10);
            }
            assertFalse(ProcessHandle.of(pid).map(ProcessHandle::isAlive).orElse(false),
                    "credential-plugin child survived kubectl timeout");
        } finally {
            killFixture();
            if (Files.exists(childPid)) {
                long pid = Long.parseLong(Files.readString(childPid));
                ProcessHandle.of(pid).filter(ProcessHandle::isAlive).ifPresent(ProcessHandle::destroyForcibly);
            }
        }
    }

    private DeployMojo hungKubectl() throws Exception {
        Path classes = TestApplications.compile(root, "TrainingServer", TestApplications.SERVER);
        DeployMojo mojo = new DeployMojo();
        mojo.namespace = "apps";
        mojo.appName = "orders";
        mojo.waitTimeout = 2;
        mojo.kubectl = javaExecutable();
        mojo.kubectlRunner = (args, timeout) -> mojo.runKubectl(
                List.of("-cp", classes.toString(), "TrainingServer",
                        root.resolve("kubectl.pid").toString()), timeout);
        return mojo;
    }

    private static String javaExecutable() {
        return Path.of(System.getProperty("java.home"), "bin", "java").toString();
    }

    private void assertReapedWithinBudget(long start) throws Exception {
        assertTrue(System.nanoTime() - start < TimeUnit.SECONDS.toNanos(8),
                "command exceeded timeout plus cleanup budget");
        assertFalse(fixtureAlive(), "kubectl process was not reaped");
    }

    private boolean fixtureAlive() throws Exception {
        long pid = Long.parseLong(Files.readString(root.resolve("kubectl.pid")));
        return ProcessHandle.of(pid).map(ProcessHandle::isAlive).orElse(false);
    }

    private void killFixture() throws Exception {
        if (Files.exists(root.resolve("kubectl.pid"))) {
            long pid = Long.parseLong(Files.readString(root.resolve("kubectl.pid")));
            ProcessHandle.of(pid).filter(ProcessHandle::isAlive).ifPresent(ProcessHandle::destroyForcibly);
        }
    }
}
