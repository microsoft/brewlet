// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin.util;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import org.apache.maven.plugin.MojoExecutionException;
import org.apache.maven.settings.Server;
import org.apache.maven.settings.Settings;
import org.apache.maven.settings.building.SettingsProblem;
import org.apache.maven.settings.crypto.DefaultSettingsDecryptionRequest;
import org.apache.maven.settings.crypto.SettingsDecrypter;
import org.apache.maven.settings.crypto.SettingsDecryptionResult;
import sh.brewlet.maven.plugin.oci.Credential;

import java.io.ByteArrayOutputStream;
import java.io.File;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.URI;
import java.nio.charset.StandardCharsets;
import java.util.Base64;
import java.util.Iterator;
import java.util.List;
import java.util.Locale;
import java.util.concurrent.TimeUnit;
import java.util.function.Consumer;
import java.util.function.Function;
import java.util.regex.Pattern;

/**
 * Resolves OCI registry credentials using the standard Maven credential chain:
 *
 * <ol>
 *   <li>{@code settings.xml} {@code <server>} whose {@code <id>} matches the
 *       registry hostname (supports Maven password encryption).</li>
 *   <li>Docker/OCI config ({@code ~/.docker/config.json} or the path indicated
 *       by the {@code DOCKER_CONFIG} environment variable), the same way the
 *       Docker CLI reads it: a per-registry {@code credHelpers} entry, then an
 *       inline {@code auths} entry ({@code auth} or {@code identitytoken}), then
 *       the default {@code credsStore}. Credential helpers
 *       ({@code docker-credential-<name>}) are how {@code docker login} and
 *       {@code az acr login} store secrets on macOS, Windows and Docker
 *       Desktop.</li>
 *   <li>{@code BREWLET_REGISTRY_USERNAME} / {@code BREWLET_REGISTRY_PASSWORD}
 *       environment variables for CI pipelines.</li>
 * </ol>
 *
 * If no credentials are found, returns {@code null} (anonymous access).
 * Credentials are <strong>never</strong> logged.
 */
public class CredentialResolver {

    private static final ObjectMapper MAPPER = new ObjectMapper();
    private static final Pattern HELPER_NAME = Pattern.compile("[A-Za-z0-9][A-Za-z0-9._-]*");
    private static final String DOCKER_HUB_KEY = "https://index.docker.io/v1/";
    private static final List<String> DOCKER_HUB_HOSTS =
            List.of("registry-1.docker.io", "docker.io", "index.docker.io");
    static final long HELPER_TIMEOUT_SECONDS = 30;

    /** Runs a Docker credential helper's {@code get} command. */
    @FunctionalInterface
    interface HelperRunner {
        /**
         * @return the helper's stdout, or {@code null} when it has no
         *         credentials for {@code serverUrl}
         */
        String get(String helper, String serverUrl) throws IOException, InterruptedException;
    }

    private CredentialResolver() {}

    /**
     * Resolves credentials for the given registry hostname.
     *
     * @param registry registry hostname, e.g. {@code "registry.example.com"}
     * @param settings Maven settings (for {@code settings.xml} {@code <server>} lookup)
     * @param decrypter Maven's injected settings decrypter
     * @return resolved {@link Credential}, or {@code null} for anonymous access
     * @throws MojoExecutionException if the matching server cannot be decrypted
     */
    public static Credential resolve(String registry, Settings settings,
                                     SettingsDecrypter decrypter) throws MojoExecutionException {
        return resolve(registry, settings, decrypter, message -> { });
    }

    /**
     * Resolves credentials for the given registry hostname, reporting why a
     * configured credential source could not be used to {@code diagnostics}.
     * Diagnostics never contain secrets.
     */
    public static Credential resolve(String registry, Settings settings, SettingsDecrypter decrypter,
                                     Consumer<String> diagnostics) throws MojoExecutionException {
        return resolve(registry, settings, decrypter, diagnostics, System::getenv,
                CredentialResolver::runHelper);
    }

