// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.ValueSource;
import org.apache.maven.execution.DefaultMavenExecutionRequest;
import org.apache.maven.execution.DefaultMavenExecutionResult;
import org.apache.maven.execution.MavenSession;
import org.apache.maven.model.Plugin;
import org.apache.maven.model.PluginExecution;
import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.project.MavenProject;
import org.apache.maven.toolchain.RequirementMatcherFactory;
import org.apache.maven.toolchain.Toolchain;
import org.apache.maven.toolchain.ToolchainManager;
import org.apache.maven.toolchain.ToolchainPrivate;
import org.apache.maven.toolchain.java.DefaultJavaToolChain;
import org.apache.maven.toolchain.model.ToolchainModel;
import org.codehaus.plexus.logging.console.ConsoleLogger;
import org.codehaus.plexus.util.xml.Xpp3Dom;
import org.codehaus.plexus.util.xml.Xpp3DomBuilder;
import sh.brewlet.maven.plugin.util.JdkVersionResolver;

import java.io.StringReader;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.List;
import java.util.Map;
import java.util.Properties;

import static org.junit.jupiter.api.Assertions.*;

/**
 * Tests JDK version inference logic.
 */
class JdkVersionResolverTest {
    @TempDir Path root;

    @Test
    void parseFeature_simpleVersion() {
        assertEquals(21, JdkVersionResolver.parseFeature("21"));
    }

    @Test
    void parseFeature_patchVersion() {
        assertEquals(21, JdkVersionResolver.parseFeature("21.0.1"));
    }

    @Test
    void parseFeature_oldStyleJava8() {
        assertEquals(8, JdkVersionResolver.parseFeature("1.8"));
    }

    @Test
    void parseFeature_oldStyleJava8WithPatch() {
        assertEquals(8, JdkVersionResolver.parseFeature("1.8.0_391"));
    }

    @Test
    void parseFeature_java17() {
        assertEquals(17, JdkVersionResolver.parseFeature("17.0.8+7"));
    }

    @Test
    void parseFeature_earlyAccessSuffix() {
        assertEquals(22, JdkVersionResolver.parseFeature("22-ea"));
    }

    @Test
    void parseFeature_nullFallsBackToDefault() {
        assertEquals(17, JdkVersionResolver.parseFeature(null));
    }

    @Test
    void parseFeature_emptyFallsBackToDefault() {
        assertEquals(17, JdkVersionResolver.parseFeature(""));
    }

    @Test
    void runningJdkFeature_returnsPositiveVersion() {
        int feature = JdkVersionResolver.runningJdkFeature();
        assertTrue(feature >= 8, "Running JDK feature should be >= 8, got " + feature);
    }

    @Test
    void literalCompilerXmlWinsOverUserPropertyButExpressionsUseMavenPrecedence() throws Exception {
        MavenProject project = project("<release>17</release>");
        project.getProperties().setProperty("maven.compiler.release", "8");
        MavenSession session = session(project, Map.of("maven.compiler.release", "21"));
        assertEquals(17, resolve(project, session, null));
        compiler(project).setConfiguration(xml("<release>${maven.compiler.release}</release>"));
        assertEquals(21, resolve(project, session, null));
        compiler(project).setConfiguration(xml(""));
        assertEquals(21, resolve(project, session, null));
    }

    @Test
    void releaseThenTargetThenSourceAcrossEffectiveValues() throws Exception {
        MavenProject project = project("<source>8</source><target>11</target>");
        project.getProperties().setProperty("maven.compiler.release", "17");
        assertEquals(17, resolve(project));
        project.getProperties().remove("maven.compiler.release");
        assertEquals(11, resolve(project));
        compiler(project).setConfiguration(xml("<source>1.8</source>"));
        assertEquals(8, resolve(project));
        project.getProperties().setProperty("maven.compiler.target", "17");
        assertEquals(17, resolve(project));
    }

    @Test
    void mainExecutionOverridesPluginWhileTestOnlySettingsAreIgnored() throws Exception {
        MavenProject project = project("<release>17</release>");
        compiler(project).addExecution(execution("default-testCompile", "testCompile", null, "<release>25</release>"));
        compiler(project).addExecution(execution("default-compile", "compile", null, "<release>11</release>"));
        assertEquals(11, resolve(project));
    }

