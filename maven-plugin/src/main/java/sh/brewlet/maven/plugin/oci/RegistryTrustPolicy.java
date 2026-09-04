// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin.oci;

import java.net.URI;
import java.net.URISyntaxException;
import java.util.Collection;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.Set;

/**
 * Transport and credential-forwarding policy for {@link RegistryClient}.
 *
 * <p>Registry credentials are the most sensitive input the plugin handles, and
 * a registry controls both the authentication challenge it returns and the
 * upload/pagination URLs it hands back. This policy centralizes every decision
 * that can put an {@code Authorization} header on the wire:
 *
 * <ul>
 *   <li><strong>Transport.</strong> HTTPS is required unless the registry
 *       authority is an exact loopback authority ({@code localhost},
 *       {@code 127.0.0.0/8}, {@code ::1}) or was explicitly listed as an
 *       insecure registry. Matching is exact on the whole {@code host[:port]}
 *       authority, so {@code localhost.attacker.example} and
 *       {@code 127.example.com} stay on HTTPS.</li>
 *   <li><strong>Credential scope.</strong> Credentials are attached only to
 *       requests that are same-origin with the registry. Registry-supplied URLs
 *       that point elsewhere (blob upload {@code Location}, pagination links)
 *       are still usable, but never carry credentials.</li>
 *   <li><strong>Token realms.</strong> A {@code Bearer} challenge realm must be
 *       an absolute {@code http}/{@code https} URL without userinfo, must be
 *       HTTPS unless it is an insecure-eligible authority, and only receives
 *       credentials when it is same-origin with the registry, matches a
 *       built-in well-known mapping, or was explicitly allowlisted.</li>
 * </ul>
 */
public final class RegistryTrustPolicy {

    /**
     * Well-known registries whose token realm legitimately lives on another
     * origin. Docker Hub is the only widely deployed registry that does this;
     * ACR, ECR, GHCR, Artifact Registry, and Quay all use same-origin realms.
     */
    private static final Map<String, Set<String>> BUILT_IN_TOKEN_REALMS = Map.of(
            "registry-1.docker.io", Set.of("auth.docker.io"),
            "index.docker.io", Set.of("auth.docker.io"),
            "docker.io", Set.of("auth.docker.io"),
            "registry.hub.docker.com", Set.of("auth.docker.io"));

    private static final RegistryTrustPolicy SECURE_DEFAULT =
            new RegistryTrustPolicy(Set.of(), Set.of());

    private final Set<String> insecureRegistries;
    private final Set<String> allowedTokenRealms;

    private RegistryTrustPolicy(Set<String> insecureRegistries, Set<String> allowedTokenRealms) {
        this.insecureRegistries = insecureRegistries;
        this.allowedTokenRealms = allowedTokenRealms;
    }

    /** Policy that permits plaintext and credentials only for loopback registries. */
    public static RegistryTrustPolicy secureDefault() {
        return SECURE_DEFAULT;
    }

    /**
     * Builds a policy from user-supplied configuration.
     *
     * @param insecureRegistries exact {@code host[:port]} authorities reachable over
     *                           plain HTTP; loopback authorities are always allowed
     * @param allowedTokenRealms exact {@code host[:port]} authorities allowed to
     *                           receive registry credentials for a token exchange
     * @throws IllegalArgumentException if an entry is not a bare {@code host[:port]}
     */
    public static RegistryTrustPolicy of(Collection<String> insecureRegistries,
                                         Collection<String> allowedTokenRealms) {
        return new RegistryTrustPolicy(
                normalizeAuthorities(insecureRegistries, "insecureRegistries"),
                normalizeAuthorities(allowedTokenRealms, "allowedTokenRealms"));
    }

