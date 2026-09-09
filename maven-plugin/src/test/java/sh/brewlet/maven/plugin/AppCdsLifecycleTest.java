// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.plugin.logging.SystemStreamLog;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.ValueSource;

import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.concurrent.atomic.AtomicReference;

import static org.junit.jupiter.api.Assertions.*;

class AppCdsLifecycleTest {
    @TempDir Path root;

    @ParameterizedTest
    @ValueSource(strings = {"log", "http", "exit", "shutdown", "output"})
    void failureReapsRealNeverReadyJvmAndOutputPump(String failure) throws Exception {
        AppCdsMojo mojo = mojo();
        List<String> command = command(failure.equals("shutdown"));
        if (failure.equals("http")) {
            TestApplications.set(mojo, "readyHttp", "http://127.0.0.1:1/never-ready");
        }
        if (failure.equals("shutdown")) TestApplications.set(mojo, "readyLog", "SERVER STARTED");
        if (failure.equals("output")) {
            mojo.setLog(new SystemStreamLog() {
                @Override public void info(CharSequence content) {
                    if (content.toString().contains("[training]")) throw new IllegalStateException("failed log sink");
                }
            });
        }
        assertThrows(MojoExecutionException.class, () -> {
            if (failure.equals("exit")) mojo.runSelfTerminating(command, root.toFile());
            else mojo.runSignalTraining(command, root.toFile());
        });
        assertReaped(readPid());
    }

    @ParameterizedTest
    @ValueSource(strings = {"exit", "readiness", "settle", "shutdown"})
    void interruptionStillReapsJvmAndPreservesInterrupt(String phase) throws Exception {
        AppCdsMojo mojo = mojo();
        TestApplications.set(mojo, "timeoutSeconds", 60);
        if (phase.equals("settle") || phase.equals("shutdown")) {
            TestApplications.set(mojo, "readyLog", "SERVER STARTED");
        }
        if (phase.equals("settle")) TestApplications.set(mojo, "readyDelaySeconds", 30);
        if (phase.equals("shutdown")) TestApplications.set(mojo, "shutdownGraceSeconds", 30);
        List<String> command = command(phase.equals("shutdown"));
        AtomicReference<Throwable> failure = new AtomicReference<>();
        AtomicBoolean interrupted = new AtomicBoolean();
        Thread worker = new Thread(() -> {
            try {
                if (phase.equals("exit")) mojo.runSelfTerminating(command, root.toFile());
                else mojo.runSignalTraining(command, root.toFile());
            } catch (Throwable e) {
                failure.set(e);
            } finally {
                interrupted.set(Thread.currentThread().isInterrupted());
            }
        });
        worker.start();
        long pid = awaitPid();
        try {
            // Allow the readiness latch to hand off to settle/shutdown.
            if (phase.equals("settle") || phase.equals("shutdown")) Thread.sleep(300);
            worker.interrupt();
            worker.join(20000);
            assertFalse(worker.isAlive(), "training wait did not finish after interruption");
            assertInstanceOf(MojoExecutionException.class, failure.get());
            assertTrue(interrupted.get(), "interruption was lost");
            assertReaped(pid);
        } finally {
            worker.interrupt();
            ProcessHandle.of(pid).filter(ProcessHandle::isAlive).ifPresent(ProcessHandle::destroyForcibly);
            worker.join(10000);
        }
    }

    @Test
    void gracefulSignalFlushesAnActualArchive() throws Exception {
        AppCdsMojo mojo = mojo();
        TestApplications.set(mojo, "timeoutSeconds", 10);
        TestApplications.set(mojo, "readyLog", "SERVER STARTED");
        TestApplications.set(mojo, "shutdownGraceSeconds", 10);
        Path archive = root.resolve("signal.jsa");
        List<String> command = command(false);
        command.add("report-shutdown");
        command.add(1, "-XX:ArchiveClassesAtExit=" + archive);
        List<String> output = java.util.Collections.synchronizedList(new ArrayList<>());
        mojo.setLog(new SystemStreamLog() {
            @Override public void info(CharSequence content) { output.add(content.toString()); }
        });
        int code = mojo.runSignalTraining(command, root.toFile());
        assertTrue(code == 0 || code == 143);
        assertTrue(Files.size(archive) > 0);
        assertReaped(readPid());
        for (String message : List.of("SERVER STOPPING", "SERVER STDERR STOPPING",
                "SERVER STOPPED WITHOUT NEWLINE")) {
            assertTrue(output.stream().anyMatch(line -> line.contains(message)),
                    "Training output was closed before shutdown output drained: " + message);
        }
    }

