package sh.brewlet.maven;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.SerializationFeature;
import java.io.IOException;
import java.net.URLEncoder;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.LinkOption;
import java.nio.file.Path;
import java.nio.file.StandardCopyOption;
import java.nio.file.attribute.FileTime;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.HashSet;
import java.util.HexFormat;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Set;
import org.apache.maven.artifact.Artifact;
import org.apache.maven.model.Dependency;

/** Maven-specific graph data is an ordinary locked file, never a core wire extension. */
final class MavenGraph {
    static final ObjectMapper JSON = new ObjectMapper()
            .enable(SerializationFeature.ORDER_MAP_ENTRIES_BY_KEYS);
    static final String METADATA_PATH = "/app/.brewlet/maven-lock.json";

    record Member(String groupId, String artifactId, String version, String type,
                  String classifier, String scope, Path file) {
        String key() {
            return groupId + ":" + artifactId + ":" + type + ":" + classifier;
        }
    }

    private final String sourceBom;
    private final List<Member> members;
    private final String target;

    MavenGraph(String sourceBom, List<Member> members, String target) throws IOException {
        if (sourceBom == null || !sourceBom.matches("[^:\\s]+:[^:\\s]+:[^:\\s]+")) {
            throw new IOException("brewlet.sourceBom must be explicit groupId:artifactId:version");
        }
        if (!List.of("/app/lib", "/app/mods").contains(target)) {
            throw new IOException("Java dependency target must be /app/lib or /app/mods");
        }
        this.sourceBom = sourceBom;
        this.target = target;
        this.members = members.stream().sorted(Comparator.comparing(
                member -> member.key() + ":" + member.version())).toList();
        Set<String> keys = new HashSet<>();
        Set<String> filenames = new HashSet<>();
        for (Member member : this.members) {
            if (!List.of("compile", "runtime").contains(member.scope())) {
                throw new IOException("Not a runtime-scoped dependency: " + member.key());
            }
            if (!keys.add(member.key())) {
                throw new IOException("Duplicate Maven coordinates: " + member.key());
            }
            if (!Files.isRegularFile(member.file(), LinkOption.NOFOLLOW_LINKS)
                    || !member.file().getFileName().toString().endsWith(".jar")) {
                throw new IOException("Expected a resolved non-symlink dependency JAR: " + member.file());
            }
            if (!filenames.add(member.file().getFileName().toString())) {
                throw new IOException("Duplicate dependency filename: " + member.file().getFileName());
            }
        }
    }

    static List<Member> runtimeMembers(Set<Artifact> artifacts) throws IOException {
        List<Member> result = new ArrayList<>();
        for (Artifact artifact : artifacts) {
            if (!List.of("compile", "runtime").contains(artifact.getScope())) {
                continue;
            }
            if (artifact.getFile() == null) {
                throw new IOException("Unresolved runtime dependency: " + artifact);
            }
            result.add(new Member(artifact.getGroupId(), artifact.getArtifactId(),
                    artifact.getVersion(), artifact.getType(),
                    artifact.getClassifier() == null ? "" : artifact.getClassifier(),
                    artifact.getScope(), artifact.getFile().toPath()));
        }
        return result;
    }

    void requireBomMembership(List<Dependency> managed) throws IOException {
        for (Member member : members) {
            boolean found = managed.stream().anyMatch(dependency ->
                    member.groupId().equals(dependency.getGroupId())
                    && member.artifactId().equals(dependency.getArtifactId())
                    && member.version().equals(dependency.getVersion())
                    && member.type().equals(dependency.getType() == null ? "jar" : dependency.getType())
                    && member.classifier().equals(dependency.getClassifier() == null
                            ? "" : dependency.getClassifier()));
            if (!found) {
                throw new IOException("Runtime dependency is not an exact member of source BOM "
                        + sourceBom + ": " + member.key() + ":" + member.version());
            }
        }
    }

    byte[] canonicalBytes() throws IOException {
        List<Map<String, Object>> artifacts = new ArrayList<>();
        for (Member member : members) {
            Map<String, Object> entry = new LinkedHashMap<>();
            entry.put("groupId", member.groupId());
            entry.put("artifactId", member.artifactId());
            entry.put("version", member.version());
            entry.put("type", member.type());
            if (!member.classifier().isEmpty()) {
                entry.put("classifier", member.classifier());
            }
            entry.put("scope", member.scope());
            entry.put("path", path(member));
            entry.put("sha256", sha256(Files.readAllBytes(member.file())));
            artifacts.add(entry);
        }
        return JSON.writeValueAsBytes(Map.of("schemaVersion", 1, "sourceBom", sourceBom,
                "artifacts", artifacts));
    }

    List<Map<String, Object>> neutralEntries() throws IOException {
        List<Map<String, Object>> entries = new ArrayList<>();
        for (Member member : members) {
            entries.add(Map.of("ecosystem", "java", "name", member.artifactId(),
                    "group", member.groupId(), "version", member.version(),
                    "purl", purl(member), "path", path(member),
                    "sha256", sha256(Files.readAllBytes(member.file()))));
        }
        entries.add(Map.of("ecosystem", "java", "name", "maven-lock", "version", "1",
                "path", METADATA_PATH, "sha256", sha256(canonicalBytes())));
        entries.sort(Comparator.comparing(entry -> entry.get("path").toString()));
        return entries;
    }

    Path stage(Path root) throws IOException {
        Path directory = Files.createDirectories(root.resolve(target.substring("/app/".length())));
        for (Member member : members) {
            Files.copy(member.file(), directory.resolve(member.file().getFileName()),
                    StandardCopyOption.COPY_ATTRIBUTES);
        }
        Path metadata = Files.createDirectories(root.resolve(".brewlet"));
        Path lock = metadata.resolve("maven-lock.json");
        Files.write(lock, canonicalBytes());
        Files.setLastModifiedTime(lock, FileTime.fromMillis(0));
        Files.setLastModifiedTime(metadata, FileTime.fromMillis(0));
        Files.setLastModifiedTime(directory, FileTime.fromMillis(0));
        Files.setLastModifiedTime(root, FileTime.fromMillis(0));
        return root;
    }

    private String path(Member member) {
        return target + "/" + member.file().getFileName();
    }

    static String purl(Member member) {
        String qualifiers = "";
        if (!member.classifier().isEmpty()) {
            qualifiers = "classifier=" + encode(member.classifier());
        }
        if (!"jar".equals(member.type())) {
            qualifiers += (qualifiers.isEmpty() ? "" : "&") + "type=" + encode(member.type());
        }
        return "pkg:maven/" + encode(member.groupId()) + "/" + encode(member.artifactId())
                + "@" + encode(member.version()) + (qualifiers.isEmpty() ? "" : "?" + qualifiers);
    }

    private static String encode(String value) {
        return URLEncoder.encode(value, StandardCharsets.UTF_8).replace("+", "%20");
    }

    static String sha256(byte[] bytes) {
        try {
            return HexFormat.of().formatHex(MessageDigest.getInstance("SHA-256").digest(bytes));
        } catch (NoSuchAlgorithmException impossible) {
            throw new IllegalStateException(impossible);
        }
    }
}