    private static Set<String> normalizeAuthorities(Collection<String> values, String option) {
        if (values == null || values.isEmpty()) {
            return Set.of();
        }
        Set<String> normalized = new LinkedHashSet<>();
        for (String value : values) {
            if (value == null || value.isBlank()) {
                continue;
            }
            String candidate = value.trim();
            if (!isBareAuthority(candidate)) {
                throw new IllegalArgumentException(option + " entry '" + value
                        + "' must be a bare registry authority such as "
                        + "'registry.internal' or 'registry.internal:5000' "
                        + "(no scheme, path, credentials, or wildcards)");
            }
            normalized.add(normalizeAuthority(candidate));
        }
        return Set.copyOf(normalized);
    }

    /**
     * Returns whether {@code registry} is a syntactically valid registry
     * authority: a bare {@code host[:port]} with no scheme, userinfo, path, or
     * whitespace. Rejecting anything else keeps a value such as
     * {@code evil.example@real.example} from silently retargeting requests.
     */
    public static boolean isBareAuthority(String registry) {
        return parseAuthority(registry) != null;
    }

    private static URI parseAuthority(String registry) {
        if (registry == null || registry.isBlank() || registry.chars().anyMatch(
                character -> Character.isWhitespace(character))) {
            return null;
        }
        URI uri;
        try {
            uri = new URI("//" + registry);
        } catch (URISyntaxException e) {
            return null;
        }
        if (uri.getHost() == null || uri.getUserInfo() != null) {
            return null;
        }
        // The parsed host and port must reproduce the input exactly; anything
        // else (path, query, fragment, a non-numeric port, an embedded '@')
        // means the string is not a bare authority.
        if (!authority(uri.getHost(), uri.getPort()).equalsIgnoreCase(registry)) {
            return null;
        }
        return uri;
    }

    /** Lowercases the host while preserving an explicit port. */
    private static String normalizeAuthority(String registry) {
        URI uri = parseAuthority(registry);
        if (uri == null) {
            return registry.toLowerCase(Locale.ROOT);
        }
        return authority(uri.getHost(), uri.getPort());
    }

    private static String authority(String host, int port) {
        String normalizedHost = host.toLowerCase(Locale.ROOT);
        return port < 0 ? normalizedHost : normalizedHost + ":" + port;
    }

    /**
     * Returns whether the given registry authority may be contacted over plain
     * HTTP: exact loopback authorities and explicitly configured insecure
     * registries only.
     */
    public boolean allowsPlaintext(String registry) {
        URI uri = parseAuthority(registry);
        if (uri == null) {
            return false;
        }
        return isLoopbackHost(uri.getHost())
                || insecureRegistries.contains(authority(uri.getHost(), uri.getPort()));
    }

    /** Returns the URL scheme to use for the given registry authority. */
    public String scheme(String registry) {
        return allowsPlaintext(registry) ? "http" : "https";
    }

    /**
     * Returns whether {@code host} is exactly a loopback host. Unlike a prefix
     * check, {@code localhost.attacker.example}, {@code 127.example.com}, and
     * {@code notlocalhost} are not loopback.
     */
    public static boolean isLoopbackHost(String host) {
        if (host == null || host.isBlank()) {
            return false;
        }
        String candidate = host.toLowerCase(Locale.ROOT);
        if (candidate.startsWith("[") && candidate.endsWith("]")) {
            candidate = candidate.substring(1, candidate.length() - 1);
        }
        if ("localhost".equals(candidate)) {
            return true;
        }
        if (isIpv4Loopback(candidate)) {
            return true;
        }
        return "::1".equals(candidate) || "0:0:0:0:0:0:0:1".equals(candidate);
    }

    private static boolean isIpv4Loopback(String host) {
        String[] octets = host.split("\\.", -1);
        if (octets.length != 4) {
            return false;
        }
        for (String octet : octets) {
            if (octet.isEmpty() || octet.length() > 3
                    || !octet.chars().allMatch(Character::isDigit)
                    || Integer.parseInt(octet) > 255) {
                return false;
            }
        }
        return Integer.parseInt(octets[0]) == 127;
    }