    @Test
    void shutdownOutputFailureIsNotSilentlyIgnored() throws Exception {
        AppCdsMojo mojo = mojo();
        TestApplications.set(mojo, "timeoutSeconds", 10);
        TestApplications.set(mojo, "readyLog", "SERVER STARTED");
        TestApplications.set(mojo, "shutdownGraceSeconds", 10);
        List<String> command = command(false);
        command.add("report-shutdown");
        mojo.setLog(new SystemStreamLog() {
            @Override public void info(CharSequence content) {
                if (content.toString().contains("SERVER STDERR STOPPING")) {
                    throw new IllegalStateException("failed shutdown log sink");
                }
            }
        });
        MojoExecutionException failure = assertThrows(MojoExecutionException.class,
                () -> mojo.runSignalTraining(command, root.toFile()));
        assertInstanceOf(IllegalStateException.class, failure.getCause());
        assertReaped(readPid());
    }

    @ParameterizedTest
    @ValueSource(strings = {"Stream closed", "deliberate read failure"})
    void genuineOutputReadFailuresRemainVisible(String message) throws Exception {
        var failure = new java.io.IOException(message);
        var brokenStream = new java.io.InputStream() {
            @Override public int read() throws java.io.IOException { throw failure; }
        };
        AtomicReference<Throwable> observed = new AtomicReference<>();
        var pump = AppCdsMojo.class.getDeclaredMethod("pumpOutput", java.io.InputStream.class,
                java.util.regex.Pattern.class, CountDownLatch.class, AtomicReference.class);
        pump.setAccessible(true);
        pump.invoke(mojo(), brokenStream, null, null, observed);
        assertSame(failure, observed.get(), "Real I/O errors must not be suppressed based on their message");
    }

    @Test
    void invalidReadinessNeverLaunchesAndRemovesPreviousArchive() throws Exception {
        AppCdsMojo mojo = mojo();
        List<String> command = command(false);
        for (String url : List.of("file:///etc/hosts", "http:/missing-host", "not a URL")) {
            TestApplications.set(mojo, "readyHttp", url);
            assertThrows(MojoExecutionException.class, () -> mojo.runSignalTraining(command, root.toFile()));
            assertFalse(Files.exists(root.resolve("server.pid")));
        }
        Path jar = root.resolve("server.jar");
        TestApplications.configure(mojo, jar, root.resolve("output"), false);
        Path archive = root.resolve("old.jsa");
        Files.writeString(archive, "stale");
        TestApplications.set(mojo, "cdsArchiveOutput", archive.toFile());
        TestApplications.set(mojo, "mode", "signal");
        assertThrows(MojoExecutionException.class, mojo::execute);
        assertFalse(Files.exists(archive), "a failed invocation must not leave old output as new proof");
    }

    @Test
    void bareReadinessDelayIsBoundedByTrainingTimeout() throws Exception {
        AppCdsMojo mojo = mojo();
        TestApplications.set(mojo, "readyLog", null);
        TestApplications.set(mojo, "readyDelaySeconds", 30);
        long start = System.nanoTime();
        assertThrows(MojoExecutionException.class,
                () -> mojo.runSignalTraining(command(false), root.toFile()));
        assertTrue(TimeUnit.NANOSECONDS.toSeconds(System.nanoTime() - start) < 15);
        assertReaped(readPid());
    }

    @Test
    void publicTrainingFailureRemovesOldAndPartiallyFlushedArchive() throws Exception {
        org.junit.jupiter.api.Assumptions.assumeTrue(Runtime.version().feature() >= 21,
                "The public training goal intentionally requires JDK 21+");
        AppCdsMojo mojo = mojo();
        command(false);
        TestApplications.configure(mojo, root.resolve("server.jar"), root.resolve("output"), false);
        Path archive = root.resolve("output.jsa");
        Files.writeString(archive, "old archive");
        TestApplications.set(mojo, "cdsArchiveOutput", archive.toFile());
        TestApplications.set(mojo, "mode", "signal");
        TestApplications.set(mojo, "trainingArgs", List.of(root.resolve("server.pid").toString()));
        assertThrows(MojoExecutionException.class, mojo::execute);
        assertFalse(Files.exists(archive), "timeout may flush a partial archive during cleanup; it must be discarded");
        assertReaped(readPid());
    }

