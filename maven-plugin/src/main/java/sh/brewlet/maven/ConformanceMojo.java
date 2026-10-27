package sh.brewlet.maven;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import java.io.File;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import org.apache.maven.plugin.AbstractMojo;
import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.plugins.annotations.Mojo;
import org.apache.maven.plugins.annotations.Parameter;

/**
 * Evaluates the shared conformance fixtures with the plugin's independent Java
 * contract implementation using the normalized shared fixture result schema.
 */
@Mojo(name = "conformance", threadSafe = true)
public class ConformanceMojo extends AbstractMojo {
    @Parameter(property = "brewlet.fixtures", required = true)
    private File fixtures;

    @Parameter(property = "brewlet.conformanceResult", required = true)
    private File result;

    @Override
    public void execute() throws MojoExecutionException {
        if (fixtures == null || !fixtures.isFile() || result == null) {
            throw new MojoExecutionException("Conformance requires a fixture file and result path");
        }
        JsonNode results;
        try {
            results = Conformance.run(new String(Files.readAllBytes(fixtures.toPath()), StandardCharsets.UTF_8));
        } catch (IOException | IllegalArgumentException invalid) {
            throw new MojoExecutionException("Cannot evaluate conformance fixtures: " + invalid.getMessage(), invalid);
        }
        for (JsonNode item : results) {
            if (!item.path("passed").booleanValue()) {
                throw new MojoExecutionException("conformance fixture failed: " + item.path("name").asText()
                        + " " + item);
            }
        }
        try {
            var target = result.toPath().toAbsolutePath();
            Files.createDirectories(target.getParent());
            Files.writeString(target, new ObjectMapper().writeValueAsString(results) + "\n");
        } catch (IOException error) {
            throw new MojoExecutionException("Cannot record producer conformance", error);
        }
    }
}
