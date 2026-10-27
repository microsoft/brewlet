import java.io.File;
import java.security.KeyStore;
import java.security.MessageDigest;
import java.security.cert.X509Certificate;
import java.util.ArrayList;
import java.util.Collections;
import java.util.HexFormat;
import java.util.List;
import javax.naming.ldap.LdapName;
import javax.tools.ToolProvider;

class VerifyRuntime {
    public static void main(String[] args) throws Exception {
        String version = System.getProperty("java.vm.version");
        String vendor = System.getProperty("java.vm.vendor");
        if (!version.equals("25.0.5+8-LTS") || !vendor.equals("Eclipse Adoptium")
                || ToolProvider.getSystemJavaCompiler() == null) {
            throw new IllegalStateException("Expected a complete Eclipse Temurin JDK 25.0.5+8");
        }
        var store = KeyStore.getInstance(new File(System.getProperty("java.home"), "lib/security/cacerts"), (char[]) null);
        var names = new ArrayList<String>();
        var certificates = new ArrayList<String>();
        var sha256 = MessageDigest.getInstance("SHA-256");
        var hex = HexFormat.ofDelimiter(":").withUpperCase();
        for (String alias : Collections.list(store.aliases())) {
            if (!(store.getCertificate(alias) instanceof X509Certificate cert)) continue;
            String name = alias;
            for (var rdn : new LdapName(cert.getSubjectX500Principal().getName()).getRdns()) {
                if (rdn.getType().equalsIgnoreCase("CN")) name = String.valueOf(rdn.getValue());
            }
            names.add(name);
            certificates.add("{\"name\":" + json(name) + ",\"sha256Fingerprint\":"
                    + json(hex.formatHex(sha256.digest(cert.getEncoded()))) + "}");
        }
        if (!names.contains("emSign Root CA - G1")) throw new IllegalStateException("G1 must remain present");
        for (String removed : List.of("emSign ECC Root CA - G3", "emSign Root CA - C1", "emSign ECC Root CA - C3")) {
            if (names.contains(removed)) throw new IllegalStateException("Distrusted root remains: " + removed);
        }
        System.out.println("{\"vmVersion\":" + json(version) + ",\"vmVendor\":" + json(vendor)
                + ",\"vmName\":" + json(System.getProperty("java.vm.name"))
                + ",\"jdkCompiler\":true,\"caCertificates\":[" + String.join(",", certificates) + "]}");
    }

    private static String json(String value) {
        var escaped = new StringBuilder("\"");
        for (char c : value.toCharArray()) {
            if (c == '"' || c == '\\') escaped.append('\\').append(c);
            else if (c < 32) escaped.append(String.format("\\u%04x", (int) c));
            else escaped.append(c);
        }
        return escaped.append('"').toString();
    }
}
