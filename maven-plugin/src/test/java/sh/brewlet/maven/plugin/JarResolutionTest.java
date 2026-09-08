// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import org.apache.maven.artifact.DefaultArtifact;
import org.apache.maven.artifact.handler.DefaultArtifactHandler;
import org.apache.maven.model.Build;
import org.apache.maven.model.Plugin;
import org.apache.maven.model.PluginExecution;
import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.project.MavenProject;
import org.codehaus.plexus.util.xml.Xpp3Dom;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

import java.io.File;
import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.List;
import java.util.jar.Attributes;
import java.util.jar.JarOutputStream;
import java.util.jar.Manifest;

import static org.junit.jupiter.api.Assertions.*;

class JarResolutionTest {
    @TempDir
    Path directory;

    @Test
    void freshProjectFindsPackagedMainJarAndGeneratesConfig() throws Exception {
        ConfigMojo mojo = mojo();
        File jar = jar(directory.resolve("build-output/orders-release.jar"));
        assertNull(mojo.project.getArtifact().getFile());

        assertEquals(jar, mojo.resolveJarFile());
        mojo.doExecute();
        assertEquals("orders-release.jar", AbstractBrewletMojo.MAPPER.readTree(
                directory.resolve("brewlet/jvm-config.json").toFile()).path("mainJar").asText());
    }

    @Test
    void relativeBuildDirectoryIsResolvedAgainstProjectBaseDirectory() throws Exception {
        ConfigMojo mojo = mojo();
        mojo.project.getBuild().setDirectory("build-output");
        File jar = jar(directory.resolve("build-output/orders-release.jar"));
        assertEquals(jar, mojo.resolveJarFile());
    }

    @Test
    void explicitJarWinsOverAssociatedAndConventionalArtifacts() throws Exception {
        ConfigMojo mojo = mojo();
        jar(directory.resolve("build-output/orders-release.jar"));
        mojo.project.getArtifact().setFile(jar(directory.resolve("associated.jar")));
        mojo.jarFile = jar(directory.resolve("explicit.jar"));
        assertEquals(mojo.jarFile, mojo.resolveJarFile());
    }

    @Test
    void associatedArtifactWinsOverConventionalPath() throws Exception {
        ConfigMojo mojo = mojo();
        jar(directory.resolve("build-output/orders-release.jar"));
        File associated = jar(directory.resolve("custom-name.jar"));
        mojo.project.getArtifact().setFile(associated);
        assertEquals(associated, mojo.resolveJarFile());
    }

    @Test
    void missingExplicitOrAssociatedArtifactDoesNotFallBackToStaleJar() throws Exception {
        ConfigMojo mojo = mojo();
        jar(directory.resolve("build-output/orders-release.jar"));
        File missing = directory.resolve("missing.jar").toFile();
        mojo.jarFile = missing;
        assertThrows(MojoExecutionException.class, mojo::resolveJarFile);

        mojo.jarFile = null;
        mojo.project.getArtifact().setFile(missing);
        assertThrows(MojoExecutionException.class, mojo::resolveJarFile);
    }

    @Test
    void directoriesAreNotJarFiles() throws Exception {
        ConfigMojo mojo = mojo();
        mojo.jarFile = directory.toFile();
        assertThrows(MojoExecutionException.class, mojo::resolveJarFile);
        mojo.jarFile = null;
        mojo.project.getArtifact().setFile(directory.toFile());
        assertThrows(MojoExecutionException.class, mojo::resolveJarFile);
        mojo.project.getArtifact().setFile(null);
        Files.createDirectories(directory.resolve("build-output/orders-release.jar"));
        assertThrows(MojoExecutionException.class, mojo::resolveJarFile);
    }

    @Test
    void doesNotSearchForOtherOrClassifiedJars() throws Exception {
        ConfigMojo mojo = mojo();
        jar(directory.resolve("build-output/orders-release-tests.jar"));
        jar(directory.resolve("build-output/unrelated.jar"));
        jar(directory.resolve("build-output/orders-1.0.jar"));
        assertThrows(MojoExecutionException.class, mojo::resolveJarFile);
    }

    @Test
    void classifierAndNonJarExtensionDisableFallback() throws Exception {
        ConfigMojo mojo = mojo();
        jar(directory.resolve("build-output/orders-release.jar"));
        mojo.project.setArtifact(new DefaultArtifact("g", "orders", "1.0", "compile",
                "jar", "tests", new DefaultArtifactHandler("jar")));
        assertThrows(MojoExecutionException.class, mojo::resolveJarFile);

        DefaultArtifactHandler handler = new DefaultArtifactHandler("jar");
        handler.setExtension("war");
        mojo.project.setArtifact(new DefaultArtifact("g", "orders", "1.0", "compile",
                "jar", null, handler));
        assertThrows(MojoExecutionException.class, mojo::resolveJarFile);
    }

