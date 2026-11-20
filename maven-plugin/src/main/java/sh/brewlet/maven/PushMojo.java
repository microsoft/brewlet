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

    /** Entries such as {@code 21=java-21}: Java feature release to runtime image ID. */
    @Parameter(property = "brewlet.runtimeImageMappings")
    private List<String> runtimeImageMappings = new ArrayList<>();

    @Parameter(property = "brewlet.appCdsArchive")
    private File appCdsArchive;

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
            for (String platform : platforms) {
                ArtifactContract.parsePlatform(platform);
            }
            List<String> effectiveRuntimeImages = launch == null ? runtimeImages() : List.of();
            for (String id : effectiveRuntimeImages) {
                ArtifactContract.validateRuntimeImageID(id);
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
                    javaArgs, effectiveRuntimeImages, appCdsArchive == null ? null : appCdsArchive.toPath()), output);
            Path layout = store == null ? output.resolve("oci") : store.toPath();
            List<String> command = JavaProducer.command(executable, image, layout, platforms, prepared);
            run(command);
            if (store == null) {
                run(JavaProducer.pushCommand(executable, image, layout));
            }
        } catch (IOException | IllegalArgumentException failure) {
            throw new MojoExecutionException("Brewlet Java producer failed: " + failure.getMessage(), failure);
        }
    }

    /** Proposes runtimeImages from maven.compiler.release, or warns when configured IDs are too old. */
    private List<String> runtimeImages() throws IOException {
        RuntimeImageMapping mapping = RuntimeImageMapping.parse(runtimeImageMappings);
        Integer release = RuntimeImageMapping.release(project.getProperties());
        if (mapping.isEmpty() || release == null) {
            return runtimeImages;
        }
        boolean preview = RuntimeImageMapping.preview(javaArgs);
        if (runtimeImages.isEmpty()) {
            List<String> proposed = mapping.propose(release, preview);
            getLog().info("Proposed runtimeImages for Java " + release + ": " + String.join(", ", proposed));
            return proposed;
        }
        mapping.warnings(runtimeImages, release, preview).forEach(getLog()::warn);
        runtimeImages.stream().filter(id -> !mapping.maps(id))
                .forEach(id -> getLog().debug("runtimeImageMappings has no entry for " + id));
        return runtimeImages;
    }
}
