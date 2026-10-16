package sh.brewlet.maven;

import static org.junit.jupiter.api.Assertions.*;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.node.ArrayNode;
import com.fasterxml.jackson.databind.node.ObjectNode;
import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.attribute.FileTime;
import java.nio.file.attribute.PosixFilePermissions;
import java.util.List;
import java.util.Map;
import org.apache.maven.model.Dependency;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

class MavenGraphTest {
    @TempDir Path temp;
    private static final String BOM = "example:approved:1.0";
    private static final String DIGEST = "sha256:" + "a".repeat(64);

    private MavenGraph.Member member(String name, String scope) throws IOException {
        Path jar = temp.resolve(name + ".jar");
        Files.writeString(jar, "resolved bytes for " + name);
        return new MavenGraph.Member("example", name, "1.0", "jar", "", scope, jar);
    }

    private MavenGraph graph(MavenGraph.Member... members) throws IOException {
        return new MavenGraph(BOM, List.of(members), "/app/lib");
    }

    private ObjectNode inspected(MavenGraph graph) throws IOException {
        return MavenGraph.JSON.valueToTree(Map.of("digest", DIGEST,
                "evidence", Map.of("manifestDigest", DIGEST),
                "layerSet", Map.of("schemaVersion", 1, "ecosystem", "java"),
                "lock", Map.of("schemaVersion", 1, "entries", graph.neutralEntries())));
    }

    @Test void canonicalScopedGraphAndNeutralInventoryAreStableAndSorted() throws Exception {
        var z = member("z", "runtime");
        var a = member("a", "compile");
        MavenGraph first = graph(z, a);
        MavenGraph second = graph(a, z);
        assertArrayEquals(first.canonicalBytes(), second.canonicalBytes());
        JsonNode json = MavenGraph.JSON.readTree(first.canonicalBytes());
        assertEquals(BOM, json.path("sourceBom").textValue());
        assertEquals(1, json.path("schemaVersion").intValue());
        assertEquals("a", json.at("/artifacts/0/artifactId").textValue());
        assertEquals("compile", json.at("/artifacts/0/scope").textValue());
        assertEquals("/app/lib/a.jar", json.at("/artifacts/0/path").textValue());
        assertEquals("jar", json.at("/artifacts/0/type").textValue());
        assertFalse(json.at("/artifacts/0").has("classifier"));
        assertEquals(MavenGraph.sha256(Files.readAllBytes(a.file())),
                json.at("/artifacts/0/sha256").textValue());
        assertEquals("pkg:maven/example/a@1.0", first.neutralEntries().get(1).get("purl"));
        assertEquals(MavenGraph.sha256(first.canonicalBytes()),
                first.neutralEntries().get(0).get("sha256"));
        assertEquals("pkg:maven/g%20roup/name@1%2B2?classifier=test%20suite&type=test-jar",
                MavenGraph.purl(new MavenGraph.Member("g roup", "name", "1+2", "test-jar",
                        "test suite", "runtime", a.file())));
    }

    @Test void bomMembershipRequiresExactVersionTypeAndClassifier() throws Exception {
        var a = member("a", "runtime");
        Dependency approved = new Dependency();
        approved.setGroupId("example");
        approved.setArtifactId("a");
        approved.setVersion("1.0");
        approved.setType("jar");
        graph(a).requireBomMembership(List.of(approved));
        assertThrows(IOException.class, () -> graph(a).requireBomMembership(List.of()));
        approved.setVersion("2.0");
        assertThrows(IOException.class, () -> graph(a).requireBomMembership(List.of(approved)));
        approved.setVersion("1.0");
        approved.setType("test-jar");
        assertThrows(IOException.class, () -> graph(a).requireBomMembership(List.of(approved)));
        approved.setType("jar");
        approved.setClassifier("tests");
        assertThrows(IOException.class, () -> graph(a).requireBomMembership(List.of(approved)));
        assertThrows(IOException.class, () -> new MavenGraph(null, List.of(a), "/app/lib"));
    }

