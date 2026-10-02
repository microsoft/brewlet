// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.plugin.MojoFailureException;
import org.apache.maven.plugins.annotations.LifecyclePhase;
import org.apache.maven.plugins.annotations.Mojo;
import org.apache.maven.plugins.annotations.ResolutionScope;

/**
 * <strong>brewlet:push</strong> — Build the Brewlet OCI artifact and push it to
 * the registry specified by {@code <image>}. Optionally bound to the
 * {@code deploy} phase.
 *
 * <p>The push mirrors the Go CLI's {@code brewlet push} command:
 * <ol>
 *   <li>Serialize the JvmConfig → {@code application/vnd.brewlet.jvm.config.v1+json} blob.</li>
 *   <li>Push the JAR → {@code application/vnd.brewlet.jar.layer.v1+jar} layer blob.</li>
 *   <li>Build an OCI manifest with {@code artifactType: application/vnd.brewlet.app.v1+json}.</li>
 *   <li>Push the manifest and tag it.</li>
 * </ol>
 *
 * <p>Example:
 * <pre>
 * mvn clean package brewlet:push -Dbrewlet.image=registry.example.com/team/app:1.0
 * mvn clean package brewlet:push -Dbrewlet.registry=registry.example.com/team
 * </pre>
 *
 * <p>The digest-pinned result is recorded in {@code target/brewlet/push.json},
 * which {@code brewlet:manifest} picks up automatically.
 *
 * <p>Or bind to the deploy lifecycle:
 * <pre>{@code
 * <executions>
 *   <execution>
 *     <goals><goal>push</goal></goals>
 *   </execution>
 * </executions>
 * }</pre>
 */
@Mojo(name = "push",
      defaultPhase = LifecyclePhase.DEPLOY,
      requiresProject = true,
      requiresDependencyResolution = ResolutionScope.RUNTIME,
      threadSafe = true)
public class PushMojo extends AbstractPushMojo {

    @Override
    protected void doExecute() throws MojoExecutionException, MojoFailureException {
        pushApplication("push");
    }
}
