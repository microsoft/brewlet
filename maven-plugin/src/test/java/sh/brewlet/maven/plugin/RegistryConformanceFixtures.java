// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;

import java.io.IOException;
import java.util.ArrayList;
import java.util.List;
import java.util.stream.Stream;
import java.util.stream.StreamSupport;

/** The Go testdata file is copied onto the test classpath by Maven. */
public final class RegistryConformanceFixtures {
    private RegistryConformanceFixtures() {}

    public static Stream<JsonNode> cases(String section) throws IOException {
        try (var input = RegistryConformanceFixtures.class.getResourceAsStream(
                "/registry-conformance/conformance.json")) {
            if (input == null) {
                throw new IOException("Missing shared registry conformance fixtures");
            }
            JsonNode cases = new ObjectMapper().readTree(input).required(section);
            if (!cases.isArray() || cases.isEmpty()) {
                throw new IOException("Empty conformance section: " + section);
            }
            return StreamSupport.stream(cases.spliterator(), false);
        }
    }

    public static List<String> strings(JsonNode node) {
        List<String> values = new ArrayList<>();
        node.forEach(value -> values.add(value.asText()));
        return values;
    }
}