    @ParameterizedTest
    @ValueSource(strings = {"none", "disabled", "never", "clean", "site"})
    void disabledDefaultCompileAndAReplacementMainExecutionAreSupported(String disabledPhase) throws Exception {
        MavenProject project = project("<release>8</release>");
        compiler(project).addExecution(execution("default-compile", "compile", disabledPhase, "<release>8</release>"));
        compiler(project).addExecution(execution("main", "compile", "compile", "<release>21</release>"));
        compiler(project).addExecution(execution("only-tests", "testCompile", "test-compile", "<release>25</release>"));
        compiler(project).addExecution(execution("only-clean", "compile", "clean", "<release>11</release>"));
        compiler(project).addExecution(execution("only-site", "compile", "site", "<release>11</release>"));
        var mappings = mappings(project);
        assertEquals(List.of("main"), mappings.values().stream().flatMap(List::stream)
                .filter(mojo -> "compile".equals(mojo.getGoal())).map(mojo -> mojo.getExecutionId()).toList());
        assertEquals(21, resolve(project));
    }

    @Test
    void conflictingMainExecutionsFailRatherThanPickingAnExtremum() throws Exception {
        MavenProject project = project("<release>17</release>");
        compiler(project).addExecution(execution("other-main", "compile", "compile", "<release>21</release>"));
        assertGuidance(project, session(project, Map.of()), null);
        compiler(project).getExecutions().get(0).setConfiguration(xml("<release>17</release>"));
        assertEquals(17, resolve(project));
    }

    @Test
    void effectivePluginManagementCanConfigureImplicitMainCompilation() throws Exception {
        MavenProject project = project("<release>11</release>");
        var management = new org.apache.maven.model.PluginManagement();
        management.addPlugin(compiler(project));
        project.getBuild().setPlugins(List.of());
        project.getBuild().setPluginManagement(management);
        assertEquals(11, resolve(project));
    }

    @Test
    void unresolvedExpressionsInXmlAndPropertiesFail() throws Exception {
        MavenProject project = project("<release>${not.defined}</release><target>17</target>");
        assertGuidance(project, session(project, Map.of()), null);
        compiler(project).setConfiguration(xml("<target>17</target>"));
        project.getProperties().setProperty("maven.compiler.release", "${not.defined}");
        assertGuidance(project, session(project, Map.of()), null);
    }

    @Test
    void duplicateCompilationLevelElementsAreRejected() throws Exception {
        MavenProject project = project("<release>17</release><release>21</release>");
        assertGuidance(project, session(project, Map.of()), null);
    }

    @ParameterizedTest
    @ValueSource(strings = {
            "<compilerArgs><arg>--release</arg><arg>21</arg></compilerArgs>",
            "<compilerArgument>-target 21</compilerArgument>",
            "<compilerArguments><source>21</source></compilerArguments>"})
    void opaqueLevelArgumentsCannotOverrideTheInferredLevelSilently(String arguments) throws Exception {
        MavenProject project = project("<release>17</release>" + arguments);
        assertGuidance(project, session(project, Map.of()), null);
    }

    @Test
    void unrelatedCompilerArgumentsDoNotAffectInference() throws Exception {
        MavenProject project = project("<release>17</release><compilerArgs>"
                + "<arg>-Xlint:all</arg><arg>-Aprocessor.release=21</arg></compilerArgs>");
        assertEquals(17, resolve(project));
    }

    @ParameterizedTest
    @ValueSource(strings = {"", "banana", "0", "-1", "[17,22)", "17-ea", "17.0.1", "99999999999999999"})
    void malformedConfiguredCompilationLevelsNeverFallBack(String value) throws Exception {
        MavenProject project = project("<target>21</target>");
        project.getProperties().setProperty("maven.compiler.release", value);
        assertGuidance(project, session(project, Map.of()), null);
    }

    @Test
    void ordinaryMavenRuntimeIsOnlyTheLastFallback() throws Exception {
        MavenProject project = project("");
        assertEquals(Runtime.version().feature(), resolve(project));
    }

