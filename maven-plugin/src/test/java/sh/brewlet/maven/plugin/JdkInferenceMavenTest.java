// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import org.apache.maven.model.Model;
import org.apache.maven.model.io.xpp3.MavenXpp3Reader;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;
import sh.brewlet.maven.plugin.util.JdkVersionResolver;

import java.io.File;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import java.util.Properties;
import java.util.concurrent.TimeUnit;
import java.util.jar.JarEntry;
import java.util.jar.JarOutputStream;

import static org.junit.jupiter.api.Assertions.*;

/** Exercises the plugin in real Maven sessions, with private artifacts/settings/toolchains. */
class JdkInferenceMavenTest {
    @TempDir Path root;
    private Path repository;
    private Path settings;
    private Path emptySettings;
    private Path toolchains;
    private Path emptyToolchains;
    private String goal;
    private int invocation;

    @Test
    void realMavenLifecycleOrderAndDisabledBindingsAreRespected() throws Exception {
        preparePlugin();
        Path application = root.resolve("lifecycle-application");
        Files.createDirectories(application.resolve("src/main/java"));
        Files.writeString(application.resolve("src/main/java/App.java"),
                "public class App { public static void main(String[] args) {} }\n");
        int currentFeature = Runtime.version().feature();
        Files.writeString(application.resolve("pom.xml"), """
                <project xmlns="http://maven.apache.org/POM/4.0.0"><modelVersion>4.0.0</modelVersion>
                  <groupId>test.inference</groupId><artifactId>lifecycle-application</artifactId><version>1</version>
                  <properties><disabled.phase>none</disabled.phase></properties>
                  <build><plugins>
                    <plugin><artifactId>maven-compiler-plugin</artifactId><version>3.16.0</version>
                      <configuration><release>8</release></configuration>
                      <executions>
                        <execution><id>default-compile</id><phase>${disabled.phase}</phase>
                          <configuration><release>8</release></configuration></execution>
                        <execution><id>replacement-main</id><phase>compile</phase><goals><goal>compile</goal></goals>
                          <configuration><release>%d</release></configuration></execution>
                        <execution><id>default-testCompile</id><configuration><release>25</release></configuration></execution>
                      </executions>
                    </plugin>
                    <plugin><artifactId>maven-jar-plugin</artifactId><version>3.4.2</version>
                      <configuration><archive><manifest><mainClass>App</mainClass></manifest></archive></configuration>
                    </plugin>
                  </plugins></build>
                </project>
                """.formatted(currentFeature));
        for (String phase : List.of("none", "disabled", "never")) {
            String output = run(application, true, "-Ddisabled.phase=" + phase, "clean", "package", goal);
            assertFalse(output.contains(":compile (default-compile)"), output);
            assertTrue(output.contains(":compile (replacement-main)"), output);
            assertCompiledAndManifest(application, currentFeature);
        }

        Path currentHome = Path.of(System.getProperty("java.home"));
        java.util.Map<Integer, Path> homes = new java.util.HashMap<>();
        homes.put(currentFeature, currentHome);
        String other = System.getProperty("brewlet.test.otherJdk");
        if (other != null) homes.put(feature(Path.of(other)), Path.of(other));
        Path mainHome = homes.getOrDefault(21, currentHome);
        Path lateHome = homes.getOrDefault(17, currentHome);
        int mainFeature = feature(mainHome);
        writeToolchains(new ArrayList<>(new java.util.LinkedHashSet<>(List.of(mainHome, lateHome))));
        String lateExecution = """
                <execution><id>late-jdk</id><phase>compile</phase><goals><goal>toolchain</goal></goals>
                  <configuration><toolchains><jdk><version>%d</version><vendor>fixture</vendor></jdk></toolchains></configuration>
                </execution>
                """.formatted(feature(lateHome));
        String template = """
                <project xmlns="http://maven.apache.org/POM/4.0.0"><modelVersion>4.0.0</modelVersion>
                  <groupId>test.inference</groupId><artifactId>lifecycle-application</artifactId><version>1</version>
                  <build><plugins>
                    <plugin><artifactId>maven-compiler-plugin</artifactId><version>3.16.0</version></plugin>
                    <plugin><artifactId>maven-toolchains-plugin</artifactId><version>3.2.0</version><executions>
                      <execution><id>main-jdk</id><phase>validate</phase><goals><goal>toolchain</goal></goals>
                        <configuration><toolchains><jdk><version>%d</version><vendor>fixture</vendor></jdk></toolchains></configuration>
                      </execution>%s
                    </executions></plugin>
                    <plugin><artifactId>maven-jar-plugin</artifactId><version>3.4.2</version>
                      <configuration><archive><manifest><mainClass>App</mainClass></manifest></archive></configuration>
                    </plugin>
                  </plugins></build>
                </project>
                """;
        Files.writeString(application.resolve("pom.xml"), template.formatted(mainFeature, lateExecution));
        String ambiguous = run(application, false, "clean", "package", goal);
        int selectMain = ambiguous.indexOf(":toolchain (main-jdk)");
        int compileMain = ambiguous.indexOf(":compile (default-compile)");
        int selectLate = ambiguous.indexOf(":toolchain (late-jdk)");
        assertTrue(selectMain >= 0 && compileMain > selectMain && selectLate > compileMain, ambiguous);
        assertTrue(ambiguous.substring(compileMain, selectLate).contains(
                "Toolchain in maven-compiler-plugin: JDK[" + mainHome), ambiguous);
        assertTrue(ambiguous.contains("at or after main compilation"), ambiguous);
        run(application, true, "-Dbrewlet.jdkFeature=" + mainFeature, goal);
        assertManifest(application, mainFeature);
        Files.writeString(application.resolve("pom.xml"), template.formatted(mainFeature, ""));
        run(application, true, goal);
        assertManifest(application, mainFeature);
        System.out.println("Verified " + invocation + " actual Maven lifecycle invocations: disabled default bindings, "
                + "replacement compilation, main-before-late toolchain ordering, and explicit override.");
    }

