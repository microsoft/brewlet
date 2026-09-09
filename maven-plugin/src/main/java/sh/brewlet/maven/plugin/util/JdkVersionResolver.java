// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin.util;

import org.apache.maven.execution.DefaultMavenExecutionRequest;
import org.apache.maven.execution.DefaultMavenExecutionResult;
import org.apache.maven.execution.MavenSession;
import org.apache.maven.lifecycle.DefaultLifecycles;
import org.apache.maven.model.Plugin;
import org.apache.maven.model.PluginExecution;
import org.apache.maven.plugin.MojoExecution;
import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.plugin.PluginParameterExpressionEvaluator;
import org.apache.maven.plugin.logging.Log;
import org.apache.maven.project.MavenProject;
import org.apache.maven.toolchain.Toolchain;
import org.apache.maven.toolchain.ToolchainManager;
import org.apache.maven.toolchain.ToolchainPrivate;
import org.apache.maven.toolchain.java.JavaToolchainImpl;
import org.codehaus.plexus.component.configurator.expression.ExpressionEvaluationException;
import org.codehaus.plexus.component.repository.exception.ComponentLookupException;
import org.codehaus.plexus.util.xml.Xpp3Dom;

import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.HashSet;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Properties;
import java.util.Set;
import java.util.regex.Pattern;

/** Infers the application's declared compilation level, never a test-only JDK. */
public final class JdkVersionResolver {
    private static final String COMPILER = "maven-compiler-plugin";
    private static final String TOOLCHAINS = "maven-toolchains-plugin";
    private static final String GUIDANCE = " Set <jdkFeature> or -Dbrewlet.jdkFeature=<feature> explicitly,"
            + " or configure an unambiguous main compiler <release>/<target>.";
    private static final List<String> STANDARD_APPLICATION_PHASES = List.of(
            "validate", "initialize", "generate-sources", "process-sources", "generate-resources",
            "process-resources", "compile", "process-classes", "generate-test-sources", "process-test-sources",
            "generate-test-resources", "process-test-resources", "test-compile", "process-test-classes",
            "test", "prepare-package", "package", "pre-integration-test", "integration-test",
            "post-integration-test", "verify", "install", "deploy");
    private static final Pattern JDK_VERSION = Pattern.compile(
            "[1-9][0-9]*(?:\\.[0-9]+)*(?:_[0-9]+)?"
                    + "(?:-[a-zA-Z0-9]+(?:[.-][a-zA-Z0-9]+)*)?(?:\\+[0-9]+(?:-[a-zA-Z0-9.-]+)?)?");
    private static final Pattern LEVEL_ARGUMENT = Pattern.compile(
            "(?:^|\\s)(?:--release|--?target|--?source)(?:$|[\\s=])");

    private JdkVersionResolver() {}

    public static int resolve(MavenProject project) throws MojoExecutionException {
        return resolve(project, null, null, null);
    }