    /** Returns whether two URIs share a scheme, host, and effective port. */
    public static boolean sameOrigin(URI left, URI right) {
        if (left == null || right == null
                || left.getScheme() == null || right.getScheme() == null
                || left.getHost() == null || right.getHost() == null) {
            return false;
        }
        return left.getScheme().equalsIgnoreCase(right.getScheme())
                && left.getHost().equalsIgnoreCase(right.getHost())
                && effectivePort(left) == effectivePort(right);
    }

    /** Returns the port of {@code uri}, defaulting to the scheme's well-known port. */
    public static int effectivePort(URI uri) {
        if (uri.getPort() >= 0) {
            return uri.getPort();
        }
        return "https".equalsIgnoreCase(uri.getScheme()) ? 443 : 80;
    }

    /**
     * Validates an authentication challenge realm before it is contacted at all.
     * A realm that fails this check is never requested, credentials or not.
     *
     * @param realm the raw {@code realm} value from the {@code WWW-Authenticate} header
     * @return the parsed realm URI
     * @throws IllegalArgumentException if the realm is not an absolute
     *         {@code http}/{@code https} URL, carries userinfo, or would use
     *         plaintext for an authority that is not insecure-eligible
     */
    public URI validateTokenRealm(String realm) {
        if (realm == null || realm.isBlank()) {
            throw new IllegalArgumentException(
                    "Registry authentication challenge did not provide a token realm");
        }
        URI uri;
        try {
            uri = new URI(realm.trim());
        } catch (URISyntaxException e) {
            throw new IllegalArgumentException(
                    "Registry authentication realm is not a valid URL: " + realm);
        }
        if (!uri.isAbsolute() || uri.getHost() == null || uri.getScheme() == null) {
            throw new IllegalArgumentException(
                    "Registry authentication realm must be an absolute URL: " + realm);
        }
        String scheme = uri.getScheme().toLowerCase(Locale.ROOT);
        if (!"https".equals(scheme) && !"http".equals(scheme)) {
            throw new IllegalArgumentException(
                    "Registry authentication realm must use http or https: " + realm);
        }
        if (uri.getUserInfo() != null) {
            throw new IllegalArgumentException(
                    "Registry authentication realm must not embed credentials: " + realm);
        }
        if ("http".equals(scheme) && !allowsPlaintext(authority(uri.getHost(), uri.getPort()))) {
            throw new IllegalArgumentException("Registry authentication realm " + realm
                    + " uses plaintext HTTP. Use an https realm, or add its host to "
                    + "<insecureRegistries> (-Dbrewlet.insecureRegistries) to accept it.");
        }
        return uri;
    }

    /**
     * Returns whether registry credentials may be sent to {@code realm} while
     * authenticating to {@code registryOrigin}.
     */
    public boolean allowsCredentials(URI registryOrigin, URI realm) {
        if (registryOrigin == null || realm == null || realm.getHost() == null) {
            return false;
        }
        if (sameOrigin(registryOrigin, realm)) {
            return true;
        }
        String realmAuthority = authority(realm.getHost(), realm.getPort());
        String realmHost = realm.getHost().toLowerCase(Locale.ROOT);
        if (allowedTokenRealms.contains(realmAuthority)
                || allowedTokenRealms.contains(realmHost)) {
            return true;
        }
        if (!"https".equalsIgnoreCase(realm.getScheme())) {
            return false;
        }
        Set<String> builtIn = BUILT_IN_TOKEN_REALMS.get(
                registryOrigin.getHost().toLowerCase(Locale.ROOT));
        return builtIn != null && builtIn.contains(realmHost)
                && effectivePort(realm) == 443;
    }

    /** Returns the configured insecure registry authorities. */
    public List<String> insecureRegistries() {
        return List.copyOf(insecureRegistries);
    }

    /** Returns the configured cross-origin token realm authorities. */
    public List<String> allowedTokenRealms() {
        return List.copyOf(allowedTokenRealms);
    }
}
