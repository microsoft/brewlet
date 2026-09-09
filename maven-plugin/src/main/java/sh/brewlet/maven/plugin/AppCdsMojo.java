// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.plugin.MojoFailureException;
import org.apache.maven.plugins.annotations.Mojo;
import org.apache.maven.plugins.annotations.Parameter;
import org.apache.maven.plugins.annotations.ResolutionScope;
import sh.brewlet.maven.plugin.model.Entry;
import sh.brewlet.maven.plugin.model.JvmConfig;
import sh.brewlet.maven.plugin.util.LayerBuilder;
import sh.brewlet.maven.plugin.util.PreparedApplication;

import java.io.BufferedReader;
import java.io.File;
import java.io.IOException;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.io.UncheckedIOException;
import java.net.URI;
import java.net.URISyntaxException;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.StandardCopyOption;
import java.nio.file.attribute.FileTime;
import java.time.Duration;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.List;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutionException;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.TimeoutException;
import java.util.concurrent.atomic.AtomicReference;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * <strong>brewlet:appcds</strong> — Generate a dynamic Application Class-Data
 * Sharing archive with a self-terminating training run.
 *
 * <p>Usage:
 * <pre>
 * mvn package brewlet:appcds \
 *   -Dbrewlet.appcds.trainingArgs=--warmup \
 *   -Dbrewlet.appcds.timeoutSeconds=180
 * mvn brewlet:push -Dbrewlet.image=registry.example.com/team/app:1.0 \
 *   -Dbrewlet.cdsArchive=target/brewlet/app.jsa
 * </pre>
 *
 * <p>Two termination strategies (see {@code brewlet.appcds.mode}):
 * <ul>
 *   <li><strong>{@code exit}</strong> (default) — the app is expected to boot,
 *       exercise startup paths, and exit on its own within the timeout.</li>
 *   <li><strong>{@code signal}</strong> — for long-running servers: the goal
 *       starts the app, waits for a readiness signal
 *       ({@code readyLog} / {@code readyHttp} / {@code readyDelaySeconds}), then
 *       sends {@code SIGTERM} so the JVM shutdown hook runs and dynamic CDS writes
 *       the archive at exit. Unix-oriented (a graceful {@code SIGTERM} is required
 *       for the archive to flush); Brewlet nodes are Linux.</li>
 * </ul>
 *
 * <p>Bind it to {@code pre-integration-test} if your build already has a short
 * startup/warmup path. Training matches how the artifact is pushed: a fat JAR
 * ({@code -jar}), a layered class-path app ({@code -cp <mainJar>:lib/*}, with the
 * resolved runtime dependencies staged into {@code lib/}), or a JPMS module
 * ({@code -p <mainJar>:mods -m …}, dependencies staged into {@code mods/}) — run
 * the goal with the same {@code -Dbrewlet.layered=true} you push with. All staged
 * files get a canonical mtime so the archive maps against the shim's layout.
 * See https://github.com/microsoft/brewlet/blob/main/docs/appcds.md#42-turnkey-generation-in-the-maven-plugin--cli.
 */
@Mojo(name = "appcds",
      requiresProject = true,
      requiresDependencyResolution = ResolutionScope.RUNTIME,
      threadSafe = true)
public class AppCdsMojo extends AbstractBrewletMojo {

    static final String MODE_EXIT = "exit";
    static final String MODE_SIGNAL = "signal";

    private static final HttpClient READINESS_HTTP_CLIENT = HttpClient.newBuilder()
            .connectTimeout(Duration.ofSeconds(2))
            .followRedirects(HttpClient.Redirect.NEVER)
            .version(HttpClient.Version.HTTP_1_1)
            .build();

    static final FileTime CANONICAL_APP_MTIME = PreparedApplication.CANONICAL_MTIME;

    /**
     * Output path for the generated dynamic-CDS archive.
     */
    @Parameter(defaultValue = "${project.build.directory}/brewlet/app.jsa")
    private File cdsArchiveOutput;

    /**
     * Extra program arguments passed after {@code -jar <mainJar>} for the
     * self-terminating training run.
     */
    @Parameter(property = "brewlet.appcds.trainingArgs")
    private List<String> trainingArgs;

    /**
     * Maximum time to wait for the app to boot, exercise startup paths, and exit
     * ({@code exit} mode) or become ready and complete the settle delay
     * ({@code signal} mode). Shutdown grace is a separate bound.
     */
    @Parameter(property = "brewlet.appcds.timeoutSeconds", defaultValue = "120")
    private int timeoutSeconds;

    /**
     * JDK used for training. Defaults to the JDK running Maven
     * ({@code java.home}); turnkey AppCDS requires JDK 21+ per
     * https://github.com/microsoft/brewlet/blob/main/docs/appcds.md#22-minimum-jdk-version.
     */
    @Parameter(property = "brewlet.appcds.javaHome")
    private File trainingJavaHome;

    /**
     * Training termination strategy: {@code exit} (self-terminating, default) or
     * {@code signal} (start, wait for readiness, then {@code SIGTERM}).
     */
    @Parameter(property = "brewlet.appcds.mode", defaultValue = MODE_EXIT)
    private String mode;

    /**
     * {@code signal} mode readiness: a regular expression matched line-by-line
     * against the training JVM's combined stdout/stderr. The app is considered
     * warmed up as soon as a line matches (e.g. {@code "Started .* in .* seconds"}
     * for Spring Boot).
     */
    @Parameter(property = "brewlet.appcds.readyLog")
    private String readyLog;

    /**
     * {@code signal} mode readiness: an HTTP(S) URL polled until it returns a
     * 2xx/3xx status (e.g. {@code http://localhost:8080/actuator/health}).
     */
    @Parameter(property = "brewlet.appcds.readyHttp")
    private String readyHttp;

    /**
     * {@code signal} mode readiness: a fixed warmup delay (seconds) before
     * {@code SIGTERM}. Used alone as a simple fallback, or as a settle time after
     * {@code readyLog}/{@code readyHttp} fires.
     */
    @Parameter(property = "brewlet.appcds.readyDelaySeconds", defaultValue = "0")
    private int readyDelaySeconds;

    /**
     * {@code signal} mode: how long to wait after {@code SIGTERM} for the app to
     * shut down and flush the archive before force-killing.
     */
    @Parameter(property = "brewlet.appcds.shutdownGraceSeconds", defaultValue = "30")
    private int shutdownGraceSeconds;

    /**
     * {@code signal} mode: poll interval (milliseconds) for {@code readyHttp}.
     */
    @Parameter(property = "brewlet.appcds.readyPollMillis", defaultValue = "500")
    private long readyPollMillis;

    @Override
    protected void doExecute() throws MojoExecutionException, MojoFailureException {
        Path output = cdsArchiveOutput.toPath().toAbsolutePath().normalize();
        if (!dryRun) {
            try {
                Path source = resolveJarFile().toPath().toAbsolutePath().normalize();
                if (output.equals(source) || (Files.exists(output) && Files.isSameFile(output, source))) {
                    throw new MojoExecutionException("AppCDS archive output must not overwrite the input JAR.");
                }
                Files.deleteIfExists(output);
            } catch (IOException e) {
                throw new MojoExecutionException("Failed to remove previous AppCDS archive output", e);
            }
        }
        try {
            generateArchive();
        } catch (MojoExecutionException | RuntimeException e) {
            if (!dryRun) {
                try {
                    Files.deleteIfExists(output);
                } catch (IOException cleanup) {
                    e.addSuppressed(cleanup);
                }
            }
            throw e;
        }
    }

    private void generateArchive() throws MojoExecutionException {
        if (timeoutSeconds <= 0) {
            throw new MojoExecutionException("brewlet.appcds.timeoutSeconds must be greater than zero.");
        }
        String trainingMode = normalizeMode(mode);
        if (MODE_SIGNAL.equals(trainingMode)) {
            validateReadiness(readyLog, readyHttp, readyDelaySeconds);
            if (shutdownGraceSeconds <= 0) {
                throw new MojoExecutionException(
                        "brewlet.appcds.shutdownGraceSeconds must be greater than zero.");
            }
        }

        JvmConfig cfg = buildConfig();
        PreparedApplication payload = prepareApplication();
        Entry entry = cfg.getEntry();
        String entryMode = entry == null || entry.getMode() == null || entry.getMode().isBlank()
                ? "jar"
                : entry.getMode();

        File javaHome = trainingJavaHome != null
                ? trainingJavaHome
                : new File(System.getProperty("java.home"));
        File java = javaBinary(javaHome);
        Path trainingDir = outputDirectory.toPath().resolve("appcds-training");
        File output = cdsArchiveOutput.getAbsoluteFile();
        if (output.toPath().normalize().startsWith(trainingDir.toAbsolutePath().normalize())) {
            throw new MojoExecutionException("AppCDS archive output must be outside appcds-training, "
                    + "which is cleaned before each run.");
        }
        List<String> command = buildTrainingCommand(java, cfg, output, cfg.getMainJar(), trainingArgs);

        if (dryRun) {
            getLog().info("Brewlet: dry-run mode — would generate AppCDS archive:");
            getLog().info("  mode: " + trainingMode
                    + (MODE_SIGNAL.equals(trainingMode) ? " (start → wait for readiness → SIGTERM)" : " (self-terminating)"));
            getLog().info("  entry: " + entryMode);
            getLog().info("  working directory: " + trainingDir);
            getLog().info("  command: " + String.join(" ", command));
            getLog().info("  training JAR copy mtime: 2000-01-01T00:00:00Z");
            return;
        }

        if (!java.isFile()) {
            throw new MojoExecutionException("Training java binary not found: " + java.getAbsolutePath());
        }

        int feature = detectTrainingJdkFeature(java);
        if (feature < 21) {
            throw new MojoExecutionException("brewlet:appcds requires a JDK 21+ training runtime "
                    + "(https://github.com/microsoft/brewlet/blob/main/docs/appcds.md#22-minimum-jdk-version); "
                    + java.getAbsolutePath()
                    + " reports feature " + feature + ".");
        }

        try {
            Files.createDirectories(output.toPath().getParent());
            Files.deleteIfExists(output.toPath());
        } catch (IOException e) {
            throw new MojoExecutionException("Failed to prepare AppCDS archive output", e);
        }
        Path trainingJar = stageTrainingInputs(cfg, entryMode, payload, trainingDir);

        getLog().info("Brewlet: generating AppCDS archive ("
                + (MODE_SIGNAL.equals(trainingMode) ? "signal mode: readiness → SIGTERM" : "self-terminating training run") + ")");
        getLog().info("  entry mode: " + entryMode);
        getLog().info("  java: " + java.getAbsolutePath() + " (feature " + feature + ")");
        getLog().info("  training jar: " + trainingJar + " (mtime pinned to 2000-01-01T00:00:00Z)");
        getLog().info("  archive: " + output.getAbsolutePath());
        getLog().info("  command: " + String.join(" ", command));

        int exitCode = MODE_SIGNAL.equals(trainingMode)
                ? runSignalTraining(command, trainingDir.toFile())
                : runSelfTerminating(command, trainingDir.toFile());

        if (!output.isFile() || output.length() == 0) {
            throw new MojoExecutionException("AppCDS training exited with code " + exitCode
                    + " but did not write a non-empty archive at " + output.getAbsolutePath()
                    + (MODE_SIGNAL.equals(trainingMode)
                        ? ". In signal mode the app's shutdown hook must let the JVM exit cleanly so "
                          + "dynamic CDS can flush the archive; check that SIGTERM triggers a graceful shutdown."
                        : ""));
        }
        if (exitCode != 0 && !(MODE_SIGNAL.equals(trainingMode) && exitCode == 143)) {
            throw new MojoExecutionException("AppCDS training failed with exit code " + exitCode
                    + "; discarding its archive.");
        }

        getLog().info("Brewlet: wrote AppCDS archive → " + output.getAbsolutePath());
        getLog().info("Attach it with brewlet:build/push using -Dbrewlet.cdsArchive="
                + output.getAbsolutePath());
    }

    /** {@code exit} mode: run the self-terminating training JVM to completion. */
    int runSelfTerminating(List<String> command, File workingDir)
            throws MojoExecutionException {
        try (TrainingProcess training = startTraining(command, workingDir, null, null)) {
            Process process = training.process;
            if (!process.waitFor(timeoutSeconds, TimeUnit.SECONDS)) {
                throw new MojoExecutionException("AppCDS training timed out after " + timeoutSeconds
                        + "s. This mode expects a self-terminating app: it must boot, exercise startup "
                        + "paths, and exit. Raise -Dbrewlet.appcds.timeoutSeconds=..., use "
                        + "-Dbrewlet.appcds.mode=signal for long-running servers, or supply a prebuilt "
                        + "archive with -Dbrewlet.cdsArchive=...");
            }
            training.checkOutput();
            return process.exitValue();
        } catch (IOException e) {
            throw new MojoExecutionException("Failed to launch AppCDS training JVM", e);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            throw new MojoExecutionException("Interrupted while waiting for AppCDS training JVM", e);
        }
    }

    /**
     * {@code signal} mode: start the app, wait for readiness, then {@code SIGTERM}
     * so the JVM shutdown hook runs and dynamic CDS flushes the archive at exit.
     */
    int runSignalTraining(List<String> command, File workingDir)
            throws MojoExecutionException {
        validateReadiness(readyLog, readyHttp, readyDelaySeconds);
        if (timeoutSeconds <= 0 || shutdownGraceSeconds <= 0 || readyPollMillis <= 0) {
            throw new MojoExecutionException("AppCDS timeout, shutdown grace, and poll interval must be positive.");
        }
        CountDownLatch logReady = new CountDownLatch(1);
        Pattern readyPattern = readyLog == null || readyLog.isBlank() ? null : Pattern.compile(readyLog);

        try (TrainingProcess training = startTraining(command, workingDir, readyPattern, logReady)) {
            Process process = training.process;
            long deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(timeoutSeconds);
            awaitReadiness(training, readyPattern, logReady, deadline);
            if (readyDelaySeconds > 0) {
                getLog().info("  readiness reached; settling for " + readyDelaySeconds + "s before SIGTERM");
                long remaining = Math.max(0, deadline - System.nanoTime());
                long settle = TimeUnit.SECONDS.toNanos(readyDelaySeconds);
                if (process.waitFor(Math.min(remaining, settle), TimeUnit.NANOSECONDS)) {
                    throw new MojoExecutionException("Training JVM exited during the readiness/settle delay.");
                }
                if (remaining < settle) {
                    throw new MojoExecutionException("AppCDS readiness/settle delay exceeded timeoutSeconds="
                            + timeoutSeconds + ". Raise the timeout or reduce readyDelaySeconds.");
                }
            }

            training.checkOutput();
            getLog().info("  sending SIGTERM to let the app shut down and flush the archive");
            process.destroy();
            if (!process.waitFor(shutdownGraceSeconds, TimeUnit.SECONDS)) {
                process.destroyForcibly();
                throw new MojoExecutionException("Training JVM did not exit within "
                        + shutdownGraceSeconds + "s of SIGTERM; the archive was likely not flushed. "
                        + "Ensure the app shuts down gracefully on SIGTERM, or raise "
                        + "-Dbrewlet.appcds.shutdownGraceSeconds=...");
            }
            training.checkOutput();
            return process.exitValue();
        } catch (IOException e) {
            throw new MojoExecutionException("Failed to launch AppCDS training JVM", e);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            throw new MojoExecutionException("Interrupted while training AppCDS archive", e);
        }
    }

    /**
     * Blocks until the training app signals readiness or the timeout elapses.
     * Honors {@code readyLog} (latch), {@code readyHttp} (poll), and a bare
     * {@code readyDelaySeconds} (handled by the caller). Fails fast if the process
     * dies before becoming ready.
     */
    private void awaitReadiness(TrainingProcess training, Pattern readyPattern, CountDownLatch logReady,
                                long deadline)
            throws MojoExecutionException, InterruptedException {
        Process process = training.process;
        if (readyHttp != null && !readyHttp.isBlank()) {
            getLog().info("  waiting for readiness: HTTP 2xx/3xx from " + readyHttp);
            while (System.nanoTime() < deadline) {
                training.checkOutput();
                if (!process.isAlive()) {
                    throw new MojoExecutionException("Training JVM exited (code " + process.exitValue()
                            + ") before " + readyHttp + " became ready.");
                }
                if (httpReady(readyHttp, deadline) && System.nanoTime() < deadline) {
                    training.checkOutput();
                    if (!process.isAlive()) {
                        throw new MojoExecutionException("Training JVM exited before HTTP readiness completed.");
                    }
                    getLog().info("  readiness reached via HTTP probe");
                    return;
                }
                TimeUnit.NANOSECONDS.sleep(Math.max(0, Math.min(
                        TimeUnit.MILLISECONDS.toNanos(readyPollMillis), deadline - System.nanoTime())));
            }
            throw new MojoExecutionException("Timed out after " + timeoutSeconds
                    + "s waiting for " + readyHttp + " to become ready.");
        }

        if (readyPattern != null) {
            getLog().info("  waiting for readiness: log match /" + readyPattern.pattern() + "/");
            while (System.nanoTime() < deadline) {
                training.checkOutput();
                if (!process.isAlive()) {
                    throw new MojoExecutionException("Training JVM exited (code " + process.exitValue()
                            + ") before the readyLog pattern matched.");
                }
                if (logReady.await(Math.max(0, Math.min(TimeUnit.MILLISECONDS.toNanos(100),
                        deadline - System.nanoTime())), TimeUnit.NANOSECONDS)) {
                    getLog().info("  readiness reached via log match");
                    return;
                }
            }
            throw new MojoExecutionException("Timed out after " + timeoutSeconds
                    + "s waiting for a log line matching /" + readyPattern.pattern() + "/.");
        }

        // readyDelaySeconds-only: readiness is simply "still alive after the delay",
        // which the caller applies. Guard against an immediate crash here.
        if (!process.isAlive()) {
            throw new MojoExecutionException("Training JVM exited (code " + process.exitValue()
                    + ") immediately; nothing to train.");
        }
    }

    /** Reads the process output, echoes each line, and trips {@code ready} on match. */
    private void pumpOutput(InputStream in, Pattern readyPattern, CountDownLatch ready,
                            AtomicReference<Throwable> failure) {
        try (BufferedReader reader = new BufferedReader(new InputStreamReader(in, StandardCharsets.UTF_8))) {
            String line;
            while ((line = reader.readLine()) != null) {
                getLog().info("  [training] " + line);
                if (readyPattern != null && ready.getCount() > 0 && readyPattern.matcher(line).find()) {
                    ready.countDown();
                }
            }
        } catch (IOException | RuntimeException e) {
            failure.compareAndSet(null, e);
        }
    }

    private TrainingProcess startTraining(List<String> command, File workingDir, Pattern pattern,
                                          CountDownLatch ready) throws IOException {
        Process process = new ProcessBuilder(command).directory(workingDir).redirectErrorStream(true).start();
        return new TrainingProcess(process, pattern, ready);
    }

    private final class TrainingProcess implements AutoCloseable {
        private final Process process;
        private final Thread pump;
        private final AtomicReference<Throwable> outputFailure = new AtomicReference<>();

        TrainingProcess(Process process, Pattern pattern, CountDownLatch ready) {
            this.process = process;
            pump = new Thread(() -> pumpOutput(process.getInputStream(), pattern, ready, outputFailure),
                    "brewlet-appcds-training-io-" + process.pid());
            pump.setDaemon(true);
            pump.start();
        }

        void checkOutput() throws MojoExecutionException {
            Throwable failure = outputFailure.get();
            if (failure != null) throw new MojoExecutionException("Failed to read AppCDS training output", failure);
        }

        @Override
        public void close() throws MojoExecutionException {
            boolean interrupted = Thread.interrupted();
            MojoExecutionException failure = null;
            try {
                if (process.isAlive()) {
                    process.destroy();
                    interrupted |= waitForCleanup(process, 5);
                    if (process.isAlive()) {
                        process.destroyForcibly();
                        interrupted |= waitForCleanup(process, 5);
                    }
                }
                if (process.isAlive()) {
                    failure = new MojoExecutionException("Could not reap AppCDS training JVM PID " + process.pid());
                }
                // Let a normally exiting process drain before closing the pipe.
                long deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(5);
                while (pump.isAlive() && System.nanoTime() < deadline) {
                    try {
                        pump.join(Math.max(1, TimeUnit.NANOSECONDS.toMillis(deadline - System.nanoTime())));
                    } catch (InterruptedException e) {
                        interrupted = true;
                    }
                }
                if (outputFailure.get() != null && failure == null) {
                    failure = new MojoExecutionException("Failed to read AppCDS training output", outputFailure.get());
                }
                for (var stream : List.of(process.getInputStream(), process.getErrorStream(), process.getOutputStream())) {
                    try {
                        stream.close();
                    } catch (IOException e) {
                        if (failure == null) failure = new MojoExecutionException("Failed to close training JVM streams", e);
                        else failure.addSuppressed(e);
                    }
                }
                pump.interrupt();
                deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(1);
                while (pump.isAlive() && System.nanoTime() < deadline) {
                    try {
                        pump.join(Math.max(1, TimeUnit.NANOSECONDS.toMillis(deadline - System.nanoTime())));
                    } catch (InterruptedException e) {
                        interrupted = true;
                    }
                }
                if (pump.isAlive() && failure == null) {
                    failure = new MojoExecutionException("AppCDS output pump did not stop for PID " + process.pid());
                }
                if (interrupted && failure == null) {
                    failure = new MojoExecutionException("Interrupted while reaping AppCDS training JVM");
                }
                if (failure != null) throw failure;
            } finally {
                if (interrupted) Thread.currentThread().interrupt();
            }
        }
    }

    /** Cleanup cannot abandon the child when the caller is already interrupted. */
    private static boolean waitForCleanup(Process process, int seconds) {
        boolean interrupted = false;
        long deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(seconds);
        while (process.isAlive() && System.nanoTime() < deadline) {
            try {
                process.waitFor(Math.max(0, deadline - System.nanoTime()), TimeUnit.NANOSECONDS);
            } catch (InterruptedException e) {
                interrupted = true;
            }
        }
        return interrupted;
    }

    /** Returns true when an HTTP(S) GET to {@code url} answers with a 2xx/3xx status. */
    static boolean httpReady(String url) throws MojoExecutionException, InterruptedException {
        return httpReady(url, System.nanoTime() + TimeUnit.SECONDS.toNanos(2));
    }

    private static boolean httpReady(String url, long deadline)
            throws MojoExecutionException, InterruptedException {
        long now = System.nanoTime();
        long remaining = Math.min(TimeUnit.SECONDS.toNanos(2), deadline - now);
        if (remaining <= 0) return false;
        long probeDeadline = now + remaining;
        HttpRequest request;
        try {
            request = HttpRequest.newBuilder(new URI(url))
                    .timeout(Duration.ofNanos(remaining)).GET().build();
        } catch (URISyntaxException | IllegalArgumentException e) {
            throw new MojoExecutionException("Invalid HTTP readiness request: " + url, e);
        }
        var response = READINESS_HTTP_CLIENT.sendAsync(request, HttpResponse.BodyHandlers.ofInputStream());
        // Complete on headers, not on body EOF. Closing here also owns a response
        // that races with cancellation of the caller's deadline-bounded wait.
        var status = response.thenApply(value -> {
            try (InputStream body = value.body()) {
                return value.statusCode();
            } catch (IOException e) {
                throw new UncheckedIOException(e);
            }
        });
        try {
            long wait = probeDeadline - System.nanoTime();
            if (wait <= 0) return false;
            int code = status.get(wait, TimeUnit.NANOSECONDS);
            return System.nanoTime() < deadline && System.nanoTime() < probeDeadline
                    && code >= 200 && code < 400;
        } catch (TimeoutException e) {
            return false;
        } catch (ExecutionException e) {
            if (e.getCause() instanceof IOException || e.getCause() instanceof UncheckedIOException) {
                return false;
            }
            throw new MojoExecutionException("HTTP readiness probe failed", e.getCause());
        } finally {
            // Cancel the exchange itself, not just its derived status future.
            response.cancel(true);
        }
    }

    /** Normalizes and validates the training termination mode. */
    static String normalizeMode(String raw) throws MojoExecutionException {
        String m = raw == null ? MODE_EXIT : raw.trim().toLowerCase(java.util.Locale.ROOT);
        if (m.isEmpty()) {
            return MODE_EXIT;
        }
        if (!MODE_EXIT.equals(m) && !MODE_SIGNAL.equals(m)) {
            throw new MojoExecutionException("brewlet.appcds.mode \"" + raw
                    + "\" is not recognized (expected \"exit\" or \"signal\").");
        }
        return m;
    }

    /** In signal mode at least one readiness signal must be configured. */
    static void validateReadiness(String readyLog, String readyHttp, int readyDelaySeconds)
            throws MojoExecutionException {
        boolean hasLog = readyLog != null && !readyLog.isBlank();
        boolean hasHttp = readyHttp != null && !readyHttp.isBlank();
        if (readyDelaySeconds < 0) {
            throw new MojoExecutionException("brewlet.appcds.readyDelaySeconds must not be negative.");
        }
        if (hasHttp) {
            try {
                URI uri = new URI(readyHttp);
                if ((!"http".equalsIgnoreCase(uri.getScheme()) && !"https".equalsIgnoreCase(uri.getScheme()))
                        || uri.getHost() == null) {
                    throw new URISyntaxException(readyHttp, "expected an HTTP(S) URL with a host");
                }
            } catch (URISyntaxException e) {
                throw new MojoExecutionException("Invalid brewlet.appcds.readyHttp: " + e.getMessage(), e);
            }
        }
        if (hasLog) {
            try {
                Pattern.compile(readyLog);
            } catch (java.util.regex.PatternSyntaxException e) {
                throw new MojoExecutionException("brewlet.appcds.readyLog is not a valid regex: "
                        + e.getMessage(), e);
            }
        }
        if (!hasLog && !hasHttp && readyDelaySeconds <= 0) {
            throw new MojoExecutionException("brewlet.appcds.mode=signal requires a readiness signal: "
                    + "set one of -Dbrewlet.appcds.readyLog=<regex>, "
                    + "-Dbrewlet.appcds.readyHttp=<url>, or -Dbrewlet.appcds.readyDelaySeconds=<n>.");
        }
    }

    private int detectTrainingJdkFeature(File java) throws MojoExecutionException {
        if (trainingJavaHome == null) {
            return parseJavaFeatureVersion(System.getProperty("java.version"));
        }
        Path versionOutput = outputDirectory.toPath().resolve("appcds-java-version.log");
        try {
            Files.createDirectories(versionOutput.toAbsolutePath().getParent());
            Process process = new ProcessBuilder(java.getAbsolutePath(), "-version")
                    .redirectErrorStream(true)
                    .redirectOutput(versionOutput.toFile())
                    .start();
            try (TrainingProcess training = new TrainingProcess(process, null, null)) {
                if (!process.waitFor(10, TimeUnit.SECONDS)) {
                    throw new MojoExecutionException("Timed out running " + java.getAbsolutePath()
                            + " -version to verify the JDK 21+ AppCDS floor.");
                }
                if (process.exitValue() != 0) {
                    throw new MojoExecutionException("Training java -version failed with code " + process.exitValue());
                }
                return parseJavaFeatureVersion(Files.readString(versionOutput));
            }
        } catch (IOException e) {
            throw new MojoExecutionException("Failed to run " + java.getAbsolutePath()
                    + " -version to verify the JDK 21+ AppCDS floor", e);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            throw new MojoExecutionException("Interrupted while checking training JDK version", e);
        } catch (IllegalArgumentException e) {
            throw new MojoExecutionException("Could not parse training JDK version from "
                    + java.getAbsolutePath() + " -version", e);
        }
    }

    static File javaBinary(File javaHome) {
        String exe = System.getProperty("os.name", "").toLowerCase().contains("win")
                ? "java.exe"
                : "java";
        return javaHome.toPath().resolve("bin").resolve(exe).toFile();
    }

    static List<String> buildTrainingCommand(File javaBinary, JvmConfig cfg, File archiveOutput,
                                             String mainJar, List<String> trainingArgs) {
        List<String> command = new ArrayList<>();
        command.add(javaBinary.getAbsolutePath());
        command.addAll(appIntrinsicJvmArgs(cfg));
        command.add("-XX:ArchiveClassesAtExit=" + archiveOutput.getAbsolutePath());
        command.addAll(launchSelector(cfg, mainJar));
        if (trainingArgs != null) {
            command.addAll(trainingArgs);
        }
        return command;
    }

    /**
     * Builds the launch selector (everything after the JVM options) that mirrors
     * how the shim launches the artifact, so the training classpath/module path —
     * and therefore the recorded archive — matches production:
     * <ul>
     *   <li>{@code jar} → {@code -jar <mainJar>}</li>
     *   <li>{@code classpath} → {@code -cp <classPath> <mainClass>} (relative,
     *       {@code lib/*} left literal for the JVM to expand)</li>
     *   <li>{@code module} → {@code [-cp <classPath>] -p <modulePath>
     *       -m <module>[/<mainClass>]} — the supplementary {@code -cp} is the
     *       JPMS <em>mixed form</em> and precedes the module path so
     *       {@code -m} stays terminal, exactly as the launch core emits it</li>
     * </ul>
     * Path lists are joined with the platform separator so the training JVM starts
     * locally; HotSpot validates each classpath entry by basename+size+mtime (not
     * absolute path), so the archive still maps under the shim's own layout.
     */
    static List<String> launchSelector(JvmConfig cfg, String mainJar) {
        Entry entry = cfg.getEntry();
        String mode = entry == null || entry.getMode() == null || entry.getMode().isBlank()
                ? "jar"
                : entry.getMode();
        List<String> out = new ArrayList<>();
        switch (mode) {
            case "classpath": {
                List<String> cp = entry.getClassPath() != null && !entry.getClassPath().isEmpty()
                        ? entry.getClassPath()
                        : List.of(mainJar);
                out.add("-cp");
                out.add(String.join(File.pathSeparator, cp));
                if (entry.getMainClass() != null && !entry.getMainClass().isBlank()) {
                    out.add(entry.getMainClass());
                }
                break;
            }
            case "module": {
                // Mixed form: module mode additionally permits a supplementary
                // class path. Omitting it here would train the archive against a
                // classpath that does not match production, so classes loaded
                // from those entries would be absent from the archive (or fail
                // its validation) and silently fall back to base CDS.
                if (entry.getClassPath() != null && !entry.getClassPath().isEmpty()) {
                    out.add("-cp");
                    out.add(String.join(File.pathSeparator, entry.getClassPath()));
                }
                List<String> mp = entry.getModulePath() != null && !entry.getModulePath().isEmpty()
                        ? entry.getModulePath()
                        : List.of(mainJar);
                out.add("-p");
                out.add(String.join(File.pathSeparator, mp));
                out.add("-m");
                out.add(entry.getMainClass() != null && !entry.getMainClass().isBlank()
                        ? entry.getModule() + "/" + entry.getMainClass()
                        : entry.getModule());
                break;
            }
            default:
                out.add("-jar");
                out.add(mainJar);
        }
        return out;
    }

    /**
     * Stages the training inputs into {@code trainingDir} so the runtime layout is
     * reproduced: the app JAR at {@code <mainJar>}, and — for layered classpath /
     * module apps — the resolved runtime dependencies under {@code lib/} or
     * {@code mods/} (matching the shim's {@code /app/lib} and {@code /app/mods}).
     * Every staged file's mtime is pinned to the canonical value so it matches the
     * node-side pinning and the archive maps. Returns the staged app JAR path.
     */
    Path stageTrainingInputs(JvmConfig cfg, String entryMode, PreparedApplication payload, Path trainingDir)
            throws MojoExecutionException {
        try {
            Path root = trainingDir.toAbsolutePath().normalize();
            Path realRoot = Files.exists(root) ? root.toRealPath() : root;
            List<Path> inputs = new ArrayList<>(List.of(resolveJarFile().toPath(), payload.jar().toPath()));
            for (var dep : payload.dependencies()) inputs.add(dep.path());
            for (Path input : inputs) {
                if (input.toAbsolutePath().normalize().startsWith(root) || input.toRealPath().startsWith(realRoot)) {
                    throw new MojoExecutionException("AppCDS training directory must not contain its input JARs.");
                }
            }
            // A previous wildcard classpath must not inject stale libraries into this training run.
            if (Files.exists(trainingDir)) {
                try (var paths = Files.walk(trainingDir)) {
                    for (Path path : paths.sorted(Comparator.reverseOrder()).toList()) Files.delete(path);
                }
            }
            Files.createDirectories(trainingDir);
            Path trainingJar = trainingDir.resolve(cfg.getMainJar());
            Files.copy(payload.jar().toPath(), trainingJar, StandardCopyOption.REPLACE_EXISTING);
            Files.setLastModifiedTime(trainingJar, CANONICAL_APP_MTIME);

            Entry entry = cfg.getEntry();
            if ("classpath".equals(entryMode) && referencesDir(entry.getClassPath(), "lib")) {
                stageDeps(trainingDir.resolve("lib"), payload.dependencies());
            } else if ("module".equals(entryMode)) {
                if (referencesDir(entry.getModulePath(), "mods")) {
                    stageDeps(trainingDir.resolve("mods"), payload.dependencies());
                }
                // The mixed form also carries a class path, whose lib/ entries
                // must be staged too or the training launch cannot resolve them.
                if (referencesDir(entry.getClassPath(), "lib")) {
                    stageDeps(trainingDir.resolve("lib"), payload.dependencies());
                }
            }
            return trainingJar;
        } catch (IOException e) {
            throw new MojoExecutionException("Failed to stage AppCDS training inputs", e);
        }
    }

    /** Copies the resolved runtime dependency JARs into {@code dir}, pinning mtimes. */
    private void stageDeps(Path dir, List<LayerBuilder.Dep> deps) throws IOException {
        LayerBuilder.validateDependencies(deps);
        Files.createDirectories(dir);
        for (LayerBuilder.Dep dep : deps) {
            Path dest = dir.resolve(dep.fileName());
            Files.copy(dep.path(), dest, StandardCopyOption.REPLACE_EXISTING);
            Files.setLastModifiedTime(dest, CANONICAL_APP_MTIME);
        }
        getLog().info("  staged " + deps.size() + " dependency JAR(s) into "
                + dir.getFileName() + "/ (mtimes pinned)");
    }

    /** True when a class-path/module-path list references the given staging dir. */
    static boolean referencesDir(List<String> pathList, String dir) {
        if (pathList == null) {
            return false;
        }
        for (String p : pathList) {
            if (p == null) {
                continue;
            }
            String t = p.trim();
            if (t.equals(dir) || t.equals(dir + "/*") || t.startsWith(dir + "/")) {
                return true;
            }
        }
        return false;
    }

    static List<String> appIntrinsicJvmArgs(JvmConfig cfg) {
        List<String> args = new ArrayList<>();
        if (Boolean.TRUE.equals(cfg.getEnablePreview())) {
            args.add("--enable-preview");
        }
        if (cfg.getAddModules() != null && !cfg.getAddModules().isEmpty()) {
            args.add("--add-modules=" + String.join(",", cfg.getAddModules()));
        }
        if (cfg.getAddOpens() != null) {
            for (String token : cfg.getAddOpens()) {
                args.add("--add-opens");
                args.add(token);
            }
        }
        if (cfg.getAddExports() != null) {
            for (String token : cfg.getAddExports()) {
                args.add("--add-exports");
                args.add(token);
            }
        }
        if (cfg.getSystemProperties() != null) {
            cfg.getSystemProperties().entrySet().stream()
                    .sorted(Comparator.comparing(e -> e.getKey()))
                    .forEach(e -> args.add("-D" + e.getKey() + "=" + e.getValue()));
        }
        return args;
    }

    static int parseJavaFeatureVersion(String versionText) {
        if (versionText == null || versionText.isBlank()) {
            throw new IllegalArgumentException("empty java version");
        }
        Matcher matcher = Pattern.compile("version\\s+\"([^\"]+)\"").matcher(versionText);
        String token = matcher.find()
                ? matcher.group(1)
                : versionText.trim().split("\\s+")[0].replace("\"", "");
        if (token.startsWith("1.")) {
            Matcher legacy = Pattern.compile("^1\\.(\\d+)").matcher(token);
            if (legacy.find()) {
                return Integer.parseInt(legacy.group(1));
            }
        }
        Matcher current = Pattern.compile("^(\\d+)").matcher(token);
        if (current.find()) {
            return Integer.parseInt(current.group(1));
        }
        throw new IllegalArgumentException("unrecognized java version: " + versionText);
    }
}