    @Test
    void declaredReleaseWinsWithoutResolvingAnyToolchain() throws Exception {
        MavenProject project = project("<release>17</release><jdkToolchain><version>[21,)</version></jdkToolchain>");
        assertEquals(17, resolve(project));
    }

    @Test
    void compilerSpecificToolchainBeatsSessionSelectionAndRangesFollowMavenOrder() throws Exception {
        MavenProject project = project("<jdkToolchain><version>[17,22)</version></jdkToolchain>");
        Toolchain seventeen = chain("17.0.16", "17.0.16+1");
        Toolchain twentyOne = chain("21", "21.0.8");
        MavenSession session = session(project, Map.of());
        assertEquals(21, resolve(project, session, manager(seventeen, List.of(twentyOne, seventeen))));
        assertEquals(17, resolve(project, session, manager(twentyOne, List.of(seventeen, twentyOne))));
        compiler(project).setConfiguration(xml("<jdkToolchain><version>21</version><vendor>fixture</vendor></jdkToolchain>"));
        assertEquals(21, resolve(project, session, manager(seventeen, List.of(seventeen, twentyOne))));
    }

    @Test
    void unmatchedCompilerRequirementsUseAnActualSessionSelectionButNeverGuessTheBuildJvm() throws Exception {
        MavenProject project = project("<jdkToolchain><version>[25,)</version></jdkToolchain>");
        Toolchain seventeen = chain("17", "17.0.16");
        MavenSession session = session(project, Map.of());
        assertEquals(17, resolve(project, session, manager(seventeen, List.of(seventeen))));
        assertGuidance(project, session, manager(null, List.of(seventeen)));
        assertGuidance(project, session, null);
    }

    @Test
    void testCompilerToolchainDoesNotChangeMainSessionSelection() throws Exception {
        MavenProject project = project("");
        compiler(project).addExecution(execution("default-testCompile", "testCompile", null,
                "<release>25</release><jdkToolchain><version>25</version></jdkToolchain>"));
        assertEquals(17, resolve(project, session(project, Map.of()), manager(chain("17", "17.0.16"), List.of())));
    }

    @Test
    void standaloneManifestCanResolveConfiguredLegacyToolchainsRequirements() throws Exception {
        MavenProject project = project("");
        Plugin toolchains = toolchains(project);
        toolchains.setConfiguration(xml("<toolchains><jdk><version>${compiler.jdk}</version>"
                + "<vendor>fixture</vendor></jdk></toolchains>"));
        toolchains.addExecution(execution("select-main", "toolchain", "validate", ""));
        project.getProperties().setProperty("compiler.jdk", "[17,22)");
        var available = List.<Toolchain>of(chain("17", "17.0.16"), chain("21", "21.0.8"));
        assertEquals(17, resolve(project, session(project, Map.of()), manager(null, available)));
        assertEquals(21, resolve(project, session(project, Map.of("compiler.jdk", "21")), manager(null, available)));
        assertGuidance(project, session(project, Map.of()), manager(null, List.of()));
    }

    @Test
    void sessionSelectionPrecedesRequirementsThatHaveNotSelectedAnythingInThisInvocation() throws Exception {
        MavenProject project = project("");
        toolchains(project).addExecution(execution("select-main", "toolchain", null,
                "<toolchains><jdk><version>21</version></jdk></toolchains>"));
        assertEquals(17, resolve(project, session(project, Map.of()), manager(chain("17", "17"), List.of())));
    }

    @Test
    void automaticDiscoveryRequiresAConcreteSessionSelection() throws Exception {
        MavenProject project = project("");
        toolchains(project).addExecution(execution("select", "select-jdk-toolchain", "validate", "<version>[17,)</version>"));
        assertGuidance(project, session(project, Map.of()), manager(null, List.of()));
        assertEquals(21, resolve(project, session(project, Map.of()), manager(chain("21", "21-ea+8"), List.of())));
    }

    @Test
    void lateToolchainSelectionCannotBeMistakenForTheMainCompiler() throws Exception {
        MavenProject project = project("");
        toolchains(project).addExecution(execution("select-test", "toolchain", "test-compile",
                "<toolchains><jdk><version>21</version></jdk></toolchains>"));
        assertGuidance(project, session(project, Map.of()), manager(chain("21", "21"), List.of()));
        compiler(project).setConfiguration(xml("<release>17</release>"));
        assertEquals(17, resolve(project));
    }

