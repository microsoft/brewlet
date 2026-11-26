package sh.brewlet.maven;

import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.plugins.annotations.LifecyclePhase;
import org.apache.maven.plugins.annotations.Mojo;

/** Uploads the layout written by {@code brewlet:build} unchanged, like {@code brewlet push}. */
@Mojo(name = "push", defaultPhase = LifecyclePhase.DEPLOY, threadSafe = true)
public final class PushMojo extends AbstractJavaMojo {
    @Override
    public void execute() throws MojoExecutionException {
        requireImage();
        Path layout = layout();
        if (!Files.isRegularFile(layout.resolve("index.json"))) {
            throw new MojoExecutionException("No Brewlet build in " + layout + "; run brewlet:build first");
        }
        try {
            run(JavaProducer.pushCommand(executable, image, layout));
        } catch (IOException failure) {
            throw new MojoExecutionException("Brewlet push failed: " + failure.getMessage(), failure);
        }
    }
}