    /**
     * Uses Maven's effective model (including inheritance/profile expansion),
     * parameter expression evaluator, and configured-toolchain matching APIs.
     */
    public static int resolve(MavenProject project, MavenSession session,
                              ToolchainManager manager, Log log) throws MojoExecutionException {
        MavenSession evaluationSession = sessionFor(project, session);
        LifecyclePhases lifecycle = lifecyclePhases(evaluationSession);
        Plugin compiler = plugin(project, COMPILER);
        List<PluginExecution> executions = mainExecutions(compiler, lifecycle);
        Map<Integer, List<String>> levels = new LinkedHashMap<>();
        for (PluginExecution execution : executions) {
            Xpp3Dom config = effectiveConfig(compiler, execution);
            MojoExecution mojo = new MojoExecution(compiler, "compile", execution.getId());
            mojo.setConfiguration(config);
            var evaluator = new PluginParameterExpressionEvaluator(evaluationSession, mojo);
            String basis = COMPILER + ":compile (" + execution.getId() + ")";
            rejectLevelArguments(config, evaluator, basis);
            Integer feature = null;
            String selectedBasis = null;
            for (String parameter : List.of("release", "target", "source")) {
                String value = parameter(config, parameter, "maven.compiler." + parameter, evaluator, basis, evaluationSession);
                if (value != null) {
                    if (!value.matches("[1-9][0-9]*|1\\.[1-8]")) {
                        throw cannotInfer(basis + " " + parameter + " is not a concrete compilation level: " + value);
                    }
                    feature = strictFeature(value, basis + " " + parameter);
                    selectedBasis = basis + " " + parameter + "=" + value;
                    break;
                }
            }
            if (feature == null) {
                String executable = parameter(config, "executable", "maven.compiler.executable", evaluator, basis, evaluationSession);
                String compilerId = parameter(config, "compilerId", "maven.compiler.compilerId", evaluator, basis, evaluationSession);
                if (executable != null || (compilerId != null && !"javac".equals(compilerId))) {
                    throw cannotInfer(basis + " uses a custom compiler/executable without a declared compilation level");
                }
                ToolchainSelection selection = compilerToolchain(project, evaluationSession, manager, config,
                        evaluator, basis, lifecycle, executionPhase(execution, "compile"));
                if (selection != null) {
                    feature = toolchainFeature(selection.toolchain(), selection.basis());
                    selectedBasis = selection.basis();
                } else {
                    String fork = parameter(config, "fork", "maven.compiler.fork", evaluator, basis, evaluationSession);
                    if (fork != null && !"false".equalsIgnoreCase(fork)) {
                        throw cannotInfer(basis + " forks a compiler without a known configured JDK");
                    }
                    feature = runningJdkFeature();
                    selectedBasis = basis + ": in-process Maven JDK (no declared compilation level or toolchain)";
                }
            }
            levels.computeIfAbsent(feature, ignored -> new ArrayList<>()).add(selectedBasis);
        }
        if (levels.size() != 1) {
            throw cannotInfer("Main compiler executions imply different application JDKs: " + levels);
        }
        var chosen = levels.entrySet().iterator().next();
        if (log != null) log.info("Brewlet: inferred application JDK " + chosen.getKey()
                + " from " + String.join("; ", chosen.getValue()));
        return chosen.getKey();
    }

    private static MavenSession sessionFor(MavenProject project, MavenSession session) {
        MavenSession result;
        if (session == null) {
            var request = new DefaultMavenExecutionRequest();
            request.setSystemProperties(System.getProperties());
            result = new MavenSession(null, (org.eclipse.aether.RepositorySystemSession) null,
                    request, new DefaultMavenExecutionResult());
        } else {
            result = session.clone();
        }
        result.setCurrentProject(project);
        return result;
    }

    private static Plugin plugin(MavenProject project, String artifactId) {
        for (Plugin plugin : project.getBuildPlugins()) {
            if ("org.apache.maven.plugins".equals(plugin.getGroupId()) && artifactId.equals(plugin.getArtifactId())) {
                return plugin;
            }
        }
        if (COMPILER.equals(artifactId) && project.getPluginManagement() != null) {
            for (Plugin plugin : project.getPluginManagement().getPlugins()) {
                if ("org.apache.maven.plugins".equals(plugin.getGroupId()) && artifactId.equals(plugin.getArtifactId())) {
                    return plugin;
                }
            }
        }
        Plugin empty = new Plugin();
        empty.setGroupId("org.apache.maven.plugins");
        empty.setArtifactId(artifactId);
        return empty;
    }

    private record LifecyclePhases(List<String> application, Set<String> bound) {}

    private static LifecyclePhases lifecyclePhases(MavenSession session) throws MojoExecutionException {
        if (session.getContainer() == null) {
            Set<String> bound = new HashSet<>(STANDARD_APPLICATION_PHASES);
            bound.addAll(List.of("pre-clean", "clean", "post-clean", "pre-site", "site", "post-site", "site-deploy"));
            return new LifecyclePhases(STANDARD_APPLICATION_PHASES, Set.copyOf(bound));
        }
        try {
            var definitions = session.getContainer().lookup(DefaultLifecycles.class).getLifeCycles();
            List<String> application = null;
            Set<String> bound = new HashSet<>();
            for (var definition : definitions) {
                bound.addAll(definition.getPhases());
                if ("default".equals(definition.getId())) application = List.copyOf(definition.getPhases());
            }
            if (application == null || !application.contains("compile")) {
                throw cannotInfer("The application lifecycle has no recognizable main compile phase");
            }
            return new LifecyclePhases(application, Set.copyOf(bound));
        } catch (ComponentLookupException e) {
            throw new MojoExecutionException("Cannot determine Maven application lifecycle phases." + GUIDANCE, e);
        }
    }