    @Test
    void effectiveCompilerConfigurationAndToolchainsDriveActualManifestOutput() throws Exception {
        preparePlugin();
        Path application = root.resolve("parent/application");
        Files.createDirectories(application.resolve("src/main/java"));
        Files.writeString(application.resolve("src/main/java/App.java"),
                "public class App { public static void main(String[] args) {} }\n");
        Files.writeString(application.getParent().resolve("pom.xml"), """
                <project xmlns="http://maven.apache.org/POM/4.0.0">
                  <modelVersion>4.0.0</modelVersion>
                  <groupId>test.inference</groupId><artifactId>parent</artifactId><version>1</version><packaging>pom</packaging>
                  <build><plugins>
                    <plugin><groupId>org.apache.maven.plugins</groupId><artifactId>maven-compiler-plugin</artifactId>
                      <version>3.16.0</version><configuration><release>17</release></configuration>
                      <executions><execution><id>default-testCompile</id>
                        <configuration><release>21</release></configuration>
                      </execution></executions>
                    </plugin>
                    <plugin><groupId>org.apache.maven.plugins</groupId><artifactId>maven-jar-plugin</artifactId>
                      <version>3.4.2</version><configuration><archive><manifest><mainClass>App</mainClass></manifest></archive></configuration>
                    </plugin>
                  </plugins></build>
                  <profiles><profile><id>property-release</id>
                    <properties><application.release>17</application.release></properties>
                    <build><plugins><plugin><artifactId>maven-compiler-plugin</artifactId>
                      <configuration><release>${application.release}</release></configuration>
                    </plugin></plugins></build>
                  </profile></profiles>
                </project>
                """);
        Files.writeString(application.resolve("pom.xml"), """
                <project xmlns="http://maven.apache.org/POM/4.0.0">
                  <modelVersion>4.0.0</modelVersion>
                  <parent><groupId>test.inference</groupId><artifactId>parent</artifactId><version>1</version></parent>
                  <artifactId>application</artifactId>
                  <profiles>
                    <profile><id>execution-release</id><properties><main.release>11</main.release></properties>
                      <build><plugins><plugin><artifactId>maven-compiler-plugin</artifactId><executions>
                        <execution><id>default-compile</id><configuration><release>${main.release}</release></configuration></execution>
                      </executions></plugin></plugins></build>
                    </profile>
                    <profile><id>ambiguous-main</id><build><plugins><plugin><artifactId>maven-compiler-plugin</artifactId><executions>
                      <execution><id>other-main</id><goals><goal>compile</goal></goals><configuration><release>21</release></configuration></execution>
                    </executions></plugin></plugins></build></profile>
                  </profiles>
                </project>
                """);

        run(application, true, "-Dmaven.compiler.release=8", "clean", "package", goal);
        assertCompiledAndManifest(application, 17);
        run(application, true, "-Dmaven.compiler.release=8", goal);
        assertManifest(application, 17); // Fresh standalone session, conventional packaged-JAR resolution.
        run(application, true, "-Pproperty-release", "-Dapplication.release=11",
                "-Dmaven.compiler.release=8", "clean", "package", goal);
        assertCompiledAndManifest(application, 11);
        run(application, true, "-Pexecution-release", "-Dmain.release=8",
                "-Dmaven.compiler.release=11", "clean", "package", goal);
        assertCompiledAndManifest(application, 8);
        String ambiguous = run(application, false, "-Pambiguous-main", goal);
        assertTrue(ambiguous.contains("different application JDKs"), ambiguous);

        Path selected = root.resolve("toolchain-application");
        Files.createDirectories(selected);
        Path appJar = application.resolve("target/application-1.jar");
        writeToolchainProject(selected, "<jdkToolchain><version>${compiler.requirement}</version>"
                + "<vendor>fixture</vendor></jdkToolchain>", "");
        Path currentHome = Path.of(System.getProperty("java.home"));
        int currentFeature = feature(currentHome);
        writeToolchains(List.of(currentHome));
        run(selected, true, "-Dbrewlet.jarFile=" + appJar, "-Dcompiler.requirement=" + currentFeature, goal);
        assertManifest(selected, currentFeature);

        String other = System.getProperty("brewlet.test.otherJdk");
        if (other != null) {
            Path otherHome = Path.of(other);
            int otherFeature = feature(otherHome);
            writeToolchains(List.of(otherHome, currentHome));
            run(selected, true, "-Dbrewlet.jarFile=" + appJar, "-Dcompiler.requirement=[17,30)", goal);
            assertManifest(selected, otherFeature); // First configured match, not Maven's JDK or maximum.
            run(selected, true, "-Dbrewlet.jarFile=" + appJar, "-Dcompiler.requirement=" + currentFeature, goal);
            assertManifest(selected, currentFeature);
        }
        String noMatch = run(selected, false, "-Dbrewlet.jarFile=" + appJar,
                "-Dcompiler.requirement=[999,1000)", goal);
        assertTrue(noMatch.contains("No configured JDK"), noMatch);
        run(selected, true, "-Dbrewlet.jarFile=" + appJar, "-Dcompiler.requirement=[999,1000)",
                "-Dbrewlet.jdkFeature=17", goal);
        assertManifest(selected, 17);
        String invalidOverride = run(selected, false, "-Dbrewlet.jarFile=" + appJar,
                "-Dbrewlet.jdkFeature=0", goal);
        assertTrue(invalidOverride.contains("positive JDK feature"), invalidOverride);

        writeToolchainProject(selected, "", """
                <plugin><artifactId>maven-toolchains-plugin</artifactId><version>3.2.0</version>
                  <executions><execution><goals><goal>toolchain</goal></goals></execution></executions>
                  <configuration><toolchains><jdk><version>${compiler.requirement}</version><vendor>fixture</vendor></jdk></toolchains></configuration>
                </plugin>
                """);
        run(selected, true, "-Dbrewlet.jarFile=" + appJar, "-Dcompiler.requirement=" + currentFeature, goal);
        assertManifest(selected, currentFeature);
        String selectedInSession = run(selected, true, "-Dbrewlet.jarFile=" + appJar,
                "-Dcompiler.requirement=" + currentFeature, "validate", goal);
        assertTrue(selectedInSession.contains("session-selected JDK toolchain"), selectedInSession);
        writeToolchainProject(selected, "<jdkToolchain><version>[999,1000)</version></jdkToolchain>", """
                <plugin><artifactId>maven-toolchains-plugin</artifactId><version>3.2.0</version>
                  <executions><execution><goals><goal>toolchain</goal></goals></execution></executions>
                  <configuration><toolchains><jdk><version>${compiler.requirement}</version><vendor>fixture</vendor></jdk></toolchains></configuration>
                </plugin>
                """);
        String compilerFallback = run(selected, true, "-Dbrewlet.jarFile=" + appJar,
                "-Dcompiler.requirement=" + currentFeature, "validate", goal);
        assertTrue(compilerFallback.contains("session-selected JDK toolchain"), compilerFallback);
        assertManifest(selected, currentFeature);
        writeToolchainProject(selected, "", """
                <plugin><artifactId>maven-toolchains-plugin</artifactId><version>3.2.0</version>
                  <executions><execution><goals><goal>toolchain</goal></goals></execution></executions>
                  <configuration><toolchains><jdk><version>${compiler.requirement}</version><vendor>fixture</vendor></jdk></toolchains></configuration>
                </plugin>
                """);
        Files.writeString(toolchains, "<toolchains/>");
        String unavailable = run(selected, false, "-Dbrewlet.jarFile=" + appJar,
                "-Dcompiler.requirement=" + currentFeature, goal);
        assertTrue(unavailable.contains("No configured JDK toolchain"), unavailable);
        System.out.println("Verified " + invocation + " actual Maven invocations: inherited/profile/execution compiler "
                + "precedence, bytecode levels, standalone manifests, and configured/session toolchain selection.");
    }

