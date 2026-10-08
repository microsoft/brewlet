// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.plugin.MojoFailureException;
import org.apache.maven.plugins.annotations.Mojo;
import org.apache.maven.plugins.annotations.Parameter;
import org.apache.maven.plugins.annotations.ResolutionScope;
import sh.brewlet.maven.plugin.util.TrainingRun;

import java.io.File;
import java.util.List;

/**
 * <strong>brewlet:aotcache</strong> — Generate a JDK AOT cache (JEP 514,
 * {@code -XX:AOTCacheOutput}) with a training run. Requires a JDK 25+
 * training runtime.
 *
 * <p>Usage:
 * <pre>
 * mvn package brewlet:aotcache \
 *   -Dbrewlet.aotcache.mode=signal \
 *   -Dbrewlet.aotcache.readyLog='Started .* in .* seconds'
 * mvn brewlet:push -Dbrewlet.image=registry.example.com/team/app:1.0 \
 *   -Dbrewlet.aotCache=target/brewlet/app.aot
 * </pre>
 *
 * <p>Training mirrors {@code brewlet:appcds}: the same {@code exit} /
 * {@code signal} modes, readiness signals and staged runtime layout, under the
 * {@code brewlet.aotcache.*} properties. In {@code signal} mode the cache is
 * created after {@code SIGTERM}, so the default shutdown grace is 120 seconds.
 */
@Mojo(name = "aotcache",
      requiresProject = true,
      requiresDependencyResolution = ResolutionScope.RUNTIME,
      threadSafe = true)
public class AotCacheMojo extends AbstractBrewletMojo {

    /**
     * Output path for the generated AOT cache.
     */
    @Parameter(defaultValue = "${project.build.directory}/brewlet/app.aot")
    private File aotCacheOutput;

    /**
     * Extra program arguments passed after {@code -jar <mainJar>} for the
     * self-terminating training run.
     */
    @Parameter(property = "brewlet.aotcache.trainingArgs")
    private List<String> trainingArgs;

    /**
     * Maximum time to wait for the app to boot, exercise startup paths, and exit
     * ({@code exit} mode) or become ready and complete the settle delay
     * ({@code signal} mode). Shutdown grace is a separate bound.
     */
    @Parameter(property = "brewlet.aotcache.timeoutSeconds", defaultValue = "120")
    private int timeoutSeconds;

    /**
     * JDK used for training. Defaults to the JDK running Maven
     * ({@code java.home}); {@code -XX:AOTCacheOutput} requires JDK 25+.
     */
    @Parameter(property = "brewlet.aotcache.javaHome")
    private File trainingJavaHome;

    /**
     * Training termination strategy: {@code exit} (self-terminating, default) or
     * {@code signal} (start, wait for readiness, then {@code SIGTERM}).
     */
    @Parameter(property = "brewlet.aotcache.mode", defaultValue = TrainingRun.MODE_EXIT)
    private String mode;

    /**
     * {@code signal} mode readiness: a regular expression matched line-by-line
     * against the training JVM's combined stdout/stderr. The app is considered
     * warmed up as soon as a line matches (e.g. {@code "Started .* in .* seconds"}
     * for Spring Boot).
     */
    @Parameter(property = "brewlet.aotcache.readyLog")
    private String readyLog;

    /**
     * {@code signal} mode readiness: an HTTP(S) URL polled until it returns a
     * 2xx/3xx status (e.g. {@code http://localhost:8080/actuator/health}).
     */
    @Parameter(property = "brewlet.aotcache.readyHttp")
    private String readyHttp;

    /**
     * {@code signal} mode readiness: a fixed warmup delay (seconds) before
     * {@code SIGTERM}. Used alone as a simple fallback, or as a settle time after
     * {@code readyLog}/{@code readyHttp} fires.
     */
    @Parameter(property = "brewlet.aotcache.readyDelaySeconds", defaultValue = "0")
    private int readyDelaySeconds;

    /**
     * {@code signal} mode: how long to wait after {@code SIGTERM} for the app to
     * shut down and create the cache before force-killing. Longer than the
     * {@code brewlet:appcds} default because JEP 514 creates the cache after
     * {@code SIGTERM}.
     */
    @Parameter(property = "brewlet.aotcache.shutdownGraceSeconds", defaultValue = "120")
    private int shutdownGraceSeconds;

    /**
     * {@code signal} mode: poll interval (milliseconds) for {@code readyHttp}.
     */
    @Parameter(property = "brewlet.aotcache.readyPollMillis", defaultValue = "500")
    private long readyPollMillis;


    @Override
    protected void doExecute() throws MojoExecutionException, MojoFailureException {
        training().train(this::resolveJarFile, this::buildConfig, this::prepareApplication,
                aotCacheOutput, dryRun);
    }

    TrainingRun training() {
        return new TrainingRun(new TrainingRun.Settings("aotcache", "brewlet.aotcache.", "-XX:AOTCacheOutput=", 25,
                trainingArgs, timeoutSeconds, trainingJavaHome, mode, readyLog, readyHttp, readyDelaySeconds,
                shutdownGraceSeconds, readyPollMillis),
                getLog(), outputDirectory == null ? null : outputDirectory.toPath());
    }
}
