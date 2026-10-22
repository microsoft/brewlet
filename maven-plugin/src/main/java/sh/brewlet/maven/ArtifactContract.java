package sh.brewlet.maven;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.time.LocalDateTime;
import java.util.ArrayList;
import java.util.HashSet;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Map;
import java.util.Objects;
import java.util.Set;
import java.util.function.Consumer;
import java.util.regex.Pattern;

/**
 * Java evaluation of the language-neutral launch, runtime-ID, platform and image
 * mirror rules (specification §6). Reason codes and accepted shapes are kept
 * identical to the CLI and shim; the shared conformance fixtures prove it.
 */
final class ArtifactContract {
    static final String INVALID_LAUNCH = "InvalidLaunchConfig";
    static final String MISMATCH = "LaunchConfigMismatch";

    /** A contract violation carrying its normative reason code. */
    static final class Violation extends IOException {
        final String reason;

        Violation(String reason, String detail) {
            super(reason + ": " + detail);
            this.reason = reason;
        }
    }

    record Env(String name, String value) {}
    record Process(List<String> entrypoint, List<String> args, String workingDir, List<Env> env) {}
    /** {@code runtimeImages} is null when absent: an explicit empty list denies every runtime image. */
    record Launch(Process process, List<String> runtimeImages) {
        boolean allows(String id) {
            return runtimeImages == null || runtimeImages.contains(id);
        }
    }

    private static final Pattern ID = Pattern.compile(
            "[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*");
    private static final Pattern PLATFORM = Pattern.compile("[a-z0-9][a-z0-9_.-]*");
    private static final Pattern RFC3339 = Pattern.compile(
            "(\\d{4}-\\d{2}-\\d{2}T\\d{2}:\\d{2}:\\d{2})(\\.\\d+)?(Z|[+-](\\d{2}):(\\d{2}))");

    static void validateRuntimeImageID(String id) throws Violation {
        if (id.getBytes(StandardCharsets.UTF_8).length > 55 || !ID.matcher(id).matches()) {
            throw new Violation("InvalidRuntimeImageID", quote(id));
        }
    }

    static List<String> parsePlatform(String platform) throws Violation {
        String[] parts = platform.split("/", -1);
        if (parts.length < 2 || parts.length > 3) {
            throw new Violation("InvalidPlatform", "expected os/arch[/variant]");
        }
        for (String part : parts) {
            if (!PLATFORM.matcher(part).matches()) {
                throw new Violation("InvalidPlatform", "invalid component " + quote(part));
            }
        }
        return List.of(parts);
    }

    static Launch parseLaunch(String data) throws Violation {
        Launch launch;
        try {
            launch = decodeLaunch(Json.parse(data, true));
        } catch (IllegalArgumentException invalid) {
            throw new Violation(INVALID_LAUNCH, invalid.getMessage());
        }
        if (launch.runtimeImages() != null) {
            Set<String> seen = new HashSet<>();
            for (String id : launch.runtimeImages()) {
                validateRuntimeImageID(id);
                if (!seen.add(id)) {
                    throw new Violation(INVALID_LAUNCH, "duplicate runtime ID");
                }
            }
        }
        return launch;
    }

    private static Launch decodeLaunch(Json root) {
        if (root.containsNull()) {
            throw new IllegalArgumentException("null launch fields are not allowed");
        }
        exactKeys(root, Set.of("schemaVersion", "process", "runtimeImages"));
        Json process = root.get("process");
        Json schema = root.get("schemaVersion");
        long version = schema == null ? 0 : goLong(schema, 0);
        List<String> images = root.get("runtimeImages") == null ? null : goStrings(root.get("runtimeImages"));
        List<String> entrypoint = List.of();
        List<String> args = List.of();
        String workingDir = "";
        List<Env> env = new ArrayList<>();
        if (process != null) {
            exactKeys(process, Set.of("entrypoint", "args", "workingDir", "env"));
            if (process.get("entrypoint") != null) {
                entrypoint = goStrings(process.get("entrypoint"));
            }
            if (process.get("args") != null) {
                args = goStrings(process.get("args"));
            }
            if (process.get("workingDir") != null) {
                workingDir = goString(process.get("workingDir"), "");
                if (workingDir.isEmpty()) {
                    throw new IllegalArgumentException("workingDir must be absolute");
                }
            }
            Json entries = process.get("env");
            if (entries != null) {
                if (entries.kind != Json.Kind.ARRAY) {
                    throw new IllegalArgumentException("env must be an array");
                }
                for (Json entry : entries.items) {
                    exactKeys(entry, Set.of("name", "value"));
                    if (entry.get("value") == null) {
                        throw new IllegalArgumentException("environment value is required");
                    }
                    env.add(new Env(entry.get("name") == null ? "" : goString(entry.get("name"), ""),
                            goString(entry.get("value"), "")));
                }
            }
        }
        if (version != 2 || entrypoint.isEmpty() || entrypoint.get(0).isEmpty()) {
            throw new IllegalArgumentException("schemaVersion 2 and nonempty entrypoint required");
        }
        for (String arg : concat(entrypoint, args)) {
            if (arg.indexOf(0) >= 0) {
                throw new IllegalArgumentException("NUL in argv");
            }
        }
        if (workingDir.isEmpty()) {
            workingDir = "/app";
        }
        if (!workingDir.startsWith("/") || workingDir.indexOf(0) >= 0) {
            throw new IllegalArgumentException("workingDir must be absolute");
        }
        Set<String> names = new HashSet<>();
        for (Env e : env) {
            if (e.name().isEmpty() || e.name().equals("PATH") || e.name().indexOf('=') >= 0
                    || e.name().indexOf(0) >= 0 || e.value().indexOf(0) >= 0 || !names.add(e.name())) {
                throw new IllegalArgumentException("invalid or duplicate env " + quote(e.name()));
            }
        }
        return new Launch(new Process(entrypoint, args, workingDir, List.copyOf(env)), images);
    }