    private void preparePlugin() throws Exception {
        Path module = Path.of(System.getProperty("basedir", ".")).toAbsolutePath();
        Model model;
        try (var reader = Files.newBufferedReader(module.resolve("pom.xml"))) {
            model = new MavenXpp3Reader().read(reader);
        }
        repository = root.resolve("repository");
        Path artifact = repository.resolve(model.getGroupId().replace('.', '/'))
                .resolve(model.getArtifactId()).resolve(model.getVersion());
        Files.createDirectories(artifact);
        String file = model.getArtifactId() + "-" + model.getVersion();
        Files.copy(module.resolve("pom.xml"), artifact.resolve(file + ".pom"));
        Path classes = Path.of(JdkVersionResolver.class.getProtectionDomain().getCodeSource().getLocation().toURI());
        assertTrue(Files.isRegularFile(classes.resolve("META-INF/maven/plugin.xml")),
                "Maven process-classes must generate the real plugin descriptor before this test");
        try (var jar = new JarOutputStream(Files.newOutputStream(artifact.resolve(file + ".jar")));
             var files = Files.walk(classes)) {
            for (Path path : files.filter(Files::isRegularFile).sorted().toList()) {
                jar.putNextEntry(new JarEntry(classes.relativize(path).toString().replace(File.separatorChar, '/')));
                Files.copy(path, jar);
                jar.closeEntry();
            }
        }
        goal = model.getGroupId() + ":" + model.getArtifactId() + ":" + model.getVersion() + ":manifest";
        Path cache = Path.of(System.getProperty("maven.repo.local",
                Path.of(System.getProperty("user.home"), ".m2/repository").toString()));
        settings = root.resolve("settings.xml");
        // Reuse immutable dependencies from this build as a file repository.
        // Central remains a fallback for standard lifecycle plugins not used yet.
        Files.writeString(settings, """
                <settings><profiles><profile><id>build-cache</id>
                  <repositories><repository><id>build-cache</id><url>%s</url><releases><checksumPolicy>ignore</checksumPolicy></releases></repository></repositories>
                  <pluginRepositories><pluginRepository><id>build-cache</id><url>%s</url><releases><checksumPolicy>ignore</checksumPolicy></releases></pluginRepository></pluginRepositories>
                </profile></profiles><activeProfiles><activeProfile>build-cache</activeProfile></activeProfiles></settings>
                """.formatted(cache.toUri(), cache.toUri()));
        emptySettings = root.resolve("empty-settings.xml");
        Files.writeString(emptySettings, "<settings/>");
        toolchains = root.resolve("toolchains.xml");
        emptyToolchains = root.resolve("empty-toolchains.xml");
        Files.writeString(emptyToolchains, "<toolchains/>");
        writeToolchains(List.of(Path.of(System.getProperty("java.home"))));
    }