    private static String executionPhase(PluginExecution execution, String defaultPhase) {
        return execution.getPhase() == null ? defaultPhase : execution.getPhase();
    }

    private static List<PluginExecution> mainExecutions(Plugin compiler, LifecyclePhases lifecycle)
            throws MojoExecutionException {
        List<PluginExecution> result = new ArrayList<>();
        boolean hasDefault = false;
        for (PluginExecution execution : compiler.getExecutions()) {
            boolean defaultCompile = "default-compile".equals(execution.getId());
            hasDefault |= defaultCompile;
            if ((defaultCompile || execution.getGoals().contains("compile"))
                    && lifecycle.application().contains(executionPhase(execution, "compile"))) {
                result.add(execution);
            }
        }
        if (!hasDefault) {
            PluginExecution implicit = new PluginExecution();
            implicit.setId("default-compile");
            result.add(0, implicit);
        }
        if (result.isEmpty()) throw cannotInfer("The default main compilation is disabled and no main compile execution exists");
        return result;
    }

    private static Xpp3Dom effectiveConfig(Plugin plugin, PluginExecution execution) {
        Xpp3Dom base = plugin.getConfiguration() instanceof Xpp3Dom xml ? new Xpp3Dom(xml) : null;
        Xpp3Dom specific = execution.getConfiguration() instanceof Xpp3Dom xml ? new Xpp3Dom(xml) : null;
        Xpp3Dom merged = Xpp3Dom.mergeXpp3Dom(specific, base);
        return merged == null ? new Xpp3Dom("configuration") : merged;
    }

    private static String parameter(Xpp3Dom config, String name, String property,
                                    PluginParameterExpressionEvaluator evaluator, String basis, MavenSession session)
            throws MojoExecutionException {
        Xpp3Dom child = config.getChild(name);
        if (config.getChildren(name).length > 1) {
            throw cannotInfer("Multiple " + basis + " <" + name + "> values");
        }
        if (child != null) return evaluate(child.getValue(), evaluator, basis + " <" + name + ">", true);
        boolean configured = session.getUserProperties().containsKey(property)
                || session.getSystemProperties().containsKey(property)
                || session.getCurrentProject().getProperties().containsKey(property);
        return evaluate("${" + property + "}", evaluator, property, configured);
    }

    private static String evaluate(String raw, PluginParameterExpressionEvaluator evaluator, String basis,
                                   boolean configured) throws MojoExecutionException {
        try {
            Object evaluated = evaluator.evaluate(raw, String.class);
            if (evaluated == null && !configured) return null;
            if (!(evaluated instanceof String value) || value.isBlank() || value.contains("${")) {
                throw cannotInfer("Unresolved or empty " + basis + ": " + raw);
            }
            return value.trim();
        } catch (ExpressionEvaluationException e) {
            throw new MojoExecutionException("Cannot evaluate " + basis + "." + GUIDANCE, e);
        }
    }

    private record ToolchainSelection(Toolchain toolchain, String basis) {}

    private static void rejectLevelArguments(Xpp3Dom config, PluginParameterExpressionEvaluator evaluator,
                                             String basis) throws MojoExecutionException {
        Xpp3Dom named = config.getChild("compilerArguments");
        if (named != null) {
            for (String level : List.of("release", "target", "source")) {
                if (named.getChild(level) != null) {
                    throw cannotInfer(basis + " sets " + level + " through opaque compilerArguments");
                }
            }
        }
        for (String parameter : List.of("compilerArgs", "compilerArgument")) {
            Xpp3Dom arguments = config.getChild(parameter);
            if (arguments == null) continue;
            List<Xpp3Dom> values = new ArrayList<>(List.of(arguments.getChildren()));
            if (arguments.getValue() != null) values.add(arguments);
            for (Xpp3Dom argument : values) {
                String value = evaluate(argument.getValue(), evaluator, basis + " " + parameter, true);
                if (LEVEL_ARGUMENT.matcher(value).find()) {
                    throw cannotInfer(basis + " sets a compilation level through opaque " + parameter
                            + "; use the compiler's release/target/source parameters instead");
                }
            }
        }
    }

