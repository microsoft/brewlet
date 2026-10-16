package sh.brewlet.maven;

import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.plugins.annotations.Mojo;
import org.apache.maven.plugins.annotations.Parameter;
import org.apache.maven.plugins.annotations.ResolutionScope;
import org.apache.maven.project.ProjectBuildingException;

/** Publish dependencies without requiring or packaging an application JAR. */
@Mojo(name = "layer-set", requiresDependencyResolution = ResolutionScope.RUNTIME, threadSafe = true)
public final class LayerSetMojo extends AbstractJavaMojo {
    @Parameter(property = "brewlet.layerSet", required = true)
    private String layerSet;

    @Parameter(property = "brewlet.layerSetName", defaultValue = "${project.artifactId}", required = true)
    private String name;

    @Parameter(property = "brewlet.layerSetVersion", defaultValue = "${project.version}", required = true)
    private String version;

    @Parameter
    private List<String> compatibleRuntimeImages = new ArrayList<>();

    @Override
    public void execute() throws MojoExecutionException {
        try {
            if (layerSet == null || layerSet.isBlank()) {
                throw new IOException("brewlet.layerSet is required");
            }
            MavenGraph graph = graph();
            Path output = output();
            Path dependencies = graph.stage(Files.createDirectory(output.resolve("dependencies")));
            LayerSets.writeInputs(graph, output, name, version, compatibleRuntimeImages);
            run(LayerSets.publishCommand(executable, layerSet, store == null ? null : store.toPath(),
                    output.resolve("config.json"), output.resolve("lock.json"), dependencies,
                    signingKeyPath(), builderIdentity));
        } catch (IOException | IllegalArgumentException | ProjectBuildingException failure) {
            throw new MojoExecutionException("Brewlet Java layer-set producer failed: "
                    + failure.getMessage(), failure);
        }
    }
}
