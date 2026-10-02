// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin.oci;

/**
 * Registry credentials: either a username + password/token pair, or an OAuth2
 * identity (refresh) token as stored by {@code docker login} for registries such
 * as Azure Container Registry.
 * The secret is held as a {@link String} and is never logged (see
 * {@link #toString()}); callers should avoid retaining it longer than needed.
 */
public class Credential {

    /**
     * Username that Docker credential helpers return when the secret is an
     * identity token rather than a password.
     */
    public static final String IDENTITY_TOKEN_USERNAME = "<token>";

    private final String username;
    private final String password;
    private final boolean identityToken;

    public Credential(String username, String password) {
        this(username, password, false);
    }

    private Credential(String username, String password, boolean identityToken) {
        this.username = username;
        this.password = password;
        this.identityToken = identityToken;
    }

    /**
     * Creates a credential from an OAuth2 identity (refresh) token. The token is
     * exchanged at the registry's token realm with
     * {@code grant_type=refresh_token} and is never sent as Basic credentials.
     */
    public static Credential identityToken(String token) {
        return new Credential(IDENTITY_TOKEN_USERNAME, token, true);
    }

    public String getUsername() { return username; }

    /** Returns the password/token. Never log this value. */
    public String getPassword() { return password; }

    /** Whether {@link #getPassword()} is an OAuth2 identity (refresh) token. */
    public boolean isIdentityToken() { return identityToken; }

    @Override
    public String toString() {
        return "Credential{username='" + username + "', ******";
    }
}