    private static ToolchainSelection compilerToolchain(MavenProject project, MavenSession session,
                                                        ToolchainManager manager, Xpp3Dom config,
                                                        PluginParameterExpressionEvaluator evaluator, String basis,
                                                        LifecyclePhases lifecycle, String mainPhase)
            throws MojoExecutionException {
        Xpp3Dom compilerRequirements = config.getChild("jdkToolchain");
        if (config.getChildren("jdkToolchain").length > 1) {
            throw cannotInfer("Multiple " + basis + " jdkToolchain configurations");
        }
        Map<String, String> compilerRequired = null;
        if (compilerRequirements != null) {
            compilerRequired = requirements(compilerRequirements, evaluator, basis);
            List<Toolchain> matches = configuredToolchains(session, manager, compilerRequired);
            if (!matches.isEmpty()) {
                return new ToolchainSelection(matches.get(0),
                        basis + " jdkToolchain " + compilerRequired + " (Maven first match)");
            }
        }
        Plugin toolchains = plugin(project, TOOLCHAINS);
        for (PluginExecution execution : toolchains.getExecutions()) {
            String selectionPhase = executionPhase(execution, "validate");
            if (!selectsJdk(execution) || !lifecycle.bound().contains(selectionPhase)) continue;
            int selectionOrder = lifecycle.application().indexOf(selectionPhase);
            if (selectionOrder < 0 || selectionOrder >= lifecycle.application().indexOf(mainPhase)) {
                throw cannotInfer("A toolchains-plugin execution at or after main compilation, or in another lifecycle ("
                        + execution.getId() + ", phase " + selectionPhase + "), may select a test/other JDK; "
                        + "ordering before " + basis + " (phase " + mainPhase + ") is not established");
            }
        }
        Toolchain selected = manager == null ? null : manager.getToolchainFromBuildContext("jdk", session);
        if (selected != null) return new ToolchainSelection(selected, basis + " session-selected JDK toolchain");

        Map<String, String> mainRequirements = null;
        for (PluginExecution execution : toolchains.getExecutions()) {
            if (!selectsJdk(execution)
                    || !lifecycle.application().contains(executionPhase(execution, "validate"))) continue;
            if (execution.getGoals().contains("select-jdk-toolchain")) {
                throw cannotInfer("Automatic toolchains:select-jdk-toolchain has no selected session JDK; "
                        + "standalone inference cannot reproduce host discovery");
            }
            Xpp3Dom toolchainConfig = effectiveConfig(toolchains, execution);
            Xpp3Dom types = toolchainConfig.getChild("toolchains");
            Xpp3Dom jdk = types == null ? null : types.getChild("jdk");
            if (jdk == null) continue;
            Map<String, String> required = requirements(jdk, evaluator, TOOLCHAINS + " (" + execution.getId() + ")");
            if (mainRequirements != null && !mainRequirements.equals(required)) {
                throw cannotInfer("Multiple main toolchains-plugin executions have different JDK requirements");
            }
            mainRequirements = required;
        }
        if (mainRequirements != null) {
            return matchToolchain(session, manager, mainRequirements, basis + " configured toolchains-plugin JDK");
        }
        if (compilerRequired != null) {
            throw cannotInfer("No configured JDK matches " + basis + " jdkToolchain " + compilerRequired
                    + " and no session/project toolchain is available; provide Maven toolchains.xml (or -t)");
        }
        return null;
    }

    private static boolean selectsJdk(PluginExecution execution) {
        return execution.getGoals().contains("toolchain") || execution.getGoals().contains("select-jdk-toolchain");
    }

