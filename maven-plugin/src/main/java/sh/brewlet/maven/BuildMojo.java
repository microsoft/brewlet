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

/** Packages the Java layers into a local OCI layout, like {@code brewlet build}. */
@Mojo(name = "build", defaultPhase = LifecyclePhase.PACKAGE,
        requiresDependencyResolution = ResolutionScope.RUNTIME, threadSafe = true)
public final class BuildMojo extends AbstractJavaMojo {
    @Parameter(property = "brewlet.platforms")
    private List<String> platforms;

    @Parameter(property = "brewlet.module")
    private String module;

    @Parameter(property = "brewlet.launch")
    private File launch;

    @Parameter(property = "brewlet.mainClass")
    private String mainClass;

    @Parameter
    private List<String> javaArgs = new ArrayList<>();

    @Parameter(property = "brewlet.appCdsArchive")
    private File appCdsArchive;

    @Override
    public void execute() throws MojoExecutionException {
        if (platforms == null || platforms.isEmpty() || image == null || image.isBlank()) {
            throw new MojoExecutionException("InvalidPlatform: brewlet.image and explicit brewlet.platforms are required");
        }
        Artifact application = project.getArtifact();
        if (application == null || application.getFile() == null) {
            throw new MojoExecutionException("Package the project before brewlet:build");
        }
        try {
            for (String platform : platforms) {
                ArtifactContract.parsePlatform(platform);
            }
            if (launch != null && appCdsArchive != null) {
                getLog().info("brewlet.launch is authoritative; it must reference " + JavaProducer.APP_CDS_PATH
                        + " itself to use the attached AppCDS archive");
            }
            Path output = output();
            List<Path> dependencies = new ArrayList<>();
            for (Artifact artifact : project.getArtifacts()) {
                if (List.of("compile", "runtime").contains(artifact.getScope())) {
                    if (artifact.getFile() == null) {
                        throw new IOException("Unresolved runtime dependency: " + artifact);
                    }
                    dependencies.add(artifact.getFile().toPath());
                }
            }
            JavaProducer.Prepared prepared = JavaProducer.prepare(new JavaProducer.Inputs(
                    application.getFile().toPath(), dependencies,
                    launch == null ? null : launch.toPath(), mainClass, module,
                    javaArgs, appCdsArchive == null ? null : appCdsArchive.toPath()), output);
            run(JavaProducer.command(executable, image, layout(), platforms, prepared));
        } catch (IOException | IllegalArgumentException failure) {
            throw new MojoExecutionException("Brewlet Java producer failed: " + failure.getMessage(), failure);
        }
    }
}