    @Test
    void drippingHttpHeadersCannotExtendTheReadinessDeadlineOrBeAcceptedLate() throws Exception {
        AppCdsMojo mojo = mojo();
        List<String> command = command(false);
        try (DripServer server = new DripServer(false)) {
            server.gateOnStartup(mojo);
            TestApplications.set(mojo, "readyHttp", server.url());
            long start = System.nanoTime();
            MojoExecutionException failure = assertThrows(MojoExecutionException.class,
                    () -> mojo.runSignalTraining(command, root.toFile()));
            assertTrue(failure.getMessage().contains("Timed out"), failure.getMessage());
            assertTrue(TimeUnit.NANOSECONDS.toMillis(System.nanoTime() - start) < 4000,
                    "A one-second readiness budget waited for the five-second response");
            assertFalse(server.headersComplete.get(), "Late HTTP 200 must not be accepted as readiness");
            assertTrue(server.disconnected.await(3, TimeUnit.SECONDS), "Timed-out HTTP exchange was not cancelled");
            assertReaped(readPid());
        }
    }

    @Test
    void interruptionCancelsDrippingHttpProbeAndReapsTrainingJvm() throws Exception {
        AppCdsMojo mojo = mojo();
        TestApplications.set(mojo, "timeoutSeconds", 60);
        List<String> command = command(false);
        try (DripServer server = new DripServer(false)) {
            server.gateOnStartup(mojo);
            TestApplications.set(mojo, "readyHttp", server.url());
            AtomicReference<Throwable> failure = new AtomicReference<>();
            AtomicBoolean interrupted = new AtomicBoolean();
            Thread training = new Thread(() -> {
                try {
                    mojo.runSignalTraining(command, root.toFile());
                } catch (Throwable e) {
                    failure.set(e);
                } finally {
                    interrupted.set(Thread.currentThread().isInterrupted());
                }
            });
            training.start();
            long pid = awaitPid();
            try {
                assertTrue(server.dripping.await(5, TimeUnit.SECONDS), "HTTP probe did not reach the server");
                training.interrupt();
                training.join(3000);
                assertFalse(training.isAlive(), "Interruption did not cancel the in-flight HTTP probe");
                assertInstanceOf(MojoExecutionException.class, failure.get());
                assertTrue(interrupted.get(), "Training lost the interruption flag");
                assertFalse(server.headersComplete.get());
                assertTrue(server.disconnected.await(3, TimeUnit.SECONDS), "Interrupted HTTP exchange remained open");
                assertReaped(pid);
            } finally {
                server.close();
                training.interrupt();
                training.join(10000);
                ProcessHandle.of(pid).filter(ProcessHandle::isAlive).ifPresent(ProcessHandle::destroyForcibly);
            }
        }
    }

    @Test
    void httpReadinessClosesTheBodyWithoutWaitingForItsCompletion() throws Exception {
        AppCdsMojo mojo = mojo();
        List<String> command = command(false);
        try (DripServer server = new DripServer(true)) {
            server.gateOnStartup(mojo);
            TestApplications.set(mojo, "readyHttp", server.url());
            long start = System.nanoTime();
            int exit = mojo.runSignalTraining(command, root.toFile());
            assertTrue(exit == 0 || exit == 143);
            assertTrue(TimeUnit.NANOSECONDS.toMillis(System.nanoTime() - start) < 4000);
            assertTrue(server.disconnected.await(3, TimeUnit.SECONDS), "Unused response body remained open");
            assertReaped(readPid());
        }
    }

    /** Raw HTTP is necessary to drip headers; HttpServer writes headers atomically. */
    private static final class DripServer implements AutoCloseable {
        private final java.net.ServerSocket listener;
        private final CountDownLatch stop = new CountDownLatch(1);
        private final CountDownLatch applicationStarted = new CountDownLatch(1);
        private final CountDownLatch dripping = new CountDownLatch(1);
        private final CountDownLatch disconnected = new CountDownLatch(1);
        private final AtomicBoolean headersComplete = new AtomicBoolean();
        private final Thread worker;
        private volatile java.net.Socket client;

        DripServer(boolean body) throws java.io.IOException {
            listener = new java.net.ServerSocket(0, 1, java.net.InetAddress.getByName("127.0.0.1"));
            listener.setSoTimeout(6000);
            worker = new Thread(() -> serve(body), "brewlet-test-drip-http");
            worker.start();
        }