    /** Mutable target mirroring how OCI image config decoding merges case-variant members. */
    private static final class ImageConfig {
        List<String> entrypoint;
        List<String> cmd;
        List<String> env;
        String workingDir = "";
        String user = "";
        String stopSignal = "";
        boolean argsEscaped;
        Set<String> exposedPorts = new LinkedHashSet<>();
        Set<String> volumes = new LinkedHashSet<>();
    }

    static void validateMirror(Launch launch, String image) throws Violation {
        ImageConfig got;
        try {
            got = parseImage(image);
        } catch (IllegalArgumentException invalid) {
            throw new Violation(MISMATCH, invalid.getMessage());
        }
        Process want = launch.process();
        List<String> env = want.env().stream().map(e -> e.name() + "=" + e.value()).toList();
        if (!sameList(want.entrypoint(), got.entrypoint) || !sameList(want.args(), got.cmd)
                || !sameList(env, got.env) || !want.workingDir().equals(got.workingDir)
                || !got.user.isEmpty() || !got.exposedPorts.isEmpty() || !got.volumes.isEmpty()
                || !got.stopSignal.isEmpty() || got.argsEscaped) {
            throw new Violation(MISMATCH, "image config differs from launch.json");
        }
    }

    // Execution fields absent from the OCI image model (for example Docker's
    // Healthcheck) are rejected before typed decoding would silently drop them.
    private static ImageConfig parseImage(String data) {
        Json root = Json.parse(data, true);
        if (root.kind != Json.Kind.OBJECT && root.kind != Json.Kind.NULL) {
            throw new IllegalArgumentException("image config must be an object");
        }
        Json config = null;
        if (root.kind == Json.Kind.OBJECT) {
            for (Json.Member member : root.members) {
                if (member.key().equalsIgnoreCase("config")) {
                    if (member.value().kind != Json.Kind.OBJECT && member.value().kind != Json.Kind.NULL) {
                        throw new IllegalArgumentException("invalid config");
                    }
                    config = member.value();
                }
            }
        }
        if (config != null && config.kind == Json.Kind.OBJECT) {
            for (Json.Member member : config.members) {
                boolean unsupported = List.of("Healthcheck", "Shell", "OnBuild").stream()
                        .anyMatch(member.key()::equalsIgnoreCase);
                if (unsupported && !Set.of("null", "{}", "[]", "\"\"").contains(member.value().raw)) {
                    throw new IllegalArgumentException("unsupported config " + member.key());
                }
            }
        }
        ImageConfig c = new ImageConfig();
        Consumer<Json> string = v -> goString(v, "");
        Consumer<Json> strings = ArtifactContract::goStrings;
        goStruct(root, Map.of("created", ArtifactContract::goTime, "author", string,
                "architecture", string, "os", string, "os.version", string, "os.features", strings,
                "variant", string, "config", v -> goStruct(v, Map.of(
                        "User", x -> c.user = goString(x, c.user),
                        "ExposedPorts", x -> c.exposedPorts = goSet(x, c.exposedPorts),
                        "Env", x -> c.env = goStrings(x),
                        "Entrypoint", x -> c.entrypoint = goStrings(x),
                        "Cmd", x -> c.cmd = goStrings(x),
                        "Volumes", x -> c.volumes = goSet(x, c.volumes),
                        "WorkingDir", x -> c.workingDir = goString(x, c.workingDir),
                        "Labels", ArtifactContract::goStringMap,
                        "StopSignal", x -> c.stopSignal = goString(x, c.stopSignal),
                        "ArgsEscaped", x -> c.argsEscaped = goBool(x, c.argsEscaped))),
                "rootfs", v -> goStruct(v, Map.of("type", string, "diff_ids", strings)),
                "history", v -> goArray(v, item -> goStruct(item, Map.of("created", ArtifactContract::goTime,
                        "created_by", string, "author", string, "comment", string,
                        "empty_layer", x -> goBool(x, false))))));
        return c;
    }

