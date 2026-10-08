// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.plugin.MojoFailureException;
import org.apache.maven.plugins.annotations.Mojo;
import org.apache.maven.plugins.annotations.Parameter;
import org.apache.maven.plugins.annotations.ResolutionScope;
import sh.brewlet.maven.plugin.util.PreparedApplication;
import sh.brewlet.maven.plugin.util.TrainingRun;

import java.io.File;
import java.nio.file.attribute.FileTime;
import java.util.List;

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
    @Parameter(property = "brewlet.appcds.mode", defaultValue = TrainingRun.MODE_EXIT)
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
        training().train(this::resolveJarFile, this::buildConfig, this::prepareApplication,
                cdsArchiveOutput, dryRun);
    }

    TrainingRun training() {
        return new TrainingRun(new TrainingRun.Settings("appcds", "brewlet.appcds.", "-XX:ArchiveClassesAtExit=", 21,
                trainingArgs, timeoutSeconds, trainingJavaHome, mode, readyLog, readyHttp, readyDelaySeconds,
                shutdownGraceSeconds, readyPollMillis),
                getLog(), outputDirectory == null ? null : outputDirectory.toPath());
    }
}