        String url() { return "http://127.0.0.1:" + listener.getLocalPort() + "/ready"; }

        void gateOnStartup(AppCdsMojo mojo) {
            mojo.setLog(new SystemStreamLog() {
                @Override public void info(CharSequence content) {
                    if (content.toString().contains("[training] SERVER STARTED")) applicationStarted.countDown();
                }
            });
        }

        private void serve(boolean body) {
            try (var socket = listener.accept()) {
                client = socket;
                socket.setSoTimeout(2000);
                var input = new java.io.BufferedReader(new java.io.InputStreamReader(
                        socket.getInputStream(), StandardCharsets.US_ASCII));
                String line;
                while ((line = input.readLine()) != null && !line.isEmpty()) {
                    // Wait for the complete request before beginning the response.
                }
                if (!applicationStarted.await(5, TimeUnit.SECONDS)) return;
                var output = socket.getOutputStream();
                output.write((body
                        ? "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"
                        : "HTTP/1.1 200 OK\r\n").getBytes(StandardCharsets.US_ASCII));
                if (body) headersComplete.set(true);
                long deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(5);
                int sequence = 0;
                while (System.nanoTime() < deadline && stop.getCount() > 0) {
                    output.write((body ? "1\r\nx\r\n" : "X-Drip-" + sequence++ + ": x\r\n")
                            .getBytes(StandardCharsets.US_ASCII));
                    output.flush();
                    dripping.countDown();
                    if (stop.await(50, TimeUnit.MILLISECONDS)) return;
                }
                if (stop.getCount() == 0) return;
                output.write((body ? "0\r\n\r\n" : "Content-Length: 0\r\n\r\n")
                        .getBytes(StandardCharsets.US_ASCII));
                output.flush();
                headersComplete.set(true);
                if (input.read() == -1) disconnected.countDown();
            } catch (java.io.IOException e) {
                if (stop.getCount() > 0) disconnected.countDown();
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
            }
        }

        @Override
        public void close() throws Exception {
            stop.countDown();
            listener.close();
            if (client != null) client.close();
            worker.interrupt();
            worker.join(3000);
            assertFalse(worker.isAlive(), "Drip HTTP server failed to stop");
        }
    }

    private AppCdsMojo mojo() throws Exception {
        AppCdsMojo mojo = new AppCdsMojo();
        TestApplications.set(mojo, "timeoutSeconds", 1);
        TestApplications.set(mojo, "shutdownGraceSeconds", 1);
        TestApplications.set(mojo, "readyLog", "THIS NEVER APPEARS");
        TestApplications.set(mojo, "readyPollMillis", 50L);
        return mojo;
    }

    private List<String> command(boolean slowShutdown) throws Exception {
        Path classes = TestApplications.compile(root.resolve("server"), "TrainingServer", TestApplications.SERVER);
        Path jar = root.resolve("server.jar");
        Files.write(jar, TestApplications.zip(Map.of(
                "META-INF/MANIFEST.MF", "Manifest-Version: 1.0\nMain-Class: TrainingServer\n\n".getBytes(StandardCharsets.UTF_8),
                "TrainingServer.class", Files.readAllBytes(classes.resolve("TrainingServer.class")))));
        List<String> command = new ArrayList<>(List.of(
                AppCdsMojo.javaBinary(new java.io.File(System.getProperty("java.home"))).toString(),
                "-jar", jar.toString(), root.resolve("server.pid").toString()));
        if (slowShutdown) command.add("slow-shutdown");
        return command;
    }

    private long awaitPid() throws Exception {
        long deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(10);
        while ((!Files.exists(root.resolve("server.pid")) || Files.size(root.resolve("server.pid")) == 0)
                && System.nanoTime() < deadline) Thread.sleep(20);
        return readPid();
    }

    private long readPid() throws Exception {
        return Long.parseLong(Files.readString(root.resolve("server.pid")));
    }

    private static void assertReaped(long pid) {
        assertFalse(ProcessHandle.of(pid).map(ProcessHandle::isAlive).orElse(false), "leaked training JVM " + pid);
        assertFalse(Thread.getAllStackTraces().keySet().stream()
                .anyMatch(t -> t.isAlive() && t.getName().equals("brewlet-appcds-training-io-" + pid)), "leaked output pump");
    }
}
