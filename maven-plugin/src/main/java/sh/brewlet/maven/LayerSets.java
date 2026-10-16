package sh.brewlet.maven;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.node.ArrayNode;
import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.HashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;

final class LayerSets {
    record Trust(List<String> keys, String identity, boolean requireProvenance) {
        void append(List<String> command) {
            for (String key : keys) {
                command.addAll(List.of("--trust-key", key));
            }
            if (identity != null && !identity.isBlank()) {
                command.addAll(List.of("--signer-identity", identity));
            }
            if (requireProvenance) {
                command.add("--require-provenance");
            }
        }
    }

    static List<String> inspectCommand(String executable, String reference, Trust trust) {
        List<String> command = new ArrayList<>(List.of(executable, "inspect", reference));
        trust.append(command);
        return command;
    }

    static List<String> publishCommand(String executable, String reference, Path store,
            Path config, Path lock, Path dependencies, String signingKey, String builderIdentity) {
        List<String> command = new ArrayList<>(List.of(executable, "layer-set", reference));
        if (store != null) {
            command.addAll(List.of("--store", store.toString()));
        }
        command.addAll(List.of("--config", config.toString(), "--lock", lock.toString(),
                "--layer", "dependency=" + dependencies + ":/app"));
        signing(command, signingKey, builderIdentity);
        return command;
    }

    static void signing(List<String> command, String key, String identity) {
        if (key != null && !key.isBlank()) {
            command.addAll(List.of("--signing-key", key));
        }
        if (identity != null && !identity.isBlank()) {
            command.addAll(List.of("--builder-identity", identity));
        }
    }

    static void writeInputs(MavenGraph graph, Path output, String name, String version,
                            List<String> compatibleRuntimeImages) throws IOException {
        if (name == null || name.isBlank() || version == null || version.isBlank()) {
            throw new IOException("Layer-set name and version are required");
        }
        Map<String, Object> config = new java.util.LinkedHashMap<>();
        config.put("schemaVersion", 1);
        config.put("name", name);
        config.put("version", version);
        config.put("ecosystem", "java");
        config.put("layers", List.of());
        if (!compatibleRuntimeImages.isEmpty()) {
            config.put("compatibleRuntimeImages", compatibleRuntimeImages.stream().sorted().distinct().toList());
        }
        Files.write(output.resolve("config.json"), MavenGraph.JSON.writeValueAsBytes(config));
        Files.write(output.resolve("lock.json"), MavenGraph.JSON.writeValueAsBytes(
                Map.of("schemaVersion", 1, "entries", graph.neutralEntries())));
    }

    /** Only CLI-verified lock inventory is trusted, and it must equal the actual Maven graph. */
    static String verifyAndPin(String reference, JsonNode inspected, MavenGraph graph) throws IOException {
        if (inspected == null || inspected.path("layerSet").path("schemaVersion").asInt() != 1
                || !"java".equals(inspected.path("layerSet").path("ecosystem").asText())
                || inspected.path("lock").path("schemaVersion").asInt() != 1
                || !inspected.path("lock").path("entries").isArray()) {
            throw new IOException("Expected a verified java layer set with a version 1 inventory lock");
        }
        String digest = inspected.path("digest").asText();
        if (!digest.matches("sha256:[0-9a-f]{64}")
                || !digest.equals(inspected.path("evidence").path("manifestDigest").asText())) {
            throw new IOException("Inspection did not return a bound manifest digest");
        }
        int suppliedPin = reference.lastIndexOf("@sha256:");
        if (suppliedPin >= 0 && !reference.substring(suppliedPin + 1).equals(digest)) {
            throw new IOException("Inspected manifest differs from the requested layer-set digest");
        }
        JsonNode actual = inspected.path("lock").path("entries");
        ArrayNode expected = MavenGraph.JSON.valueToTree(graph.neutralEntries());
        Set<JsonNode> members = new HashSet<>();
        actual.forEach(members::add);
        Set<JsonNode> wanted = new HashSet<>();
        expected.forEach(wanted::add);
        if (members.size() != actual.size() || !wanted.equals(members)) {
            throw new IOException("Layer-set lock does not exactly match resolved Maven JAR paths/hashes "
                    + "and canonical source BOM/scoped graph");
        }
        return pin(reference, digest);
    }

    static String pin(String reference, String digest) {
        int separator = reference.indexOf('@');
        String base = separator < 0 ? reference : reference.substring(0, separator);
        if (!Files.isDirectory(Path.of(base))) {
            int colon = base.lastIndexOf(':');
            if (colon > base.lastIndexOf('/')) {
                base = base.substring(0, colon);
            }
        }
        return base + "@" + digest;
    }

    private LayerSets() {}
}