    static Credential resolve(String registry, Settings settings, SettingsDecrypter decrypter,
                              Consumer<String> diagnostics, Function<String, String> env,
                              HelperRunner helpers) throws MojoExecutionException {
        // 1. settings.xml <server>
        if (settings != null) {
            Server server = settings.getServer(registry);
            if (server != null && server.getUsername() != null) {
                if (decrypter == null) {
                    throw new MojoExecutionException("Maven SettingsDecrypter is unavailable for registry "
                            + registry + ". Run this goal through Maven.");
                }
                SettingsDecryptionResult result =
                        decrypter.decrypt(new DefaultSettingsDecryptionRequest(server));
                if (result.getProblems().stream().anyMatch(problem ->
                        problem.getSeverity() == SettingsProblem.Severity.ERROR
                                || problem.getSeverity() == SettingsProblem.Severity.FATAL)) {
                    // Maven's problem messages and causes can contain encrypted secrets.
                    throw new MojoExecutionException("Cannot decrypt settings.xml credentials for registry "
                            + registry + ". Check the server password and Maven settings-security.xml.");
                }
                Server decrypted = result.getServer();
                return new Credential(decrypted.getUsername(), decrypted.getPassword());
            }
        }

        // 2. Docker config.json
        Credential dockerCred = resolveFromDockerConfig(registry, diagnostics, env, helpers);
        if (dockerCred != null) {
            return dockerCred;
        }

        // 3. Environment variables
        String envUser = env.apply("BREWLET_REGISTRY_USERNAME");
        String envPass = env.apply("BREWLET_REGISTRY_PASSWORD");
        if (envUser != null && !envUser.isEmpty()) {
            return new Credential(envUser, envPass != null ? envPass : "");
        }

        return null; // anonymous
    }

    private static Credential resolveFromDockerConfig(String registry, Consumer<String> diagnostics,
                                                      Function<String, String> env,
                                                      HelperRunner helpers) {
        String dockerConfigDir = env.apply("DOCKER_CONFIG");
        if (dockerConfigDir == null || dockerConfigDir.isEmpty()) {
            dockerConfigDir = System.getProperty("user.home") + File.separator + ".docker";
        }
        File configFile = new File(dockerConfigDir, "config.json");
        if (!configFile.isFile()) return null;

        JsonNode root;
        try {
            root = MAPPER.readTree(configFile);
        } catch (IOException e) {
            diagnostics.accept("Ignoring unreadable Docker config " + configFile + ": "
                    + e.getClass().getSimpleName());
            return null;
        }
        if (root == null || !root.isObject()) return null;

        boolean dockerHub = DOCKER_HUB_HOSTS.contains(registry.toLowerCase(Locale.ROOT));
        String serverUrl = dockerHub ? DOCKER_HUB_KEY : registry;

        // a. Per-registry credential helper wins, exactly as in the Docker CLI.
        String helper = textField(matchingEntry(root.path("credHelpers"), registry, dockerHub));
        if (helper != null) {
            return fromHelper(helper, serverUrl, diagnostics, helpers);
        }

        // b. Inline auths entry (no credential store configured).
        JsonNode entry = matchingEntry(root.path("auths"), registry, dockerHub);
        if (entry != null) {
            String identityToken = textField(entry.path("identitytoken"));
            if (identityToken != null) {
                return Credential.identityToken(identityToken);
            }
            String auth = textField(entry.path("auth"));
            if (auth != null) {
                try {
                    String authStr = new String(Base64.getDecoder().decode(auth),
                            StandardCharsets.UTF_8);
                    int colon = authStr.indexOf(':');
                    if (colon >= 0) {
                        return new Credential(authStr.substring(0, colon),
                                authStr.substring(colon + 1));
                    }
                } catch (IllegalArgumentException e) {
                    diagnostics.accept("Ignoring malformed auth entry for " + registry
                            + " in " + configFile);
                }
            }
        }

        // c. Default credential store.
        String store = textField(root.path("credsStore"));
        if (store != null) {
            return fromHelper(store, serverUrl, diagnostics, helpers);
        }
        return null;
    }

