package sh.brewlet.maven;

import java.io.File;
import java.io.IOException;
import java.nio.file.Files;
import java.util.List;
import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.plugins.annotations.Mojo;
import org.apache.maven.plugins.annotations.Parameter;

/** Exercises the producer's shared validator without duplicating its parser. */
@Mojo(name = "conformance", threadSafe = true)
public class ConformanceMojo extends AbstractJavaMojo {
    @Parameter(property = "brewlet.fixtures", required = true)
    private File fixtures;

    @Parameter(property = "brewlet.conformanceResult", required = true)
    private File result;

    @Override
    public void execute() throws MojoExecutionException {
        if (fixtures == null || !fixtures.isFile() || result == null) {
            throw new MojoExecutionException("Conformance requires a fixture file and result path");
        }
        try {
            String output = capture(List.of(executable, "conformance", fixtures.getAbsolutePath()));
            var target = result.toPath().toAbsolutePath();
            Files.createDirectories(target.getParent());
            Files.writeString(target, output);
        } catch (IOException error) {
            throw new MojoExecutionException("Cannot execute or record producer conformance", error);
        }
    }
}