    private void writeToolchainProject(Path project, String compilerConfiguration, String otherPlugins) throws Exception {
        Files.writeString(project.resolve("pom.xml"), """
                <project xmlns="http://maven.apache.org/POM/4.0.0"><modelVersion>4.0.0</modelVersion>
                  <groupId>test.inference</groupId><artifactId>toolchain-application</artifactId><version>1</version>
                  <properties><compiler.requirement>[17,30)</compiler.requirement></properties>
                  <build><plugins><plugin><artifactId>maven-compiler-plugin</artifactId><version>3.16.0</version>
                    <configuration>%s</configuration></plugin>%s</plugins></build>
                </project>
                """.formatted(compilerConfiguration, otherPlugins));
    }

    private void writeToolchains(List<Path> homes) throws Exception {
        StringBuilder xml = new StringBuilder("<toolchains>");
        for (Path home : homes) {
            xml.append("<toolchain><type>jdk</type><provides><version>").append(feature(home))
                    .append("</version><vendor>fixture</vendor></provides><configuration><jdkHome>")
                    .append(home.toString().replace("&", "&amp;").replace("<", "&lt;"))
                    .append("</jdkHome></configuration></toolchain>");
        }
        Files.writeString(toolchains, xml.append("</toolchains>").toString());
    }