    /**
     * Finds the entry whose key names {@code registry}. Keys may be bare
     * authorities or URLs ({@code https://host/v1/}); only the authority is
     * compared, and it must match exactly so that credentials for one registry
     * are never offered to another whose name merely contains it.
     */
    private static JsonNode matchingEntry(JsonNode map, String registry, boolean dockerHub) {
        if (map == null || !map.isObject()) return null;
        for (Iterator<String> it = map.fieldNames(); it.hasNext(); ) {
            String key = it.next();
            String authority = authority(key);
            if (authority == null) continue;
            if (authority.equals(registry.toLowerCase(Locale.ROOT))
                    || (dockerHub && DOCKER_HUB_HOSTS.contains(authority))) {
                return map.get(key);
            }
        }
        return null;
    }

    private static String authority(String key) {
        String value = key.trim();
        if (value.isEmpty()) return null;
        if (!value.contains("://")) {
            value = "https://" + value;
        }
        try {
            String authority = URI.create(value).getRawAuthority();
            return authority == null ? null : authority.toLowerCase(Locale.ROOT);
        } catch (IllegalArgumentException e) {
            return null;
        }
    }

    private static Credential fromHelper(String helper, String serverUrl,
                                         Consumer<String> diagnostics, HelperRunner helpers) {
        if (!HELPER_NAME.matcher(helper).matches()) {
            diagnostics.accept("Ignoring Docker credential helper with an invalid name: " + helper);
            return null;
        }
        String output;
        try {
            output = helpers.get(helper, serverUrl);
        } catch (IOException e) {
            diagnostics.accept("Docker credential helper docker-credential-" + helper
                    + " could not be run for " + serverUrl + ": " + e.getMessage());
            return null;
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            return null;
        }
        if (output == null || output.isBlank()) {
            return null;
        }
        try {
            JsonNode json = MAPPER.readTree(output);
            String username = textField(json.path("Username"));
            String secret = textField(json.path("Secret"));
            if (secret == null) {
                return null;
            }
            if (username == null || Credential.IDENTITY_TOKEN_USERNAME.equals(username)) {
                return Credential.identityToken(secret);
            }
            return new Credential(username, secret);
        } catch (IOException e) {
            diagnostics.accept("Docker credential helper docker-credential-" + helper
                    + " returned malformed output for " + serverUrl);
            return null;
        }
    }

    private static String textField(JsonNode node) {
        if (node == null || !node.isTextual() || node.asText().isEmpty()) return null;
        return node.asText();
    }

    /**
     * Runs {@code docker-credential-<helper> get}, writing the server URL to
     * stdin as the helper protocol requires. A non-zero exit means the helper
     * has no credentials for that server.
     */
    private static String runHelper(String helper, String serverUrl)
            throws IOException, InterruptedException {
        Process process = new ProcessBuilder("docker-credential-" + helper, "get")
                .redirectError(ProcessBuilder.Redirect.DISCARD)
                .start();
        try (OutputStream stdin = process.getOutputStream()) {
            stdin.write(serverUrl.getBytes(StandardCharsets.UTF_8));
        }
        ByteArrayOutputStream stdout = new ByteArrayOutputStream();
        Thread reader = new Thread(() -> {
            try (InputStream in = process.getInputStream()) {
                in.transferTo(stdout);
            } catch (IOException ignored) {
                // The exit status decides whether the output is usable.
            }
        }, "docker-credential-" + helper);
        reader.setDaemon(true);
        reader.start();
        if (!process.waitFor(HELPER_TIMEOUT_SECONDS, TimeUnit.SECONDS)) {
            process.destroyForcibly();
            throw new IOException("timed out after " + HELPER_TIMEOUT_SECONDS + "s");
        }
        reader.join(TimeUnit.SECONDS.toMillis(5));
        if (process.exitValue() != 0) {
            return null;
        }
        return stdout.toString(StandardCharsets.UTF_8);
    }
}