    private static void exactKeys(Json object, Set<String> keys) {
        if (object.kind != Json.Kind.OBJECT) {
            throw new IllegalArgumentException("expected JSON object");
        }
        for (Json.Member member : object.members) {
            if (!keys.contains(member.key())) {
                throw new IllegalArgumentException("unknown JSON field " + quote(member.key()));
            }
        }
    }

    // Decoders below follow conventional JSON-to-struct semantics used by the
    // reference implementation: case-insensitive member names (exact names win),
    // ignored unknown members, null leaving scalars unchanged and clearing lists.
    static void goStruct(Json value, Map<String, Consumer<Json>> fields) {
        if (value.kind == Json.Kind.NULL) {
            return;
        }
        if (value.kind != Json.Kind.OBJECT) {
            throw new IllegalArgumentException("cannot decode " + value.kind + " into object");
        }
        for (Json.Member member : value.members) {
            Consumer<Json> field = fields.get(member.key());
            if (field == null) {
                field = fields.entrySet().stream().filter(e -> e.getKey().equalsIgnoreCase(member.key()))
                        .map(Map.Entry::getValue).findFirst().orElse(null);
            }
            if (field != null) {
                field.accept(member.value());
            }
        }
    }

    static String goString(Json value, String current) {
        return switch (value.kind) {
            case NULL -> current;
            case STRING -> value.text;
            default -> throw new IllegalArgumentException("cannot decode " + value.kind + " into string");
        };
    }

    static List<String> goStrings(Json value) {
        List<String> out = new ArrayList<>();
        goArray(value, item -> out.add(goString(item, "")));
        return value.kind == Json.Kind.NULL ? null : out;
    }

    static void goArray(Json value, Consumer<Json> item) {
        if (value.kind == Json.Kind.NULL) {
            return;
        }
        if (value.kind != Json.Kind.ARRAY) {
            throw new IllegalArgumentException("cannot decode " + value.kind + " into array");
        }
        value.items.forEach(item);
    }

    static long goLong(Json value, long current) {
        if (value.kind == Json.Kind.NULL) {
            return current;
        }
        if (value.kind == Json.Kind.NUMBER && value.text.matches("-?\\d+")) {
            try {
                return Long.parseLong(value.text);
            } catch (NumberFormatException overflow) {
                // Reported below as an unrepresentable integer.
            }
        }
        throw new IllegalArgumentException("cannot decode " + value.raw + " into integer");
    }

    private static boolean goBool(Json value, boolean current) {
        return switch (value.kind) {
            case NULL -> current;
            case BOOLEAN -> value.text.equals("true");
            default -> throw new IllegalArgumentException("cannot decode " + value.kind + " into bool");
        };
    }

    static Map<String, String> goStringMap(Json value) {
        Map<String, String> out = new java.util.LinkedHashMap<>();
        if (value.kind == Json.Kind.NULL) {
            return null;
        }
        if (value.kind != Json.Kind.OBJECT) {
            throw new IllegalArgumentException("cannot decode " + value.kind + " into map");
        }
        value.members.forEach(member -> out.put(member.key(), goString(member.value(), "")));
        return out;
    }

    private static Set<String> goSet(Json value, Set<String> current) {
        if (value.kind == Json.Kind.NULL) {
            return new LinkedHashSet<>();
        }
        if (value.kind != Json.Kind.OBJECT) {
            throw new IllegalArgumentException("cannot decode " + value.kind + " into map");
        }
        for (Json.Member member : value.members) {
            goStruct(member.value(), Map.of());
            current.add(member.key());
        }
        return current;
    }

    private static void goTime(Json value) {
        if (value.kind == Json.Kind.NULL) {
            return;
        }
        var match = value.kind == Json.Kind.STRING ? RFC3339.matcher(value.text) : null;
        try {
            if (match != null && match.matches()) {
                LocalDateTime.parse(match.group(1));
                if (match.group(4) == null
                        || Integer.parseInt(match.group(4)) < 24 && Integer.parseInt(match.group(5)) < 60) {
                    return;
                }
            }
        } catch (java.time.format.DateTimeParseException invalid) {
            // Reported below.
        }
        throw new IllegalArgumentException("invalid RFC 3339 time " + value.raw);
    }

    private static boolean sameList(List<String> a, List<String> b) {
        return Objects.equals(a == null ? List.of() : a, b == null ? List.of() : b);
    }

    private static List<String> concat(List<String> a, List<String> b) {
        List<String> out = new ArrayList<>(a);
        out.addAll(b);
        return out;
    }

    static String quote(String value) {
        return "\"" + value.replace("\\", "\\\\").replace("\"", "\\\"") + "\"";
    }

    private ArtifactContract() {}
}