    @Test void exactGraphComparisonIncludesBomScopePathHashAndMembership() throws Exception {
        var a = member("a", "runtime");
        MavenGraph expected = graph(a);
        ObjectNode valid = inspected(expected);
        assertEquals("registry.test/team/approved@" + DIGEST,
                LayerSets.verifyAndPin("registry.test/team/approved:release", valid, expected));
        var changedScope = new MavenGraph.Member(a.groupId(), a.artifactId(), a.version(),
                a.type(), a.classifier(), "compile", a.file());
        assertThrows(IOException.class, () -> LayerSets.verifyAndPin("repo", valid, graph(changedScope)));
        assertThrows(IOException.class, () -> LayerSets.verifyAndPin("repo", valid,
                new MavenGraph("example:other:1.0", List.of(a), "/app/lib")));
        assertThrows(IOException.class, () -> LayerSets.verifyAndPin("repo", valid,
                new MavenGraph(BOM, List.of(a), "/app/mods")));
        for (var changed : List.of(
                new MavenGraph.Member(a.groupId(), a.artifactId(), a.version(),
                        "test-jar", "tests", a.scope(), a.file()),
                new MavenGraph.Member(a.groupId(), a.artifactId(), "2.0",
                        a.type(), a.classifier(), a.scope(), a.file()))) {
            assertThrows(IOException.class, () -> LayerSets.verifyAndPin("repo", valid, graph(changed)));
        }
        assertThrows(IOException.class, () -> LayerSets.verifyAndPin("repo", valid, graph()));
        var b = member("b", "runtime");
        assertThrows(IOException.class, () -> LayerSets.verifyAndPin("repo", valid, graph(a, b)));
        Files.writeString(a.file(), "substituted JAR");
        assertThrows(IOException.class, () -> LayerSets.verifyAndPin("repo", valid, expected));
    }

    @Test void metadataHashAloneCannotAuthorizeSubstitutedOrExtraJarEntries() throws Exception {
        MavenGraph graph = graph(member("a", "runtime"));
        ObjectNode valid = inspected(graph);
        ArrayNode entries = (ArrayNode) valid.at("/lock/entries");
        ((ObjectNode) entries.get(1)).put("sha256", "b".repeat(64));
        ObjectNode substituted = valid;
        assertThrows(IOException.class, () -> LayerSets.verifyAndPin("repo", substituted, graph));
        valid = inspected(graph);
        entries = (ArrayNode) valid.at("/lock/entries");
        entries.add(entries.get(1).deepCopy());
        ObjectNode duplicate = valid;
        assertThrows(IOException.class, () -> LayerSets.verifyAndPin("repo", duplicate, graph));
        valid = inspected(graph);
        entries = (ArrayNode) valid.at("/lock/entries");
        ObjectNode extra = entries.addObject();
        extra.put("ecosystem", "java").put("name", "evil").put("version", "1")
                .put("path", "/app/lib/evil.jar").put("sha256", "b".repeat(64));
        ObjectNode withExtra = valid;
        assertThrows(IOException.class, () -> LayerSets.verifyAndPin("repo", withExtra, graph));
        valid = inspected(graph);
        ((ArrayNode) valid.at("/lock/entries")).remove(1);
        ObjectNode missing = valid;
        assertThrows(IOException.class, () -> LayerSets.verifyAndPin("repo", missing, graph));
    }

    @Test void failsClosedOnMissingLockWrongEcosystemOrUnboundDescriptor() throws Exception {
        MavenGraph graph = graph(member("a", "runtime"));
        assertThrows(IOException.class, () -> LayerSets.verifyAndPin("repo", null, graph));
        assertThrows(IOException.class, () -> LayerSets.verifyAndPin("repo@sha256:" + "b".repeat(64),
                inspected(graph), graph));
        for (String mutation : List.of("lock", "ecosystem", "digest", "evidence")) {
            ObjectNode document = inspected(graph);
            switch (mutation) {
                case "lock" -> document.remove("lock");
                case "ecosystem" -> ((ObjectNode) document.path("layerSet")).put("ecosystem", "python");
                case "digest" -> document.put("digest", "sha256:bad");
                case "evidence" -> ((ObjectNode) document.path("evidence"))
                        .put("manifestDigest", "sha256:" + "b".repeat(64));
            }
            assertThrows(IOException.class, () -> LayerSets.verifyAndPin("repo", document, graph));
        }
    }