    @Test
    void compilePhaseToolchainAfterDefaultCompileCannotMasqueradeAsTheMainJdk() throws Exception {
        MavenProject project = project("");
        Plugin compiler = compiler(project);
        PluginExecution mainCompile = execution("default-compile", "compile", "compile", "");
        mainCompile.setPriority(-1); // Maven's implicit lifecycle binding precedes user executions.
        compiler.addExecution(mainCompile);
        Plugin toolchains = toolchains(project);
        toolchains.addExecution(execution("main-jdk", "toolchain", "validate",
                "<toolchains><jdk><version>21</version></jdk></toolchains>"));
        toolchains.addExecution(execution("test-jdk", "toolchain", "compile",
                "<toolchains><jdk><version>17</version></jdk></toolchains>"));
        project.getBuild().setPlugins(List.of(toolchains, compiler));
        var mappings = mappings(project);
        assertEquals(List.of("main-jdk"), mappings.get("validate").stream().map(mojo -> mojo.getExecutionId()).toList());
        assertEquals(List.of("default-compile", "test-jdk"),
                mappings.get("compile").stream().map(mojo -> mojo.getExecutionId()).toList());
        MavenSession session = session(project, Map.of());
        ToolchainManager manager = manager(chain("17", "17"), List.of(chain("21", "21"), chain("17", "17")));
        assertGuidance(project, session, manager);
        ManifestMojo explicit = new ManifestMojo();
        explicit.project = project;
        explicit.session = session;
        explicit.toolchainManager = manager;
        explicit.jdkFeature = 21;
        assertEquals(21, explicit.resolveJdkFeature());
    }

    @ParameterizedTest
    @ValueSource(strings = {"validate", "initialize", "process-resources"})
    void samePhaseSelectionIsAlsoAmbiguousForEarlierMainCompilation(String phase) throws Exception {
        MavenProject project = project("");
        compiler(project).addExecution(execution("default-compile", "compile", phase, ""));
        toolchains(project).addExecution(execution("select", "toolchain", phase,
                "<toolchains><jdk><version>21</version></jdk></toolchains>"));
        assertGuidance(project, session(project, Map.of()), manager(chain("21", "21"), List.of()));
    }

    @ParameterizedTest
    @ValueSource(strings = {"none", "disabled", "never"})
    void unboundToolchainExecutionsDoNotInvalidateTheMainSelection(String phase) throws Exception {
        MavenProject project = project("");
        Plugin toolchains = toolchains(project);
        toolchains.addExecution(execution("main-jdk", "toolchain", "validate",
                "<toolchains><jdk><version>21</version></jdk></toolchains>"));
        toolchains.addExecution(execution("disabled-jdk", "toolchain", phase,
                "<toolchains><jdk><version>17</version></jdk></toolchains>"));
        assertEquals(21, resolve(project, session(project, Map.of()), manager(chain("21", "21"), List.of())));
    }

    private static Map<String, List<org.apache.maven.plugin.MojoExecution>> mappings(MavenProject project) throws Exception {
        var lifecycle = new org.apache.maven.lifecycle.Lifecycle("default", List.of(
                "validate", "initialize", "generate-sources", "process-sources", "generate-resources",
                "process-resources", "compile", "process-classes", "generate-test-sources", "process-test-sources",
                "generate-test-resources", "process-test-resources", "test-compile", "process-test-classes",
                "test", "prepare-package", "package", "pre-integration-test", "integration-test",
                "post-integration-test", "verify", "install", "deploy"), Map.of());
        return new org.apache.maven.lifecycle.internal.DefaultLifecycleMappingDelegate()
                .calculateLifecycleMappings(session(project, Map.of()), project, lifecycle, "package");
    }

    @ParameterizedTest
    @ValueSource(strings = {"<executable>/custom/javac</executable>", "<fork>true</fork>", "<compilerId>custom</compilerId>"})
    void opaqueCompilerSelectionWithoutAnExplicitLevelFails(String config) throws Exception {
        MavenProject project = project(config);
        assertGuidance(project, session(project, Map.of()), null);
        compiler(project).setConfiguration(xml(config + "<release>17</release>"));
        assertEquals(17, resolve(project));
    }

