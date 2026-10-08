// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin.util;

import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.plugin.logging.Log;
import sh.brewlet.maven.plugin.model.Entry;
import sh.brewlet.maven.plugin.model.JvmConfig;

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
import java.time.Duration;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.List;
import java.util.Set;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutionException;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.TimeoutException;
import java.util.concurrent.atomic.AtomicReference;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * The training run shared by {@code brewlet:appcds} (dynamic CDS,
 * {@code -XX:ArchiveClassesAtExit}) and {@code brewlet:aotcache} (JEP 514,
 * {@code -XX:AOTCacheOutput}). Each goal keeps its own {@code @Parameter}
 * fields, because Maven binds {@code -D} properties per field, and hands
 * their values over as {@link Settings}.
 *
 * <p>Two termination strategies: {@code exit} runs a self-terminating app to
 * completion; {@code signal} starts it, waits for readiness, then sends
 * {@code SIGTERM} so the JVM writes the archive/cache on its way out.
 */
public final class TrainingRun {

    public static final String MODE_EXIT = "exit";
    public static final String MODE_SIGNAL = "signal";

    private static final String AOT_OPTION = "-XX:AOTCacheOutput=";

    private static final HttpClient READINESS_HTTP_CLIENT = HttpClient.newBuilder()
            .connectTimeout(Duration.ofSeconds(2))
            .followRedirects(HttpClient.Redirect.NEVER)
            .version(HttpClient.Version.HTTP_1_1)
            .build();

    /**
     * One goal's training configuration.
     *
     * @param goal          names the work dir ({@code <goal>-training}), the version log
     *                      and the output pump thread
     * @param propertyPrefix the goal's property prefix, quoted in error messages
     * @param archiveOption {@code -XX:ArchiveClassesAtExit=} or {@code -XX:AOTCacheOutput=}
     * @param jdkFloor      minimum training JDK feature version
     */
    public record Settings(String goal, String propertyPrefix, String archiveOption, int jdkFloor,
                           List<String> trainingArgs, int timeoutSeconds, File javaHome, String mode,
                           String readyLog, String readyHttp, int readyDelaySeconds,
                           int shutdownGraceSeconds, long readyPollMillis) {
    }

    /** A mojo input resolved only when the run reaches it, preserving the goal's failure order. */
    @FunctionalInterface
    public interface Input<T> {
        T get() throws MojoExecutionException;
    }

    private final Settings settings;
    private final Log log;
    private final Path outputDirectory;
    private final String prefix;
    private final String kind;
    private final String noun;
    private final String artifact;
    private final String attachProperty;
    private final List<String> trainingArgs;
    private final int timeoutSeconds;
    private final String mode;
    private final String readyLog;
    private final String readyHttp;
    private final int readyDelaySeconds;
    private final int shutdownGraceSeconds;
    private final long readyPollMillis;

    public TrainingRun(Settings settings, Log log, Path outputDirectory) {
        this.settings = settings;
        this.log = log;
        this.outputDirectory = outputDirectory;
        boolean aot = AOT_OPTION.equals(settings.archiveOption());
        prefix = settings.propertyPrefix();
        kind = aot ? "AOT" : "AppCDS";
        noun = aot ? "cache" : "archive";
        artifact = kind + " " + noun;
        attachProperty = aot ? "-Dbrewlet.aotCache=" : "-Dbrewlet.cdsArchive=";
        trainingArgs = settings.trainingArgs();
        timeoutSeconds = settings.timeoutSeconds();
        mode = settings.mode();
        readyLog = settings.readyLog();
        readyHttp = settings.readyHttp();
        readyDelaySeconds = settings.readyDelaySeconds();
        shutdownGraceSeconds = settings.shutdownGraceSeconds();
        readyPollMillis = settings.readyPollMillis();
    }