    private static int feature(Path home) throws Exception {
        Properties release = new Properties();
        try (var in = Files.newInputStream(home.resolve("release"))) { release.load(in); }
        return JdkVersionResolver.parseFeature(release.getProperty("JAVA_VERSION").replace("\"", ""));
    }

    private String run(Path project, boolean success, String... arguments) throws Exception {
        String mavenHome = System.getProperty("maven.home");
        String maven = mavenHome == null ? "mvn" : Path.of(mavenHome, "bin", "mvn").toString();
        List<String> command = new ArrayList<>(List.of(maven,
                "-B", "-ntp", "-nsu", "--settings", settings.toString(), "--global-settings", emptySettings.toString(),
                "--toolchains", toolchains.toString(), "--global-toolchains", emptyToolchains.toString(),
                "-Dmaven.repo.local=" + repository, "-Dmaven.test.skip=true",
                "-Dbrewlet.image=example.invalid/application@sha256:" + "1".repeat(64)));
        command.addAll(List.of(arguments));
        Path log = root.resolve("maven-" + ++invocation + ".log");
        ProcessBuilder builder = new ProcessBuilder(command).directory(project.toFile())
                .redirectErrorStream(true).redirectOutput(log.toFile());
        builder.environment().put("JAVA_HOME", System.getProperty("java.home"));
        builder.environment().put("MAVEN_OPTS", "-Djava.io.tmpdir=" + root);
        Process process = builder.start();
        try {
            assertTrue(process.waitFor(120, TimeUnit.SECONDS), "Maven invocation exceeded its deadline: " + command);
            String output = Files.readString(log);
            if (success) assertEquals(0, process.exitValue(), output);
            else {
                assertNotEquals(0, process.exitValue(), output);
                assertTrue(output.contains("brewlet.jdkFeature"), output);
            }
            return output;
        } finally {
            if (process.isAlive()) {
                process.toHandle().destroyForcibly();
                assertTrue(process.waitFor(5, TimeUnit.SECONDS), "Nested Maven failed to terminate");
            }
        }
    }

    private static void assertCompiledAndManifest(Path application, int feature) throws Exception {
        byte[] bytecode = Files.readAllBytes(application.resolve("target/classes/App.class"));
        int major = Byte.toUnsignedInt(bytecode[6]) * 256 + Byte.toUnsignedInt(bytecode[7]);
        assertEquals(feature + 44, major, "Actual Maven compiler target differs from expected feature");
        assertManifest(application, feature);
    }

    private static void assertManifest(Path application, int feature) throws Exception {
        String yaml = Files.readString(application.resolve("target/brewlet/javaapplication.yaml"));
        assertTrue(yaml.contains("  jvm:\n    version: " + feature + "\n"), yaml);
        assertFalse(yaml.contains("  probes:"), "JDK inference must not reintroduce inferred probes");
    }
}