    @Test
    void inconsistentToolchainVersionAndJdkReleaseMetadataFail() throws Exception {
        MavenProject project = project("");
        assertGuidance(project, session(project, Map.of()), manager(chain("17", "21.0.8"), List.of()));
        assertGuidance(project, session(project, Map.of()), manager(chain("17", "${unknown}"), List.of()));
    }

    private Toolchain chain(String provided, String actual) throws Exception {
        Path home = Files.createDirectory(root.resolve("jdk-" + java.util.UUID.randomUUID()));
        Files.writeString(home.resolve("release"), "JAVA_VERSION=\"" + actual + "\"\n");
        ToolchainModel model = new ToolchainModel();
        model.setType("jdk");
        model.addProvide("version", provided);
        model.addProvide("vendor", "fixture");
        DefaultJavaToolChain chain = new DefaultJavaToolChain(model, new ConsoleLogger());
        chain.setJavaHome(home.toString());
        chain.addProvideToken("version", RequirementMatcherFactory.createVersionMatcher(provided));
        chain.addProvideToken("vendor", RequirementMatcherFactory.createExactMatcher("fixture"));
        return chain;
    }

    private static ToolchainManager manager(Toolchain selected, List<Toolchain> configured) {
        return new ToolchainManager() {
            @Override public Toolchain getToolchainFromBuildContext(String type, MavenSession session) { return selected; }
            @Override public List<Toolchain> getToolchains(MavenSession session, String type, Map<String, String> requirements) {
                return configured.stream().filter(chain -> ((ToolchainPrivate) chain).matchesRequirements(requirements)).toList();
            }
        };
    }

    private static MavenProject project(String configuration) throws Exception {
        MavenProject project = new MavenProject();
        Plugin plugin = new Plugin();
        plugin.setGroupId("org.apache.maven.plugins");
        plugin.setArtifactId("maven-compiler-plugin");
        plugin.setConfiguration(xml(configuration));
        project.getBuild().addPlugin(plugin);
        return project;
    }

    private static Plugin compiler(MavenProject project) { return project.getBuildPlugins().get(0); }

    private static Plugin toolchains(MavenProject project) {
        Plugin plugin = new Plugin();
        plugin.setGroupId("org.apache.maven.plugins");
        plugin.setArtifactId("maven-toolchains-plugin");
        project.getBuild().addPlugin(plugin);
        return plugin;
    }

    private static PluginExecution execution(String id, String goal, String phase, String configuration) throws Exception {
        PluginExecution execution = new PluginExecution();
        execution.setId(id);
        execution.addGoal(goal);
        execution.setPhase(phase);
        execution.setConfiguration(xml(configuration));
        return execution;
    }

    private static Xpp3Dom xml(String contents) throws Exception {
        return Xpp3DomBuilder.build(new StringReader("<configuration>" + contents + "</configuration>"));
    }

    private static MavenSession session(MavenProject project, Map<String, String> userProperties) {
        var request = new DefaultMavenExecutionRequest();
        Properties properties = new Properties();
        properties.putAll(userProperties);
        request.setUserProperties(properties);
        request.setSystemProperties(new Properties());
        var session = new MavenSession(null, (org.eclipse.aether.RepositorySystemSession) null,
                request, new DefaultMavenExecutionResult());
        session.setCurrentProject(project);
        return session;
    }

    private static int resolve(MavenProject project) throws Exception {
        return resolve(project, session(project, Map.of()), null);
    }

    private static int resolve(MavenProject project, MavenSession session, ToolchainManager manager) throws Exception {
        return JdkVersionResolver.resolve(project, session, manager, null);
    }

    private static void assertGuidance(MavenProject project, MavenSession session, ToolchainManager manager) {
        MojoExecutionException failure = assertThrows(MojoExecutionException.class,
                () -> resolve(project, session, manager));
        assertTrue(failure.getMessage().contains("brewlet.jdkFeature"), failure.getMessage());
    }
}
