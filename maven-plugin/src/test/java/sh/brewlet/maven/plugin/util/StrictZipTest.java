// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin.util;

import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.Arguments;
import org.junit.jupiter.params.provider.MethodSource;

import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.nio.ByteBuffer;
import java.nio.ByteOrder;
import java.util.stream.Stream;
import java.util.zip.ZipEntry;
import java.util.zip.ZipOutputStream;

import static org.junit.jupiter.api.Assertions.assertDoesNotThrow;
import static org.junit.jupiter.api.Assertions.assertThrows;

class StrictZipTest {
    @ParameterizedTest
    @MethodSource("validAttributes")
    void recognizesCreatorSpecificRegularFilesAndDirectories(int creator, long attributes, String name)
            throws Exception {
        byte[] zip = archive(creator, attributes, name, new byte[0]);
        assertDoesNotThrow(() -> StrictZip.validate(zip));
    }

    static Stream<Arguments> validAttributes() {
        return Stream.of(
                // Exact legacy Maven metadata in jakarta.transaction-api-2.0.1.jar.
                Arguments.of(3, 0xffff0010L, "META-INF/maven/"),
                Arguments.of(3, 0x41ed0010L, "directory/"),
                Arguments.of(3, 0x41ed0000L, "directory/"),
                Arguments.of(3, 0x01ed0010L, "directory/"),
                Arguments.of(3, 0x81a40000L, "resource"),
                Arguments.of(19, 0x41ed0010L, "directory/"),
                Arguments.of(19, 0x81a40000L, "resource"),
                Arguments.of(0, 0x10L, "directory/"),
                Arguments.of(0, 0L, "directory/"),
                Arguments.of(0, 0x40000000L, "resource"),
                Arguments.of(10, 0x20L, "resource"));
    }

    @ParameterizedTest
    @MethodSource("invalidAttributes")
    void rejectsLinksSpecialEntriesAndInconsistentDirectoryMetadata(
            int creator, long attributes, String name, byte[] content) throws Exception {
        byte[] zip = archive(creator, attributes, name, content);
        assertThrows(IOException.class, () -> StrictZip.validate(zip));
    }

    static Stream<Arguments> invalidAttributes() {
        byte[] empty = new byte[0];
        return Stream.of(
                Arguments.of(3, 0xffff0010L, "resource", empty),
                Arguments.of(3, 0xffff0000L, "directory/", empty),
                Arguments.of(3, 0xffff0010L, "directory/", new byte[]{1}),
                Arguments.of(3, 0xa1ff0000L, "symlink", empty),
                Arguments.of(3, 0xa1ff0010L, "symlink/", empty),
                Arguments.of(19, 0xa1ff0000L, "symlink", empty),
                Arguments.of(3, 0xc1ff0010L, "socket/", empty),
                Arguments.of(3, 0x11b60000L, "fifo", empty),
                Arguments.of(3, 0x21b60000L, "character-device", empty),
                Arguments.of(3, 0x61b60000L, "block-device", empty),
                Arguments.of(3, 0x81a40010L, "directory/", empty),
                Arguments.of(3, 0x41ed0000L, "resource", empty),
                Arguments.of(0, 0x10L, "resource", empty),
                Arguments.of(0, 0x10L, "directory/", new byte[]{1}),
                Arguments.of(0, 0x08L, "volume-label", empty),
                Arguments.of(10, 0x400L, "reparse-point", empty),
                Arguments.of(10, 0x40L, "device", empty));
    }

    private static byte[] archive(int creator, long attributes, String name, byte[] content)
            throws IOException {
        ByteArrayOutputStream bytes = new ByteArrayOutputStream();
        try (ZipOutputStream zip = new ZipOutputStream(bytes)) {
            ZipEntry entry = new ZipEntry(name);
            if (content.length == 0) {
                entry.setMethod(ZipEntry.STORED);
                entry.setSize(0);
                entry.setCrc(0);
            }
            zip.putNextEntry(entry);
            zip.write(content);
            zip.closeEntry();
        }
        byte[] archive = bytes.toByteArray();
        ByteBuffer zip = ByteBuffer.wrap(archive).order(ByteOrder.LITTLE_ENDIAN);
        int end = archive.length - 22;
        int central = zip.getInt(end + 16);
        zip.putShort(central + 4, (short) (creator << 8 | 20));
        zip.putInt(central + 38, (int) attributes);
        return archive;
    }
}
