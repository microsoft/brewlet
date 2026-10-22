package sh.brewlet.maven;

import static org.junit.jupiter.api.Assertions.*;

import java.io.IOException;
import java.util.List;
import java.util.Properties;
import org.junit.jupiter.api.Test;

class RuntimeImageMappingTest {
    private static Properties properties(String... pairs) {
        Properties properties = new Properties();
        for (int i = 0; i < pairs.length; i += 2) {
            properties.setProperty(pairs[i], pairs[i + 1]);
        }
        return properties;
    }

    @Test void readsNumericCompilerReleaseWithTargetAndSourceFallback() {
        assertEquals(21, RuntimeImageMapping.release(properties("maven.compiler.release", " 21 ",
                "maven.compiler.target", "17")));
        assertEquals(17, RuntimeImageMapping.release(properties("maven.compiler.target", "17")));
        assertEquals(11, RuntimeImageMapping.release(properties("maven.compiler.target", "1.8",
                "maven.compiler.source", "11")));
        assertNull(RuntimeImageMapping.release(properties("maven.compiler.source", "1.8")));
        assertNull(RuntimeImageMapping.release(properties()));
    }

    @Test void proposesEveryMappedIdAtOrAboveTheReleaseInVersionOrder() throws Exception {
        var mapping = RuntimeImageMapping.parse(List.of("25=java-25", "17=java-17", "21 = java-21", "21=temurin-21"));
        assertEquals(List.of("java-21", "temurin-21", "java-25"), mapping.propose(21, false));
        assertEquals(List.of("java-21", "temurin-21"), mapping.propose(21, true));
        IOException none = assertThrows(IOException.class, () -> mapping.propose(26, false));
        assertTrue(none.getMessage().contains("Java 26"));
        assertThrows(IOException.class, () -> mapping.propose(22, true));
        assertTrue(RuntimeImageMapping.preview(List.of("--enable-preview")));
    }

    @Test void warnsWhenConfiguredIdsProvideAnOlderRuntime() throws Exception {
        var mapping = RuntimeImageMapping.parse(List.of("17=java-17", "21=java-21", "25=java-25"));
        assertEquals(List.of(), mapping.warnings(List.of("java-21", "java-25", "custom"), 21, false));
        List<String> warnings = mapping.warnings(List.of("java-17", "custom"), 21, false);
        assertEquals(1, warnings.size());
        assertTrue(warnings.get(0).contains("java-17") && warnings.get(0).contains("Java 21"));
        assertEquals(1, mapping.warnings(List.of("java-21", "java-25"), 21, true).size());
    }

    @Test void rejectsMalformedMappings() {
        assertThrows(IOException.class, () -> RuntimeImageMapping.parse(List.of("java-21")));
        assertThrows(IOException.class, () -> RuntimeImageMapping.parse(List.of("x=java-21")));
        assertThrows(IOException.class, () -> RuntimeImageMapping.parse(List.of("21=java-21", "22=java-21")));
        IOException id = assertThrows(IOException.class, () -> RuntimeImageMapping.parse(List.of("21=Java_21")));
        assertTrue(id.getMessage().startsWith("InvalidRuntimeImageID"));
        assertTrue(assertDoesNotThrow(() -> RuntimeImageMapping.parse(null)).isEmpty());
    }
}
