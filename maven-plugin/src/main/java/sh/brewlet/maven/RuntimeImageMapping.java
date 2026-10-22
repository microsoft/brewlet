package sh.brewlet.maven;

import java.io.IOException;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Properties;

/** Maps Java feature releases to runtime image IDs so the allow-list follows the targeted bytecode level. */
final class RuntimeImageMapping {
    private final Map<String, Integer> versions = new LinkedHashMap<>();

    /** Parses entries shaped {@code 21=java-21}; XML element names cannot start with a digit. */
    static RuntimeImageMapping parse(List<String> entries) throws IOException {
        RuntimeImageMapping mapping = new RuntimeImageMapping();
        for (String entry : entries == null ? List.<String>of() : entries) {
            String[] parts = entry.strip().split("=", 2);
            if (parts.length != 2 || !parts[0].strip().matches("[1-9][0-9]{0,3}")) {
                throw new IOException("runtimeImageMappings entries must look like 21=java-21: " + entry);
            }
            String id = parts[1].strip();
            ArtifactContract.validateRuntimeImageID(id);
            if (mapping.versions.put(id, Integer.parseInt(parts[0].strip())) != null) {
                throw new IOException("Duplicate runtime image ID in runtimeImageMappings: " + id);
            }
        }
        return mapping;
    }

    boolean isEmpty() {
        return versions.isEmpty();
    }

    /** Returns the numeric Java feature release the project targets, or null when unknown. */
    static Integer release(Properties properties) {
        for (String key : List.of("maven.compiler.release", "maven.compiler.target", "maven.compiler.source")) {
            String value = properties.getProperty(key);
            if (value != null && value.strip().matches("[1-9][0-9]{0,3}")) {
                return Integer.valueOf(value.strip());
            }
        }
        return null;
    }

    static boolean preview(List<String> javaArgs) {
        return javaArgs != null && javaArgs.contains("--enable-preview");
    }

    private static boolean compatible(int version, int release, boolean preview) {
        return preview ? version == release : version >= release;
    }

    /** Proposes every mapped ID able to run bytecode for {@code release}, ascending by feature version. */
    List<String> propose(int release, boolean preview) throws IOException {
        List<String> ids = new ArrayList<>(versions.keySet().stream()
                .filter(id -> compatible(versions.get(id), release, preview))
                .sorted(Comparator.comparing(versions::get)).toList());
        if (ids.isEmpty()) {
            throw new IOException("No runtimeImageMappings entry provides Java " + release
                    + (preview ? " (exactly, because --enable-preview is set)" : " or newer"));
        }
        return ids;
    }

    /** Describes configured IDs that the mapping places below the release the generated argv needs. */
    List<String> warnings(List<String> runtimeImages, int release, boolean preview) {
        List<String> warnings = new ArrayList<>();
        for (String id : runtimeImages) {
            Integer version = versions.get(id);
            if (version != null && !compatible(version, release, preview)) {
                warnings.add("Runtime image " + id + " provides Java " + version + " but the project targets Java "
                        + release + (preview ? " with --enable-preview" : ""));
            }
        }
        return warnings;
    }

    boolean maps(String id) {
        return versions.containsKey(id);
    }
}