    /**
     * Trains and writes {@code outputFile}: refuses to overwrite the input JAR,
     * clears any previous output, and removes partial output on failure.
     */
    public void train(Input<File> sourceJar, Input<JvmConfig> config, Input<PreparedApplication> application,
                      File outputFile, boolean dryRun) throws MojoExecutionException {
        Path output = outputFile.toPath().toAbsolutePath().normalize();
        if (!dryRun) {
            try {
                Path source = sourceJar.get().toPath().toAbsolutePath().normalize();
                if (output.equals(source) || (Files.exists(output) && Files.isSameFile(output, source))) {
                    throw new MojoExecutionException(artifact + " output must not overwrite the input JAR.");
                }
                Files.deleteIfExists(output);
            } catch (IOException e) {
                throw new MojoExecutionException("Failed to remove previous " + artifact + " output", e);
            }
        }
        try {
            generate(sourceJar, config, application, outputFile, dryRun);
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

    private void generate(Input<File> sourceJar, Input<JvmConfig> config, Input<PreparedApplication> application,
                          File outputFile, boolean dryRun) throws MojoExecutionException {
        if (timeoutSeconds <= 0) {
            throw new MojoExecutionException(prefix + "timeoutSeconds must be greater than zero.");
        }
        String trainingMode = normalizeMode(prefix, mode);
        if (MODE_SIGNAL.equals(trainingMode)) {
            validateReadiness(prefix, readyLog, readyHttp, readyDelaySeconds);
            if (shutdownGraceSeconds <= 0) {
                throw new MojoExecutionException(
                        prefix + "shutdownGraceSeconds must be greater than zero.");
            }
        }

        JvmConfig cfg = config.get();
        PreparedApplication payload = application.get();
        Entry entry = cfg.getEntry();
        String entryMode = entry == null || entry.getMode() == null || entry.getMode().isBlank()
                ? "jar"
                : entry.getMode();

        File javaHome = settings.javaHome() != null
                ? settings.javaHome()
                : new File(System.getProperty("java.home"));
        File java = javaBinary(javaHome);
        String workDir = settings.goal() + "-training";
        Path trainingDir = outputDirectory.resolve(workDir);
        File output = outputFile.getAbsoluteFile();
        if (output.toPath().normalize().startsWith(trainingDir.toAbsolutePath().normalize())) {
            throw new MojoExecutionException(artifact + " output must be outside " + workDir + ", "
                    + "which is cleaned before each run.");
        }
        List<String> command = buildTrainingCommand(settings.archiveOption(), java, cfg, output,
                cfg.getMainJar(), trainingArgs);

        if (dryRun) {
            log.info("Brewlet: dry-run mode — would generate " + artifact + ":");
            log.info("  mode: " + trainingMode
                    + (MODE_SIGNAL.equals(trainingMode) ? " (start → wait for readiness → SIGTERM)" : " (self-terminating)"));
            log.info("  entry: " + entryMode);
            log.info("  working directory: " + trainingDir);
            log.info("  command: " + String.join(" ", command));
            log.info("  training JAR copy mtime: 2000-01-01T00:00:00Z");
            return;
        }

        if (!java.isFile()) {
            throw new MojoExecutionException("Training java binary not found: " + java.getAbsolutePath());
        }

        int feature = detectTrainingJdkFeature(java);
        if (feature < settings.jdkFloor()) {
            throw new MojoExecutionException("brewlet:" + settings.goal() + " requires a JDK "
                    + settings.jdkFloor() + "+ training runtime "
                    + ("appcds".equals(settings.goal())
                        ? "(https://github.com/microsoft/brewlet/blob/main/docs/appcds.md#22-minimum-jdk-version); "
                        : "(-XX:AOTCacheOutput, JEP 514); ")
                    + java.getAbsolutePath()
                    + " reports feature " + feature + ".");
        }

        try {
            Files.createDirectories(output.toPath().getParent());
            Files.deleteIfExists(output.toPath());
        } catch (IOException e) {
            throw new MojoExecutionException("Failed to prepare " + artifact + " output", e);
        }
        Path trainingJar = stageTrainingInputs(sourceJar.get(), cfg, entryMode, payload, trainingDir);

        log.info("Brewlet: generating " + artifact + " ("
                + (MODE_SIGNAL.equals(trainingMode) ? "signal mode: readiness → SIGTERM" : "self-terminating training run") + ")");
        log.info("  entry mode: " + entryMode);
        log.info("  java: " + java.getAbsolutePath() + " (feature " + feature + ")");
        log.info("  training jar: " + trainingJar + " (mtime pinned to 2000-01-01T00:00:00Z)");
        log.info("  " + noun + ": " + output.getAbsolutePath());
        log.info("  command: " + String.join(" ", command));

        int exitCode = MODE_SIGNAL.equals(trainingMode)
                ? runSignalTraining(command, trainingDir.toFile())
                : runSelfTerminating(command, trainingDir.toFile());

        if (!output.isFile() || output.length() == 0) {
            throw new MojoExecutionException(kind + " training exited with code " + exitCode
                    + " but did not write a non-empty " + noun + " at " + output.getAbsolutePath()
                    + (MODE_SIGNAL.equals(trainingMode)
                        ? ". In signal mode the app's shutdown hook must let the JVM exit cleanly so "
                          + ("archive".equals(noun) ? "dynamic CDS can flush the archive" : "the JVM can create the AOT cache")
                          + "; check that SIGTERM triggers a graceful shutdown."
                        : ""));
        }
        if (exitCode != 0 && !(MODE_SIGNAL.equals(trainingMode) && exitCode == 143)) {
            throw new MojoExecutionException(kind + " training failed with exit code " + exitCode
                    + "; discarding its " + noun + ".");
        }

        log.info("Brewlet: wrote " + artifact + " → " + output.getAbsolutePath());
        log.info("Attach it with brewlet:build/push using " + attachProperty
                + output.getAbsolutePath());
    }

    /** {@code exit} mode: run the self-terminating training JVM to completion. */
    public int runSelfTerminating(List<String> command, File workingDir)
            throws MojoExecutionException {
        try (TrainingProcess training = startTraining(command, workingDir, null, null)) {
            Process process = training.process;
            if (!process.waitFor(timeoutSeconds, TimeUnit.SECONDS)) {
                throw new MojoExecutionException(kind + " training timed out after " + timeoutSeconds
                        + "s. This mode expects a self-terminating app: it must boot, exercise startup "
                        + "paths, and exit. Raise -D" + prefix + "timeoutSeconds=..., use "
                        + "-D" + prefix + "mode=signal for long-running servers, or supply a prebuilt "
                        + noun + " with " + attachProperty + "...");
            }
            training.checkOutput();
            return process.exitValue();
        } catch (IOException e) {
            throw new MojoExecutionException("Failed to launch " + kind + " training JVM", e);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            throw new MojoExecutionException("Interrupted while waiting for " + kind + " training JVM", e);
        }
    }

    /**
     * {@code signal} mode: start the app, wait for readiness, then {@code SIGTERM}
     * so the JVM shutdown hook runs and dynamic CDS flushes the archive at exit.
     */
    public int runSignalTraining(List<String> command, File workingDir)
            throws MojoExecutionException {
        validateReadiness(prefix, readyLog, readyHttp, readyDelaySeconds);
        if (timeoutSeconds <= 0 || shutdownGraceSeconds <= 0 || readyPollMillis <= 0) {
            throw new MojoExecutionException(kind + " timeout, shutdown grace, and poll interval must be positive.");
        }
        CountDownLatch logReady = new CountDownLatch(1);
        Pattern readyPattern = readyLog == null || readyLog.isBlank() ? null : Pattern.compile(readyLog);

        try (TrainingProcess training = startTraining(command, workingDir, readyPattern, logReady)) {
            Process process = training.process;
            long deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(timeoutSeconds);
            awaitReadiness(training, readyPattern, logReady, deadline);
            if (readyDelaySeconds > 0) {
                log.info("  readiness reached; settling for " + readyDelaySeconds + "s before SIGTERM");
                long remaining = Math.max(0, deadline - System.nanoTime());
                long settle = TimeUnit.SECONDS.toNanos(readyDelaySeconds);
                if (process.waitFor(Math.min(remaining, settle), TimeUnit.NANOSECONDS)) {
                    throw new MojoExecutionException("Training JVM exited during the readiness/settle delay.");
                }
                if (remaining < settle) {
                    throw new MojoExecutionException(kind + " readiness/settle delay exceeded timeoutSeconds="
                            + timeoutSeconds + ". Raise the timeout or reduce readyDelaySeconds.");
                }
            }

            training.checkOutput();
            log.info("  sending SIGTERM to let the app shut down and flush the " + noun);
            // Process.destroy() closes the pipes on Linux. Signal the owned child
            // without closing stdout/stderr so the pump can drain shutdown output.
            process.toHandle().destroy();
            if (!process.waitFor(shutdownGraceSeconds, TimeUnit.SECONDS)) {
                training.kill(true);
                throw new MojoExecutionException("Training JVM did not exit within "
                        + shutdownGraceSeconds + "s of SIGTERM; the " + noun + " was likely not flushed. "
                        + "Ensure the app shuts down gracefully on SIGTERM, or raise "
                        + "-D" + prefix + "shutdownGraceSeconds=...");
            }
            training.checkOutput();
            return process.exitValue();
        } catch (IOException e) {
            throw new MojoExecutionException("Failed to launch " + kind + " training JVM", e);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            throw new MojoExecutionException("Interrupted while training " + artifact, e);
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
            log.info("  waiting for readiness: HTTP 2xx/3xx from " + readyHttp);
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
                    log.info("  readiness reached via HTTP probe");
                    return;
                }
                TimeUnit.NANOSECONDS.sleep(Math.max(0, Math.min(
                        TimeUnit.MILLISECONDS.toNanos(readyPollMillis), deadline - System.nanoTime())));
            }
            throw new MojoExecutionException("Timed out after " + timeoutSeconds
                    + "s waiting for " + readyHttp + " to become ready.");
        }

        if (readyPattern != null) {
            log.info("  waiting for readiness: log match /" + readyPattern.pattern() + "/");
            while (System.nanoTime() < deadline) {
                training.checkOutput();
                if (!process.isAlive()) {
                    throw new MojoExecutionException("Training JVM exited (code " + process.exitValue()
                            + ") before the readyLog pattern matched.");
                }
                if (logReady.await(Math.max(0, Math.min(TimeUnit.MILLISECONDS.toNanos(100),
                        deadline - System.nanoTime())), TimeUnit.NANOSECONDS)) {
                    log.info("  readiness reached via log match");
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
                log.info("  [training] " + line);
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
        // JEP 514 assembles the AOT cache in a child JVM spawned at exit. Remember
        // descendants while the parent is alive: once it dies they are reparented
        // and could still write the cache after a failed run.
        private final Set<ProcessHandle> descendants = ConcurrentHashMap.newKeySet();

        TrainingProcess(Process process, Pattern pattern, CountDownLatch ready) {
            this.process = process;
            pump = new Thread(() -> pumpOutput(process.getInputStream(), pattern, ready, outputFailure),
                    "brewlet-" + settings.goal() + "-training-io-" + process.pid());
            pump.setDaemon(true);
            pump.start();
        }

        void checkOutput() throws MojoExecutionException {
            Throwable failure = outputFailure.get();
            if (failure != null) throw new MojoExecutionException("Failed to read " + kind + " training output", failure);
        }

        /** Signals the training JVM and every descendant seen so far. */
        void kill(boolean force) {
            snapshotDescendants();
            for (ProcessHandle handle : descendants) {
                if (force) handle.destroyForcibly(); else handle.destroy();
            }
            if (force) process.toHandle().destroyForcibly(); else process.toHandle().destroy();
        }

        private void snapshotDescendants() {
            if (process.isAlive()) process.descendants().forEach(descendants::add);
        }

        private List<Long> alive() {
            List<Long> pids = new ArrayList<>();
            if (process.isAlive()) pids.add(process.pid());
            descendants.stream().filter(ProcessHandle::isAlive).forEach(h -> pids.add(h.pid()));
            return pids;
        }

        /** Cleanup cannot abandon the processes when the caller is already interrupted. */
        private boolean waitForCleanup(int seconds) {
            boolean interrupted = false;
            long deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(seconds);
            while (!alive().isEmpty() && System.nanoTime() < deadline) {
                // A SIGTERMed JVM may spawn the assembly child while we wait.
                snapshotDescendants();
                long slice = Math.max(0, Math.min(TimeUnit.MILLISECONDS.toNanos(50), deadline - System.nanoTime()));
                try {
                    if (process.isAlive()) process.waitFor(slice, TimeUnit.NANOSECONDS);
                    else TimeUnit.NANOSECONDS.sleep(slice);
                } catch (InterruptedException e) {
                    interrupted = true;
                }
            }
            return interrupted;
        }

        @Override
        public void close() throws MojoExecutionException {
            boolean interrupted = Thread.interrupted();
            MojoExecutionException failure = null;
            try {
                if (!alive().isEmpty()) {
                    kill(false);
                    interrupted |= waitForCleanup(5);
                    if (!alive().isEmpty()) {
                        kill(true);
                        interrupted |= waitForCleanup(5);
                    }
                }
                if (!alive().isEmpty()) {
                    failure = new MojoExecutionException("Could not reap " + kind + " training JVM PID " + process.pid()
                            + " (still alive: " + alive() + ")");
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
                    failure = new MojoExecutionException("Failed to read " + kind + " training output", outputFailure.get());
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
                    failure = new MojoExecutionException(kind + " output pump did not stop for PID " + process.pid());
                }
                if (interrupted && failure == null) {
                    failure = new MojoExecutionException("Interrupted while reaping " + kind + " training JVM");
                }
                if (failure != null) throw failure;
            } finally {
                if (interrupted) Thread.currentThread().interrupt();
            }
        }
    }

    /** Returns true when an HTTP(S) GET to {@code url} answers with a 2xx/3xx status. */
    public static boolean httpReady(String url) throws MojoExecutionException, InterruptedException {
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
    public static String normalizeMode(String prefix, String raw) throws MojoExecutionException {
        String m = raw == null ? MODE_EXIT : raw.trim().toLowerCase(java.util.Locale.ROOT);
        if (m.isEmpty()) {
            return MODE_EXIT;
        }
        if (!MODE_EXIT.equals(m) && !MODE_SIGNAL.equals(m)) {
            throw new MojoExecutionException(prefix + "mode \"" + raw
                    + "\" is not recognized (expected \"exit\" or \"signal\").");
        }
        return m;
    }

    /** In signal mode at least one readiness signal must be configured. */
    public static void validateReadiness(String prefix, String readyLog, String readyHttp, int readyDelaySeconds)
            throws MojoExecutionException {
        boolean hasLog = readyLog != null && !readyLog.isBlank();
        boolean hasHttp = readyHttp != null && !readyHttp.isBlank();
        if (readyDelaySeconds < 0) {
            throw new MojoExecutionException(prefix + "readyDelaySeconds must not be negative.");
        }
        if (hasHttp) {
            try {
                URI uri = new URI(readyHttp);
                if ((!"http".equalsIgnoreCase(uri.getScheme()) && !"https".equalsIgnoreCase(uri.getScheme()))
                        || uri.getHost() == null) {
                    throw new URISyntaxException(readyHttp, "expected an HTTP(S) URL with a host");
                }
            } catch (URISyntaxException e) {
                throw new MojoExecutionException("Invalid " + prefix + "readyHttp: " + e.getMessage(), e);
            }
        }
        if (hasLog) {
            try {
                Pattern.compile(readyLog);
            } catch (java.util.regex.PatternSyntaxException e) {
                throw new MojoExecutionException(prefix + "readyLog is not a valid regex: "
                        + e.getMessage(), e);
            }
        }
        if (!hasLog && !hasHttp && readyDelaySeconds <= 0) {
            throw new MojoExecutionException(prefix + "mode=signal requires a readiness signal: "
                    + "set one of -D" + prefix + "readyLog=<regex>, "
                    + "-D" + prefix + "readyHttp=<url>, or -D" + prefix + "readyDelaySeconds=<n>.");
        }
    }

    private int detectTrainingJdkFeature(File java) throws MojoExecutionException {
        if (settings.javaHome() == null) {
            return parseJavaFeatureVersion(System.getProperty("java.version"));
        }
        Path versionOutput = outputDirectory.resolve(settings.goal() + "-java-version.log");
        try {
            Files.createDirectories(versionOutput.toAbsolutePath().getParent());
            Process process = new ProcessBuilder(java.getAbsolutePath(), "-version")
                    .redirectErrorStream(true)
                    .redirectOutput(versionOutput.toFile())
                    .start();
            try (TrainingProcess training = new TrainingProcess(process, null, null)) {
                if (!process.waitFor(10, TimeUnit.SECONDS)) {
                    throw new MojoExecutionException("Timed out running " + java.getAbsolutePath()
                            + " -version to verify the JDK " + settings.jdkFloor() + "+ " + kind + " floor.");
                }
                if (process.exitValue() != 0) {
                    throw new MojoExecutionException("Training java -version failed with code " + process.exitValue());
                }
                return parseJavaFeatureVersion(Files.readString(versionOutput));
            }
        } catch (IOException e) {
            throw new MojoExecutionException("Failed to run " + java.getAbsolutePath()
                    + " -version to verify the JDK " + settings.jdkFloor() + "+ " + kind + " floor", e);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            throw new MojoExecutionException("Interrupted while checking training JDK version", e);
        } catch (IllegalArgumentException e) {
            throw new MojoExecutionException("Could not parse training JDK version from "
                    + java.getAbsolutePath() + " -version", e);
        }
    }

    public static File javaBinary(File javaHome) {
        String exe = System.getProperty("os.name", "").toLowerCase().contains("win")
                ? "java.exe"
                : "java";
        return javaHome.toPath().resolve("bin").resolve(exe).toFile();
    }

    public static List<String> buildTrainingCommand(String archiveOption, File javaBinary, JvmConfig cfg,
                                                    File archiveOutput, String mainJar,
                                                    List<String> trainingArgs) {
        List<String> command = new ArrayList<>();
        command.add(javaBinary.getAbsolutePath());
        command.addAll(appIntrinsicJvmArgs(cfg));
        command.add(archiveOption + archiveOutput.getAbsolutePath());
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
    public static List<String> launchSelector(JvmConfig cfg, String mainJar) {
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
    public Path stageTrainingInputs(File sourceJar, JvmConfig cfg, String entryMode, PreparedApplication payload, Path trainingDir)
            throws MojoExecutionException {
        try {
            Path root = trainingDir.toAbsolutePath().normalize();
            Path realRoot = Files.exists(root) ? root.toRealPath() : root;
            List<Path> inputs = new ArrayList<>(List.of(sourceJar.toPath(), payload.jar().toPath()));
            for (var dep : payload.dependencies()) inputs.add(dep.path());
            for (Path input : inputs) {
                if (input.toAbsolutePath().normalize().startsWith(root) || input.toRealPath().startsWith(realRoot)) {
                    throw new MojoExecutionException(kind + " training directory must not contain its input JARs.");
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
            Files.setLastModifiedTime(trainingJar, PreparedApplication.CANONICAL_MTIME);

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
            throw new MojoExecutionException("Failed to stage " + kind + " training inputs", e);
        }
    }

    /** Copies the resolved runtime dependency JARs into {@code dir}, pinning mtimes. */
    private void stageDeps(Path dir, List<LayerBuilder.Dep> deps) throws IOException {
        LayerBuilder.validateDependencies(deps);
        Files.createDirectories(dir);
        for (LayerBuilder.Dep dep : deps) {
            Path dest = dir.resolve(dep.fileName());
            Files.copy(dep.path(), dest, StandardCopyOption.REPLACE_EXISTING);
            Files.setLastModifiedTime(dest, PreparedApplication.CANONICAL_MTIME);
        }
        log.info("  staged " + deps.size() + " dependency JAR(s) into "
                + dir.getFileName() + "/ (mtimes pinned)");
    }

    /** True when a class-path/module-path list references the given staging dir. */
    public static boolean referencesDir(List<String> pathList, String dir) {
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

    public static List<String> appIntrinsicJvmArgs(JvmConfig cfg) {
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

    public static int parseJavaFeatureVersion(String versionText) {
        if (versionText == null || versionText.isBlank()) {
            throw new IllegalArgumentException("empty java version");
        }
        Matcher matcher = Pattern.compile("version\\s+\"([^\"]+)\"").matcher(versionText);
        String token = matcher.find()
                ? matcher.group(1)
                : versionText.trim().split("\\s+")[0].replace("\"", "");
        if (token.startsWith("1.")) {
            Matcher oneDotVersion = Pattern.compile("^1\\.(\\d+)").matcher(token);
            if (oneDotVersion.find()) {
                return Integer.parseInt(oneDotVersion.group(1));
            }
        }
        Matcher current = Pattern.compile("^(\\d+)").matcher(token);
        if (current.find()) {
            return Integer.parseInt(current.group(1));
        }
        throw new IllegalArgumentException("unrecognized java version: " + versionText);
    }
}