    private static Map<String, String> requirements(Xpp3Dom config, PluginParameterExpressionEvaluator evaluator,
                                                   String basis) throws MojoExecutionException {
        Map<String, String> result = new LinkedHashMap<>();
        for (Xpp3Dom child : config.getChildren()) {
            if (child.getChildCount() > 0 || result.containsKey(child.getName())) {
                throw cannotInfer("Ambiguous " + basis + " toolchain requirement " + child.getName());
            }
            result.put(child.getName(), evaluate(child.getValue(), evaluator, basis + " " + child.getName(), true));
        }
        if (result.isEmpty()) throw cannotInfer("Empty " + basis + " toolchain requirements");
        return result;
    }

    private static ToolchainSelection matchToolchain(MavenSession session, ToolchainManager manager,
                                                      Map<String, String> requirements, String basis)
            throws MojoExecutionException {
        List<Toolchain> matches = configuredToolchains(session, manager, requirements);
        if (matches.isEmpty()) {
            throw cannotInfer("No configured JDK toolchain matches " + basis + " " + requirements
                    + "; provide Maven toolchains.xml (or -t) for standalone invocation");
        }
        // Both compiler:jdkToolchain and the legacy toolchains:toolchain goal
        // select the first matching configured toolchain, not the largest version.
        return new ToolchainSelection(matches.get(0), basis + " " + requirements + " (Maven first match)");
    }

    private static List<Toolchain> configuredToolchains(MavenSession session, ToolchainManager manager,
                                                        Map<String, String> requirements) throws MojoExecutionException {
        try {
            List<Toolchain> matches = manager == null ? null : manager.getToolchains(session, "jdk", requirements);
            return matches == null ? List.of() : matches;
        } catch (IllegalArgumentException e) {
            throw new MojoExecutionException("Cannot match configured JDK toolchains " + requirements + "." + GUIDANCE, e);
        }
    }

    private static int toolchainFeature(Toolchain toolchain, String basis) throws MojoExecutionException {
        String provided = toolchain instanceof ToolchainPrivate details
                ? details.getModel().getProvides().getProperty("version") : null;
        if (toolchain instanceof JavaToolchainImpl java && java.getJavaHome() != null) {
            Path release = Path.of(java.getJavaHome()).resolve("release");
            if (Files.isRegularFile(release)) {
                Properties properties = new Properties();
                try (var in = Files.newInputStream(release)) {
                    properties.load(in);
                } catch (IOException e) {
                    throw new MojoExecutionException("Cannot read selected JDK metadata " + release + "." + GUIDANCE, e);
                }
                String version = properties.getProperty("JAVA_VERSION");
                if (version != null && version.startsWith("\"") && version.endsWith("\"")) {
                    version = version.substring(1, version.length() - 1);
                }
                int feature = strictFeature(version, basis + " " + release);
                if (provided != null && strictFeature(provided, basis + " provided version") != feature) {
                    throw cannotInfer("Selected toolchain version " + provided + " conflicts with " + release
                            + " JAVA_VERSION=" + version);
                }
                return feature;
            }
        }
        return strictFeature(provided, basis + " concrete provided version");
    }

    private static int strictFeature(String version, String basis) throws MojoExecutionException {
        if (version == null || !JDK_VERSION.matcher(version.trim()).matches()) {
            throw cannotInfer("Unknown or non-concrete JDK version from " + basis + ": " + version);
        }
        String[] parts = version.trim().split("[._+\\-]");
        try {
            int feature = Integer.parseInt(parts[0]);
            if (feature == 1 && parts.length > 1) feature = Integer.parseInt(parts[1]);
            if (feature <= 0) throw new NumberFormatException("non-positive feature");
            return feature;
        } catch (NumberFormatException e) {
            throw cannotInfer("Invalid JDK feature from " + basis + ": " + version);
        }
    }

    private static MojoExecutionException cannotInfer(String reason) {
        return new MojoExecutionException("Cannot infer application JDK: " + reason + "." + GUIDANCE);
    }

    public static int runningJdkFeature() {
        return Runtime.version().feature();
    }

    /** General-purpose legacy helper; inference uses strict validation instead. */
    public static int parseFeature(String version) {
        try {
            return strictFeature(version, "version");
        } catch (MojoExecutionException e) {
            return 17;
        }
    }
}
