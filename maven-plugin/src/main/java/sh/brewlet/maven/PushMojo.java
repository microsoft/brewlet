package sh.brewlet.maven;

import java.io.File;
import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import org.apache.maven.artifact.Artifact;
import org.apache.maven.plugin.AbstractMojo;
import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.plugins.annotations.LifecyclePhase;
import org.apache.maven.plugins.annotations.Mojo;
import org.apache.maven.plugins.annotations.Parameter;
import org.apache.maven.plugins.annotations.ResolutionScope;
import org.apache.maven.project.MavenProject;

@Mojo(name = "push", defaultPhase = LifecyclePhase.PACKAGE,
        requiresDependencyResolution = ResolutionScope.RUNTIME, threadSafe = true)
public final class PushMojo extends AbstractMojo {
    @Parameter(defaultValue = "${project}", readonly = true, required = true)
    private MavenProject project;

    @Parameter(property = "brewlet.executable", defaultValue = "brewlet")
    private String executable;

    @Parameter(property = "brewlet.image", required = true)
    private String image;

    @Parameter(property = "brewlet.store")
    private File store;

    @Parameter(property = "brewlet.platforms")
    private List<String> platforms;

    @Parameter(property = "brewlet.launch")
    private File launch;

    @Parameter(property = "brewlet.mainClass")
    private String mainClass;

    @Parameter(property = "brewlet.module")
    private String module;

    @Parameter
    private List<String> javaArgs = new ArrayList<>();

    @Parameter
    private List<String> runtimeImages = new ArrayList<>();

    @Parameter
    private List<String> layerSets = new ArrayList<>();

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
            Path base = Files.createDirectories(Path.of(project.getBuild().getDirectory(), "brewlet"));
            Path output = Files.createTempDirectory(base, "producer-");
            List<Path> dependencies = new ArrayList<>();
            for (Artifact artifact : project.getArtifacts()) {
                if ("compile".equals(artifact.getScope()) || "runtime".equals(artifact.getScope())) {
                    if (artifact.getFile() == null) {
                        throw new IOException("Unresolved runtime dependency: " + artifact);
                    }
                    dependencies.add(artifact.getFile().toPath());
                }
            }
            JavaProducer.Prepared prepared = JavaProducer.prepare(new JavaProducer.Inputs(
                    application.getFile().toPath(), dependencies,
                    launch == null ? null : launch.toPath(), mainClass, module,
                    javaArgs, runtimeImages), output);
            List<String> command = JavaProducer.command(executable, image,
                    store == null ? null : store.toPath(), platforms, layerSets, prepared);
            Process process = new ProcessBuilder(command)
                    .directory(project.getBasedir()).inheritIO().start();
            int status;
            try {
                status = process.waitFor();
            } catch (InterruptedException interrupted) {
                process.destroy();
                Thread.currentThread().interrupt();
                throw new MojoExecutionException("Brewlet publication interrupted", interrupted);
            }
            if (status != 0) {
                throw new MojoExecutionException("Brewlet CLI rejected publication (exit " + status + ")");
            }
        } catch (IOException | IllegalArgumentException failure) {
            throw new MojoExecutionException("Brewlet Java producer failed: " + failure.getMessage(), failure);
        }
    }
}
