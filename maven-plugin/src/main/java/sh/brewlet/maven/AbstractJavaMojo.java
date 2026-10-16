package sh.brewlet.maven;

import java.io.File;
import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.List;
import org.apache.maven.artifact.Artifact;
import org.apache.maven.plugin.AbstractMojo;
import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.plugins.annotations.Component;
import org.apache.maven.plugins.annotations.Parameter;
import org.apache.maven.project.DefaultProjectBuildingRequest;
import org.apache.maven.project.MavenProject;
import org.apache.maven.project.ProjectBuilder;
import org.apache.maven.project.ProjectBuildingException;
import org.apache.maven.repository.RepositorySystem;

abstract class AbstractJavaMojo extends AbstractMojo {
    @Parameter(defaultValue = "${project}", readonly = true, required = true)
    protected MavenProject project;

    @Parameter(property = "brewlet.executable", defaultValue = "brewlet")
    protected String executable;

    @Parameter(property = "brewlet.store")
    protected File store;

    @Parameter(property = "brewlet.sourceBom")
    protected String sourceBom;

    @Parameter(property = "brewlet.module")
    protected String module;

    @Parameter(property = "brewlet.signingKey")
    protected File signingKey;

    @Parameter(property = "brewlet.builderIdentity")
    protected String builderIdentity;

    @Component
    private ProjectBuilder projectBuilder;

    @Component
    private RepositorySystem repositorySystem;

    protected MavenGraph graph() throws IOException, ProjectBuildingException {
        MavenGraph graph = new MavenGraph(sourceBom, MavenGraph.runtimeMembers(project.getArtifacts()),
                module == null || module.isBlank() ? "/app/lib" : "/app/mods");
        String[] gav = sourceBom.split(":");
        Artifact bom = repositorySystem.createProjectArtifact(gav[0], gav[1], gav[2]);
        var request = new DefaultProjectBuildingRequest(project.getProjectBuildingRequest());
        request.setResolveDependencies(false);
        request.setRemoteRepositories(project.getRemoteArtifactRepositories());
        MavenProject bomProject = projectBuilder.build(bom, request).getProject();
        if (!"pom".equals(bomProject.getPackaging()) || bomProject.getDependencyManagement() == null) {
            throw new IOException("Source BOM must be a POM with dependencyManagement: " + sourceBom);
        }
        graph.requireBomMembership(bomProject.getDependencyManagement().getDependencies());
        return graph;
    }

    protected Path output() throws IOException {
        Path base = Files.createDirectories(Path.of(project.getBuild().getDirectory(), "brewlet"));
        return Files.createDirectory(base.resolve("producer-" + java.util.UUID.randomUUID()));
    }

    protected void run(List<String> command) throws IOException, MojoExecutionException {
        Process process = new ProcessBuilder(command).directory(project.getBasedir()).inheritIO().start();
        await(process);
    }

    protected String capture(List<String> command) throws IOException, MojoExecutionException {
        Process process = new ProcessBuilder(command).directory(project.getBasedir())
                .redirectError(ProcessBuilder.Redirect.INHERIT).start();
        String output = new String(process.getInputStream().readAllBytes(), java.nio.charset.StandardCharsets.UTF_8);
        await(process);
        return output;
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

    protected String signingKeyPath() {
        return signingKey == null ? null : signingKey.toString();
    }
}
