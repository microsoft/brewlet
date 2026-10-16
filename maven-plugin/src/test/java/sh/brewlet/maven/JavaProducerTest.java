package sh.brewlet.maven;

import static org.junit.jupiter.api.Assertions.*;

import com.fasterxml.jackson.databind.ObjectMapper;
import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.attribute.FileTime;
import java.util.List;
import java.util.jar.Attributes;
import java.util.jar.JarOutputStream;
import java.util.jar.Manifest;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

class JavaProducerTest {
    @TempDir Path temp;

    private Path jar(String filename, String mainClass) throws IOException {
        Manifest manifest = new Manifest();
        manifest.getMainAttributes().put(Attributes.Name.MANIFEST_VERSION, "1.0");
        if (mainClass != null) {
            manifest.getMainAttributes().put(Attributes.Name.MAIN_CLASS, mainClass);
        }
        Path file = temp.resolve(filename);
        try (JarOutputStream ignored = new JarOutputStream(Files.newOutputStream(file), manifest)) {}
        return file;
    }

    @Test void generatesPlainArgvAndPreservesMtime() throws Exception {
        Path jar = jar("input.jar", "example.Main");
        Files.setLastModifiedTime(jar, FileTime.fromMillis(1234000));
        var result = JavaProducer.prepare(new JavaProducer.Inputs(jar, List.of(), null,
                null, null, List.of("-Dmessage=two words"), List.of("java-21")),
                Files.createDirectory(temp.resolve("out")));
        var json = new ObjectMapper().readTree(result.launch().toFile());
        assertEquals(2, json.path("schemaVersion").intValue());
        assertEquals("java", json.at("/process/entrypoint/0").textValue());
        assertEquals("-Dmessage=two words", json.at("/process/args/0").textValue());
        assertEquals("example.Main", json.at("/process/args/3").textValue());
        assertEquals("java-21", json.at("/requires/runtimeImages/0").textValue());
        assertEquals(Files.getLastModifiedTime(jar),
                Files.getLastModifiedTime(result.application().resolve("app.jar")));
        assertFalse(json.has("jvm"));
    }

    @Test void handWrittenDescriptorIsNotRewritten() throws Exception {
        Path descriptor = temp.resolve("custom.json");
        Files.writeString(descriptor, "{\"schemaVersion\":2,\"process\":{\"entrypoint\":[\"java\"]}}");
        byte[] original = Files.readAllBytes(descriptor);
        var result = JavaProducer.prepare(new JavaProducer.Inputs(jar("a.jar", null), List.of(),
                descriptor, null, null, List.of(), List.of()),
                Files.createDirectory(temp.resolve("out")));
        assertEquals(descriptor, result.launch());
        assertArrayEquals(original, Files.readAllBytes(descriptor));
    }

    @Test void requiresExplicitLaunchAuthority() throws Exception {
        var inputs = new JavaProducer.Inputs(jar("a.jar", null), List.of(), null,
                null, null, List.of(), List.of());
        assertThrows(IOException.class, () -> JavaProducer.prepare(inputs,
                Files.createDirectory(temp.resolve("out"))));
    }

    @Test void usesModulePathOnlyInOptionalProducer() throws Exception {
        var prepared = JavaProducer.prepare(new JavaProducer.Inputs(jar("a.jar", null), List.of(),
                null, "example.Main", "example.app", List.of("--enable-preview"), List.of()),
                Files.createDirectory(temp.resolve("out")));
        var json = new ObjectMapper().readTree(prepared.launch().toFile());
        assertEquals("--module-path", json.at("/process/args/1").textValue());
        assertEquals("example.app/example.Main", json.at("/process/args/4").textValue());
        assertEquals("/app/mods", prepared.dependencyTarget());
    }

    @Test void commandPreservesBoundariesAndNeedsPlatforms() throws Exception {
        var prepared = new JavaProducer.Prepared(temp.resolve("launch with spaces.json"),
                temp.resolve("app with spaces"), null, "/app/lib");
        assertThrows(IllegalArgumentException.class, () -> JavaProducer.command("brewlet", "app",
                null, List.of(), List.of(), prepared));
        List<String> command = JavaProducer.command("brewlet", "registry.test/apps/a:v2",
                temp.resolve("oci"), List.of("linux/amd64", "linux/arm64"),
                List.of("registry.test/dep@sha256:" + "a".repeat(64)), prepared);
        assertEquals(2, command.stream().filter("--platform"::equals).count());
        assertTrue(command.contains(prepared.launch().toString()));
        assertEquals("app=" + prepared.application() + ":/app", command.get(command.size() - 1));
        assertFalse(command.contains("manifest"));
    }
}
