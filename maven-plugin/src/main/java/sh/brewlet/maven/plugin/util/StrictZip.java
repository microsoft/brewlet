// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin.util;

import java.io.IOException;
import java.nio.ByteBuffer;
import java.nio.ByteOrder;
import java.nio.charset.StandardCharsets;
import java.util.HashSet;
import java.util.Set;

/** Validates the ZIP metadata that JarFile does not expose (notably Unix links). */
final class StrictZip {
    private StrictZip() {}

    static void validate(byte[] bytes) throws IOException {
        ByteBuffer zip = ByteBuffer.wrap(bytes).order(ByteOrder.LITTLE_ENDIAN);
        int end = -1;
        for (int p = bytes.length - 22; p >= Math.max(0, bytes.length - 65557); p--) {
            if (zip.getInt(p) == 0x06054b50 && p + 22 + u16(zip, p + 20) == bytes.length) {
                end = p;
                break;
            }
        }
        if (end < 0) throw invalid("missing ZIP end record");
        int count = u16(zip, end + 10);
        long size = u32(zip, end + 12);
        long offset = u32(zip, end + 16);
        if (u16(zip, end + 4) != 0 || u16(zip, end + 6) != 0
                || u16(zip, end + 8) != count || count == 65535
                || offset == 0xffffffffL || size == 0xffffffffL) {
            throw invalid("split/ZIP64 archives are not supported for layered extraction");
        }
        if (offset + size != end || offset > bytes.length || (count == 0 && offset != 0)) {
            throw invalid("ambiguous central directory");
        }
        Set<String> names = new HashSet<>();
        Set<String> files = new HashSet<>();
        Set<Integer> locals = new HashSet<>();
        int pos = (int) offset;
        long previousEnd = 0;
        // Local records need not have the same order as the central directory.
        java.util.TreeMap<Long, Long> extents = new java.util.TreeMap<>();
        for (int i = 0; i < count; i++) {
            if (pos + 46 > end || zip.getInt(pos) != 0x02014b50) throw invalid("invalid central entry");
            int length = u16(zip, pos + 28);
            int extra = u16(zip, pos + 30);
            int comment = u16(zip, pos + 32);
            if ((long) pos + 46 + length + extra + comment > end) throw invalid("truncated central entry");
            String name = new String(bytes, pos + 46, length, StandardCharsets.UTF_8);
            if (!java.util.Arrays.equals(name.getBytes(StandardCharsets.UTF_8),
                    java.util.Arrays.copyOfRange(bytes, pos + 46, pos + 46 + length))) {
                throw invalid("entry name is not UTF-8");
            }
            validateName(name);
            String canonical = name.endsWith("/") ? name.substring(0, name.length() - 1) : name;
            if (!names.add(canonical)) throw invalid("duplicate file/directory entry: " + name);
            if (!name.endsWith("/")) files.add(name);
            int flags = u16(zip, pos + 8);
            if ((flags & 1) != 0 || u16(zip, pos + 34) != 0) throw invalid("encrypted/split entry: " + name);
            validateAttributes(name, u16(zip, pos + 4) >>> 8, u32(zip, pos + 38),
                    u32(zip, pos + 24), u32(zip, pos + 16));
            long local = u32(zip, pos + 42);
            long compressed = u32(zip, pos + 20);
            long uncompressed = u32(zip, pos + 24);
            long crc = u32(zip, pos + 16);
            if (u32(zip, pos + 24) == 0xffffffffL || compressed == 0xffffffffL
                    || local + 30 > offset || local > Integer.MAX_VALUE || !locals.add((int) local)) {
                throw invalid("invalid/ZIP64 local entry: " + name);
            }
            int lp = (int) local;
            int localLength = u16(zip, lp + 26);
            int localExtra = u16(zip, lp + 28);
            long dataStart = local + 30 + localLength + localExtra;
            if (zip.getInt(lp) != 0x04034b50 || dataStart + compressed > offset
                    || localLength != length || u16(zip, lp + 6) != flags
                    || u16(zip, lp + 8) != u16(zip, pos + 10)
                    || !java.util.Arrays.equals(
                            java.util.Arrays.copyOfRange(bytes, lp + 30, lp + 30 + localLength),
                            java.util.Arrays.copyOfRange(bytes, pos + 46, pos + 46 + length))) {
                throw invalid("central/local entry mismatch: " + name);
            }
            long dataEnd = dataStart + compressed;
            if ((flags & 8) == 0) {
                if (u32(zip, lp + 14) != crc || u32(zip, lp + 18) != compressed
                        || u32(zip, lp + 22) != uncompressed) {
                    throw invalid("central/local size or CRC mismatch: " + name);
                }
            } else {
                int descriptor = (int) dataEnd;
                if (dataEnd + 12 > offset) throw invalid("missing data descriptor: " + name);
                if (zip.getInt(descriptor) == 0x08074b50) descriptor += 4;
                if ((long) descriptor + 12 > offset || u32(zip, descriptor) != crc
                        || u32(zip, descriptor + 4) != compressed || u32(zip, descriptor + 8) != uncompressed) {
                    throw invalid("invalid data descriptor: " + name);
                }
                dataEnd = descriptor + 12L;
            }
            extents.put(local, dataEnd);
            pos += 46 + length + extra + comment;
        }
        if (pos != end) throw invalid("central entry count mismatch");
        if (!extents.isEmpty() && extents.firstKey() != 0) {
            throw invalid("prefixed/fully executable archives are not supported; disable executable repackaging");
        }
        for (var extent : extents.entrySet()) {
            if (extent.getKey() != previousEnd) throw invalid("overlapping or unindexed ZIP entries");
            previousEnd = extent.getValue();
        }
        if (previousEnd != offset) throw invalid("unindexed data before central directory");
        for (String name : names) {
            int slash = name.indexOf('/');
            while (slash >= 0) {
                if (files.contains(name.substring(0, slash))) throw invalid("file/directory collision: " + name);
                slash = name.indexOf('/', slash + 1);
            }
        }
    }

