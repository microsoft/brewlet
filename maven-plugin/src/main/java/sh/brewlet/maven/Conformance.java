package sh.brewlet.maven;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ArrayNode;
import com.fasterxml.jackson.databind.node.NullNode;
import com.fasterxml.jackson.databind.node.ObjectNode;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.function.Consumer;

/**
 * Evaluates the published conformance fixtures with the Java contract
 * implementation and reports results in the same normalized shape as
 * {@code brewlet conformance FILE}.
 */
final class Conformance {
    private static final ObjectMapper JSON = new ObjectMapper();

    private static final class Fixture {
        String name = "", kind = "", launch = "", image = "", value = "", reason = "";
        List<LayerContract.Entry> entries = List.of();
        List<List<LayerContract.Entry>> layers = List.of();
        JsonNode process;
    }

    /** Returns results for every case, or throws for a malformed suite as the CLI does. */
    static JsonNode run(String data) {
        Json root = Json.parse(data, false);
        long[] schemaVersion = {0};
        List<Json> cases = new ArrayList<>();
        ArtifactContract.goStruct(root, Map.of(
                "schemaVersion", v -> schemaVersion[0] = ArtifactContract.goLong(v, schemaVersion[0]),
                "cases", v -> {
                    cases.clear();
                    ArtifactContract.goArray(v, cases::add);
                }));
        if (schemaVersion[0] != 1) {
            throw new IllegalArgumentException("unsupported fixture suite");
        }
        if (cases.isEmpty()) {
            return NullNode.getInstance();
        }
        List<Fixture> fixtures = cases.stream().map(Conformance::fixture).toList();
        ArrayNode results = JSON.createArrayNode();
        for (Fixture fixture : fixtures) {
            try {
                results.add(evaluate(fixture));
            } catch (IllegalArgumentException invalid) {
                throw new IllegalArgumentException(fixture.name + ": " + invalid.getMessage(), invalid);
            }
        }
        return results;
    }

    private static ObjectNode evaluate(Fixture f) {
        ArtifactContract.Process process = null;
        String reason = "";
        try {
            switch (f.kind) {
                case "launch" -> process = ArtifactContract.parseLaunch(f.launch).process();
                case "mirror" -> ArtifactContract.validateMirror(ArtifactContract.parseLaunch(f.launch), f.image);
                case "id" -> ArtifactContract.validateRuntimeImageID(f.value);
                case "platform" -> ArtifactContract.parsePlatform(f.value);
                case "compatibility" -> {
                    if (!ArtifactContract.parseLaunch(f.launch).allows(f.value)) {
                        throw new ArtifactContract.Violation("RuntimeImageIncompatible",
                                "runtime image " + ArtifactContract.quote(f.value) + " excluded by launch allow-list");
                    }
                }
                case "layer" -> LayerContract.validateComposition(List.of(f.entries));
                case "composition" -> LayerContract.validateComposition(f.layers);
                default -> throw new IllegalArgumentException("unknown fixture kind " + ArtifactContract.quote(f.kind));
            }
        } catch (ArtifactContract.Violation violation) {
            reason = violation.reason;
        }
        ObjectNode result = JSON.createObjectNode().put("name", f.name);
        JsonNode got = process == null ? NullNode.getInstance() : process(process);
        if (process != null) {
            result.set("process", got);
        }
        if (!reason.isEmpty()) {
            result.put("reason", reason);
        }
        boolean passed = reason.equals(f.reason);
        if (f.process != null && reason.isEmpty()) {
            passed = passed && f.process.equals(got);
        }
        return result.put("passed", passed);
    }

