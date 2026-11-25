package sh.brewlet.maven;

import static org.junit.jupiter.api.Assertions.*;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.List;
import org.junit.jupiter.api.Test;

class ConformanceTest {
    private static final Path FIXTURES = Path.of("..", "conformance", "artifact-v1.json");

    @Test void javaEvaluatorPassesEverySharedFixture() throws Exception {
        String data = Files.readString(FIXTURES);
        JsonNode results = Conformance.run(data);
        int cases = new ObjectMapper().readTree(data).path("cases").size();
        assertTrue(cases > 0);
        assertEquals(cases, results.size());
        for (JsonNode result : results) {
            assertTrue(result.path("passed").booleanValue(), result.toString());
        }
    }

    @Test void processIsReportedOnlyForParsedLaunchCases() {
        JsonNode results = Conformance.run("{\"schemaVersion\":1,\"cases\":["
                + "{\"name\":\"a\",\"kind\":\"launch\",\"launch\":{\"schemaVersion\":1,\"process\":{\"command\":[\"x\"],\"args\":[]}}},"
                + "{\"name\":\"b\",\"kind\":\"id\",\"value\":\"r\"},"
                + "{\"name\":\"c\",\"kind\":\"launch\",\"launch\":{\"schemaVersion\":1.0,\"process\":{\"command\":[\"x\"]}},\"reason\":\"Other\"}]}");
        assertEquals("{\"name\":\"a\",\"process\":{\"command\":[\"x\"],\"args\":[],\"workingDir\":\"/app\"},\"passed\":true}",
                results.get(0).toString());
        assertEquals("{\"name\":\"b\",\"passed\":true}", results.get(1).toString());
        assertEquals("{\"name\":\"c\",\"reason\":\"InvalidLaunchConfig\",\"passed\":false}", results.get(2).toString());
        assertTrue(Conformance.run("{\"schemaVersion\":1,\"cases\":[]}").isNull());
        assertThrows(IllegalArgumentException.class, () -> Conformance.run("{\"schemaVersion\":2}"));
        assertThrows(IllegalArgumentException.class, () -> Conformance.run(
                "{\"schemaVersion\":1,\"cases\":[{\"name\":\"x\",\"kind\":\"unknown\"}]}"));
    }

    @Test void launchRulesCarryReasonCodes() throws Exception {
        var launch = ArtifactContract.parseLaunch("{\"schemaVersion\":1,\"process\":{\"command\":[\"java\"],"
                + "\"env\":[{\"name\":\"B\",\"value\":\"1\"},{\"name\":\"A\",\"value\":\"2\"}]}}");
        assertEquals(List.of("B", "A"), launch.process().env().stream().map(ArtifactContract.Env::name).toList());
        var inherited = ArtifactContract.parseLaunch("{\"schemaVersion\":1,\"process\":{}}").process();
        assertNull(inherited.cmd());
        assertEquals(List.of(), ArtifactContract.parseLaunch(
                "{\"schemaVersion\":1,\"process\":{\"command\":[]}}").process().cmd());
        for (String invalid : List.of("", "{}", "[]", "{\"schemaVersion\":1}",
                "{\"schemaVersion\":1,\"process\":{\"entrypoint\":[\"java\"]}}",
                "{\"schemaVersion\":1,\"process\":{\"command\":[\"\"]}}", "{\"schemaVersion\":1,\"process\":{\"command\":[\"a\\u0000\"]}}",
                "{\"schemaVersion\":1,\"process\":{\"command\":[\"a\"]}} {}",
                "{\"schemaVersion\":1,\"process\":{\"command\":[\"a\"],\"env\":[{\"name\":\"A=B\",\"value\":\"\"}]}}")) {
            var violation = assertThrows(ArtifactContract.Violation.class, () -> ArtifactContract.parseLaunch(invalid));
            assertEquals("InvalidLaunchConfig", violation.reason);
            assertTrue(violation.getMessage().startsWith("InvalidLaunchConfig: "));
        }
        assertEquals("InvalidRuntimeImageID", assertThrows(ArtifactContract.Violation.class,
                () -> ArtifactContract.validateRuntimeImageID("a".repeat(56))).reason);
        assertEquals("InvalidPlatform", assertThrows(ArtifactContract.Violation.class,
                () -> ArtifactContract.parsePlatform("linux/amd64\n")).reason);
    }

    @Test void layerPathsUseLexicalSlashSemantics() {
        assertEquals("app", LayerContract.clean("app/./x/.."));
        assertEquals("/", LayerContract.clean("/.."));
        assertEquals(".", LayerContract.dir("app"));
        assertEquals("app", LayerContract.join(".", "app"));
        assertEquals("/", LayerContract.base("///"));
    }
}