    @Test
    void handlerClassifierDisablesFallback() throws Exception {
        ConfigMojo mojo = mojo();
        jar(directory.resolve("build-output/orders-release.jar"));
        DefaultArtifactHandler handler = new DefaultArtifactHandler("jar") {
            @Override
            public String getClassifier() {
                return "tests";
            }
        };
        mojo.project.setArtifact(new DefaultArtifact("g", "orders", "1.0", "compile",
                "jar", null, handler));
        assertThrows(MojoExecutionException.class, mojo::resolveJarFile);
    }

    @Test
    void unsupportedPackagingAndMismatchedArtifactTypeDisableFallback() throws Exception {
        ConfigMojo mojo = mojo();
        jar(directory.resolve("build-output/orders-release.jar"));
        for (String packaging : List.of("pom", "war", "bundle", "maven-plugin")) {
            mojo.project.setPackaging(packaging);
            assertThrows(MojoExecutionException.class, mojo::resolveJarFile, packaging);
        }
    }

    @Test
    void mavenPluginPackagingWithJarHandlerSupportsFallback() throws Exception {
        ConfigMojo mojo = mojo();
        File jar = jar(directory.resolve("build-output/orders-release.jar"));
        mojo.project.setPackaging("maven-plugin");
        DefaultArtifactHandler handler = new DefaultArtifactHandler("maven-plugin");
        handler.setExtension("jar");
        mojo.project.setArtifact(new DefaultArtifact("g", "orders", "1.0", "compile",
                "maven-plugin", null, handler));
        assertEquals(jar, mojo.resolveJarFile());
    }

    @Test
    void jarPluginOutputOverridesRequireExplicitJarFile() throws Exception {
        ConfigMojo mojo = mojo();
        File conventional = jar(directory.resolve("build-output/orders-release.jar"));
        for (String parameter : List.of("classifier", "finalName", "outputDirectory")) {
            Plugin plugin = jarPlugin(parameter, "custom");
            mojo.project.getBuild().setPlugins(List.of(plugin));
            assertThrows(MojoExecutionException.class, mojo::resolveJarFile, parameter);
            mojo.jarFile = conventional;
            assertEquals(conventional, mojo.resolveJarFile());
            mojo.jarFile = null;
        }
    }

    @Test
    void defaultJarExecutionOutputOverridesDisableFallback() throws Exception {
        ConfigMojo mojo = mojo();
        jar(directory.resolve("build-output/orders-release.jar"));
        Plugin plugin = jarPlugin("classifier", "thin");
        PluginExecution execution = new PluginExecution();
        execution.setId("default-jar");
        execution.setConfiguration(plugin.getConfiguration());
        plugin.setConfiguration(null);
        plugin.addExecution(execution);
        mojo.project.getBuild().setPlugins(List.of(plugin));
        assertThrows(MojoExecutionException.class, mojo::resolveJarFile);
    }

    @Test
    void matchingJarPluginOutputMetadataDoesNotDisableFallback() throws Exception {
        ConfigMojo mojo = mojo();
        File jar = jar(directory.resolve("build-output/orders-release.jar"));
        mojo.project.getBuild().setPlugins(List.of(
                jarPlugin("finalName", mojo.project.getBuild().getFinalName())));
        assertEquals(jar, mojo.resolveJarFile());
        mojo.project.getBuild().setPlugins(List.of(
                jarPlugin("outputDirectory", mojo.project.getBuild().getDirectory())));
        assertEquals(jar, mojo.resolveJarFile());
    }

    private ConfigMojo mojo() {
        MavenProject project = new MavenProject();
        project.setFile(directory.resolve("pom.xml").toFile());
        project.setPackaging("jar");
        project.setVersion("1.0");
        project.setArtifact(new DefaultArtifact("g", "orders", "1.0", "compile",
                "jar", null, new DefaultArtifactHandler("jar")));
        Build build = new Build();
        build.setDirectory(directory.resolve("build-output").toString());
        build.setFinalName("orders-release");
        project.setBuild(build);
        ConfigMojo mojo = new ConfigMojo();
        mojo.project = project;
        mojo.outputDirectory = directory.resolve("brewlet").toFile();
        return mojo;
    }

    private static Plugin jarPlugin(String parameter, String value) {
        Plugin plugin = new Plugin();
        plugin.setGroupId("org.apache.maven.plugins");
        plugin.setArtifactId("maven-jar-plugin");
        Xpp3Dom configuration = new Xpp3Dom("configuration");
        Xpp3Dom child = new Xpp3Dom(parameter);
        child.setValue(value);
        configuration.addChild(child);
        plugin.setConfiguration(configuration);
        return plugin;
    }

    private static File jar(Path path) throws IOException {
        Files.createDirectories(path.getParent());
        Manifest manifest = new Manifest();
        manifest.getMainAttributes().put(Attributes.Name.MANIFEST_VERSION, "1.0");
        manifest.getMainAttributes().put(Attributes.Name.MAIN_CLASS, "example.Main");
        try (JarOutputStream ignored = new JarOutputStream(Files.newOutputStream(path), manifest)) {
            return path.toFile();
        }
    }
}