    static ObjectNode process(ArtifactContract.Process process) {
        ObjectNode node = JSON.createObjectNode();
        if (process.entrypoint() == null) {
            node.putNull("entrypoint");
        } else {
            process.entrypoint().forEach(node.putArray("entrypoint")::add);
        }
        if (process.args() != null && !process.args().isEmpty()) {
            process.args().forEach(node.putArray("args")::add);
        }
        if (process.workingDir() != null && !process.workingDir().isEmpty()) {
            node.put("workingDir", process.workingDir());
        }
        if (process.env() != null && !process.env().isEmpty()) {
            ArrayNode env = node.putArray("env");
            process.env().forEach(e -> env.addObject().put("name", e.name()).put("value", e.value()));
        }
        return node;
    }

    private static Fixture fixture(Json value) {
        Fixture f = new Fixture();
        Consumer<Json> entries = v -> f.entries = entries(v);
        ArtifactContract.goStruct(value, Map.of(
                "name", v -> f.name = ArtifactContract.goString(v, f.name),
                "kind", v -> f.kind = ArtifactContract.goString(v, f.kind),
                "launch", v -> f.launch = v.raw,
                "image", v -> f.image = v.raw,
                "value", v -> f.value = ArtifactContract.goString(v, f.value),
                "entries", entries,
                "layers", v -> {
                    List<List<LayerContract.Entry>> layers = new ArrayList<>();
                    ArtifactContract.goArray(v, layer -> layers.add(entries(layer)));
                    f.layers = layers;
                },
                "process", v -> f.process = v.kind == Json.Kind.NULL ? null : expectedProcess(v),
                "reason", v -> f.reason = ArtifactContract.goString(v, f.reason)));
        return f;
    }

    private static List<LayerContract.Entry> entries(Json value) {
        List<LayerContract.Entry> out = new ArrayList<>();
        ArtifactContract.goArray(value, item -> {
            String[] text = {"", "", "", ""};
            long[] mode = {0};
            boolean[] xattrs = {false};
            ArtifactContract.goStruct(item, Map.of(
                    "path", v -> text[0] = ArtifactContract.goString(v, text[0]),
                    "type", v -> text[1] = ArtifactContract.goString(v, text[1]),
                    "mode", v -> mode[0] = ArtifactContract.goLong(v, mode[0]),
                    "data", v -> text[2] = ArtifactContract.goString(v, text[2]),
                    "link", v -> text[3] = ArtifactContract.goString(v, text[3]),
                    "xattrs", v -> {
                        Map<String, String> map = ArtifactContract.goStringMap(v);
                        xattrs[0] = map != null && !map.isEmpty();
                    }));
            char type = LayerContract.Entry.type(text[1]);
            // The reference builds real tar headers; a non-directory name with a trailing slash is unencodable.
            if (type != LayerContract.DIRECTORY && text[0].endsWith("/")) {
                throw new IllegalArgumentException("archive/tar: cannot encode header: filename may not have trailing slash");
            }
            out.add(new LayerContract.Entry(text[0], type, mode[0], text[2], text[3], xattrs[0]));
        });
        return out;
    }

    private static JsonNode expectedProcess(Json value) {
        List<List<String>> lists = new ArrayList<>(java.util.Arrays.asList(null, null));
        String[] workingDir = {""};
        List<ArtifactContract.Env> env = new ArrayList<>();
        ArtifactContract.goStruct(value, Map.of(
                "entrypoint", v -> lists.set(0, ArtifactContract.goStrings(v)),
                "args", v -> lists.set(1, ArtifactContract.goStrings(v)),
                "workingDir", v -> workingDir[0] = ArtifactContract.goString(v, workingDir[0]),
                "env", v -> {
                    env.clear();
                    ArtifactContract.goArray(v, item -> {
                        String[] pair = {"", ""};
                        ArtifactContract.goStruct(item, Map.of(
                                "name", x -> pair[0] = ArtifactContract.goString(x, pair[0]),
                                "value", x -> pair[1] = ArtifactContract.goString(x, pair[1])));
                        env.add(new ArtifactContract.Env(pair[0], pair[1]));
                    });
                }));
        return process(new ArtifactContract.Process(lists.get(0), lists.get(1), workingDir[0], env));
    }

    private Conformance() {}
}
