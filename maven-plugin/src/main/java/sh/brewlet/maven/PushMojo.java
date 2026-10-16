package sh.brewlet.maven;

import java.io.File;
import java.io.IOException;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import org.apache.maven.artifact.Artifact;
import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.plugins.annotations.LifecyclePhase;
import org.apache.maven.plugins.annotations.Mojo;
import org.apache.maven.plugins.annotations.Parameter;
import org.apache.maven.plugins.annotations.ResolutionScope;
import org.apache.maven.project.ProjectBuildingException;

@Mojo(name = "push", defaultPhase = LifecyclePhase.PACKAGE,
        requiresDependencyResolution = ResolutionScope.RUNTIME, threadSafe = true)
public final class PushMojo extends AbstractJavaMojo {
    @Parameter(property = "brewlet.image", required = true)
    private String image;

    @Parameter(property = "brewlet.platforms")
    private List<String> platforms;

    @Parameter(property = "brewlet.launch")
    private File launch;

    @Parameter(property = "brewlet.mainClass")
    private String mainClass;

    @Parameter
    private List<String> javaArgs = new ArrayList<>();

    @Parameter
    private List<String> runtimeImages = new ArrayList<>();

    @Parameter
    private List<String> layerSets = new ArrayList<>();

    @Parameter
    private List<String> trustKeys = new ArrayList<>();

    @Parameter(property = "brewlet.signerIdentity")
    private String signerIdentity;

    @Parameter(property = "brewlet.requireProvenance", defaultValue = "false")
    private boolean requireProvenance;

    @Override
    public void execute() throws MojoExecutionException {
        if (platforms == null || platforms.isEmpty() || image == null || image.isBlank()) {
            throw new MojoExecutionException("InvalidPlatform: brewlet.image and explicit brewlet.platforms are required");
        }
        Artifact application = project.getArtifact();
        if (application == null || application.getFile() == null) {
            throw new MojoExecutionException("Package the project before brewlet:push");
        }
        try {
            Path output = output();
            List<Path> dependencies = new ArrayList<>();
            LayerSets.Trust trust = new LayerSets.Trust(trustKeys, signerIdentity, requireProvenance);
            List<String> pinned = new ArrayList<>();
            if (!layerSets.isEmpty()) {
                if (layerSets.size() != 1) {
                    throw new IOException("Exactly one managed Java layer set is supported per application");
                }
                JavaProducer.requireThinJar(application.getFile().toPath());
                MavenGraph graph = graph();
                String reference = layerSets.get(0);
                String inspected = capture(LayerSets.inspectCommand(executable, reference, trust));
                pinned.add(LayerSets.verifyAndPin(reference, MavenGraph.JSON.readTree(inspected), graph));
            } else {
                for (MavenGraph.Member member : MavenGraph.runtimeMembers(project.getArtifacts())) {
                    dependencies.add(member.file());
                }
            }
            JavaProducer.Prepared prepared = JavaProducer.prepare(new JavaProducer.Inputs(
                    application.getFile().toPath(), dependencies,
                    launch == null ? null : launch.toPath(), mainClass, module,
                    javaArgs, runtimeImages), output);
            if (!pinned.isEmpty()) {
                JavaProducer.requireThinJar(prepared.application().resolve("app.jar"));
            }
            List<String> command = JavaProducer.command(executable, image,
                    store == null ? null : store.toPath(), platforms, pinned, prepared);
            trust.append(command);
            run(command);
        } catch (IOException | IllegalArgumentException | ProjectBuildingException failure) {
            throw new MojoExecutionException("Brewlet Java producer failed: " + failure.getMessage(), failure);
        }
    }
}
