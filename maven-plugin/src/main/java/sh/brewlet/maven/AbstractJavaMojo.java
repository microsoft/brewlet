package sh.brewlet.maven;

import java.io.File;
import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.List;
import org.apache.maven.plugin.AbstractMojo;
import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.plugins.annotations.Parameter;
import org.apache.maven.project.MavenProject;

abstract class AbstractJavaMojo extends AbstractMojo {
    @Parameter(defaultValue = "${project}", readonly = true, required = true)
    protected MavenProject project;

    @Parameter(property = "brewlet.executable", defaultValue = "brewlet")
    protected String executable;

    @Parameter(property = "brewlet.image", required = true)
    protected String image;

    /** OCI layout written by {@code build} and read by {@code push}. */
    @Parameter(property = "brewlet.store", defaultValue = "${project.build.directory}/brewlet/oci")
    protected File store;

    protected void requireImage() throws MojoExecutionException {
        if (image == null || image.isBlank()) {
            throw new MojoExecutionException("brewlet.image is required");
        }
    }

    protected Path layout() {
        return store == null ? Path.of(project.getBuild().getDirectory(), "brewlet", "oci") : store.toPath();
    }

    protected Path output() throws IOException {
        Path base = Files.createDirectories(Path.of(project.getBuild().getDirectory(), "brewlet"));
        return Files.createDirectory(base.resolve("producer-" + java.util.UUID.randomUUID()));
    }

    protected void run(List<String> command) throws IOException, MojoExecutionException {
        Process process = new ProcessBuilder(command).directory(project.getBasedir()).inheritIO().start();
        await(process);
    }

    private static void await(Process process) throws MojoExecutionException {
        try {
            int status = process.waitFor();
            if (status != 0) {
                throw new MojoExecutionException("Brewlet CLI rejected operation (exit " + status + ")");
            }
        } catch (InterruptedException interrupted) {
            process.destroy();
            Thread.currentThread().interrupt();
            throw new MojoExecutionException("Brewlet operation interrupted", interrupted);
        }
    }

}