    @Test void rejectsDuplicateCoordinatesAndFilenamesAndNonRuntimeInputs() throws Exception {
        var a = member("a", "runtime");
        Path other = Files.createDirectory(temp.resolve("other")).resolve("a.jar");
        Files.writeString(other, "different bytes");
        var duplicateFile = new MavenGraph.Member("other", "different", "1", "jar", "", "runtime", other);
        assertThrows(IOException.class, () -> graph(a, duplicateFile));
        assertThrows(IOException.class, () -> graph(a, a));
        assertThrows(IOException.class, () -> graph(member("test", "test")));
        Path link = temp.resolve("link.jar");
        Files.createSymbolicLink(link, a.file());
        assertThrows(IOException.class, () -> graph(new MavenGraph.Member("example", "link",
                "1", "jar", "", "runtime", link)));
    }

    @Test void stagingPreservesJarModesAndTimesButNormalizesGeneratedMetadataAndDirectories() throws Exception {
        var a = member("a", "runtime");
        Files.setPosixFilePermissions(a.file(), PosixFilePermissions.fromString("rw-r-----"));
        Files.setLastModifiedTime(a.file(), FileTime.fromMillis(1234000));
        MavenGraph graph = graph(a);
        Path root = graph.stage(Files.createDirectory(temp.resolve("stage")));
        Path copy = root.resolve("lib/a.jar");
        assertArrayEquals(Files.readAllBytes(a.file()), Files.readAllBytes(copy));
        assertEquals(Files.getLastModifiedTime(a.file()), Files.getLastModifiedTime(copy));
        assertEquals(Files.getPosixFilePermissions(a.file()), Files.getPosixFilePermissions(copy));
        for (Path path : List.of(root, root.resolve("lib"), root.resolve(".brewlet"),
                root.resolve(".brewlet/maven-lock.json"))) {
            assertEquals(FileTime.fromMillis(0), Files.getLastModifiedTime(path));
        }
        assertArrayEquals(graph.canonicalBytes(), Files.readAllBytes(root.resolve(".brewlet/maven-lock.json")));
    }

    @Test void configAndCommandsDelegateDescriptorAuthorityAndTrustToCli() throws Exception {
        MavenGraph graph = graph(member("a", "runtime"));
        LayerSets.writeInputs(graph, temp, "approved", "2026.10", List.of("java-25", "java-21"));
        JsonNode config = MavenGraph.JSON.readTree(temp.resolve("config.json").toFile());
        assertEquals(0, config.path("layers").size());
        assertEquals("java", config.path("ecosystem").textValue());
        assertEquals("java-21", config.at("/compatibleRuntimeImages/0").textValue());
        assertFalse(config.has("lockDigest"));
        assertFalse(config.has("sourceBom"));
        var trust = new LayerSets.Trust(List.of("/key one.pem", "/key two.pem"), "builder identity", true);
        var inspect = LayerSets.inspectCommand("/cli path", "registry.test/repo:tag", trust);
        assertEquals(List.of("/cli path", "inspect", "registry.test/repo:tag",
                "--trust-key", "/key one.pem", "--trust-key", "/key two.pem",
                "--signer-identity", "builder identity", "--require-provenance"), inspect);
        var publish = LayerSets.publishCommand("/cli path", "registry.test/repo:tag",
                temp.resolve("store with spaces"), temp.resolve("config.json"), temp.resolve("lock.json"),
                temp.resolve("deps with spaces"), "/signing key.pem", "builder identity");
        assertTrue(publish.contains("dependency=" + temp.resolve("deps with spaces") + ":/app"));
        assertEquals(List.of("--signing-key", "/signing key.pem", "--builder-identity", "builder identity"),
                publish.subList(publish.size() - 4, publish.size()));
        var prepared = new JavaProducer.Prepared(temp.resolve("launch.json"), temp.resolve("app"), null, "/app/lib");
        List<String> push = JavaProducer.command("cli", "image", null, List.of("linux/arm64"),
                List.of("repo@" + DIGEST), prepared);
        trust.append(push);
        assertEquals(1, push.stream().filter("--layer"::equals).count());
        assertFalse(push.stream().anyMatch(arg -> arg.startsWith("dependency=")));
        assertEquals(2, push.stream().filter("--trust-key"::equals).count());
    }
}