    static void validateName(String name) throws IOException {
        if (name.isEmpty() || name.startsWith("/") || name.indexOf('\\') >= 0
                || name.indexOf(':') >= 0 || name.chars().anyMatch(c -> c < 32 || c == 127)) {
            throw invalid("unsafe entry path: " + name);
        }
        String trimmed = name.endsWith("/") ? name.substring(0, name.length() - 1) : name;
        for (String part : trimmed.split("/", -1)) {
            if (part.isEmpty() || part.equals(".") || part.equals("..")) {
                throw invalid("unsafe entry path: " + name);
            }
        }
    }

    private static void validateAttributes(String name, int creator, long attributes, long size, long crc)
            throws IOException {
        boolean directory = name.endsWith("/");
        boolean dosDirectory = (attributes & 0x10) != 0;
        if ((attributes & (0x08 | 0x40 | 0x400)) != 0) {
            throw invalid("volume, device, or reparse-point entry: " + name);
        }
        if ((dosDirectory && !directory) || (directory && (size != 0 || crc != 0))) {
            throw invalid("ambiguous directory attributes or content: " + name);
        }
        // Only Unix/macOS creators define the high word as a POSIX mode.
        if (creator != 3 && creator != 19) return;
        int unixMode = (int) (attributes >>> 16);
        // Older Maven archivers emitted -1 (unknown permissions) for metadata
        // directories, e.g. jakarta.transaction-api 2.0.1's META-INF/maven/.
        if (unixMode == 0xffff && directory && dosDirectory) return;
        int type = unixMode & 0170000;
        if (type != 0 && type != 0100000 && type != 0040000) {
            throw invalid("symlink or special entry: " + name);
        }
        if ((type == 0040000 && !directory) || (type == 0100000 && directory)) {
            throw invalid("ambiguous entry type: " + name);
        }
    }

    private static int u16(ByteBuffer bytes, int offset) { return Short.toUnsignedInt(bytes.getShort(offset)); }
    private static long u32(ByteBuffer bytes, int offset) { return Integer.toUnsignedLong(bytes.getInt(offset)); }
    private static IOException invalid(String message) { return new IOException("Invalid layered JAR: " + message); }
}
