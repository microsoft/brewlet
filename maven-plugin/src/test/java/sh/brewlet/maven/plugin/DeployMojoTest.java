// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import org.apache.maven.plugin.MojoFailureException;
import org.apache.maven.plugin.logging.SystemStreamLog;
import org.junit.jupiter.api.Test;

import java.io.File;
import java.util.ArrayList;
import java.util.Deque;
import java.util.List;
import java.util.ArrayDeque;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

class DeployMojoTest {

    private final List<List<String>> calls = new ArrayList<>();
    private final Deque<DeployMojo.Result> responses = new ArrayDeque<>();
    private final List<String> logged = new ArrayList<>();
    private long now;

    private DeployMojo mojo() {
        DeployMojo mojo = new DeployMojo();
        mojo.namespace = "apps";
        mojo.appName = "orders";
        mojo.waitTimeout = 30;
        mojo.kubectlRunner = args -> {
            calls.add(args);
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
}
