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
 * <p>Reuses the exact image already assembled by build/push for this project in
 * the same Maven invocation, or assembles it once when invoked on its own.
 * Uploads its blobs, then child manifests, then the tagged image index (or native
 * artifact manifest when {@code format=artifact}).
 *
 * <p>Example:
 * <pre>
 * mvn clean package brewlet:push -Dbrewlet.image=registry.example.com/team/app:1.0
 * mvn clean package brewlet:push -Dbrewlet.registry=registry.example.com/team
 * </pre>
 *
 * <p>The digest-pinned result is recorded in {@code target/brewlet/push.json},
 * for downstream deployment tooling to consume without rebuilding.
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
