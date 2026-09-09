// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin.util;

import java.io.ByteArrayOutputStream;
import java.io.File;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.attribute.FileTime;
import java.util.ArrayList;
import java.util.HashSet;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.Set;
import java.util.TreeMap;
import java.util.jar.Attributes;
import java.util.jar.JarFile;
import java.util.jar.Manifest;
import java.util.zip.CRC32;
import java.util.zip.ZipEntry;
import java.util.zip.ZipOutputStream;

/** One application payload shared by config inference, publication, and CDS training. */
public record PreparedApplication(File jar, List<LayerBuilder.Dep> dependencies, boolean springBoot) {
    public static final FileTime CANONICAL_MTIME = FileTime.fromMillis(946684800000L);
    private static final String CLASSES = "BOOT-INF/classes/";
    private static final String LIB = "BOOT-INF/lib/";
    private static final Set<String> LAUNCHERS = Set.of(
            "org.springframework.boot.loader.JarLauncher",
            "org.springframework.boot.loader.launch.JarLauncher");

    public PreparedApplication {
        dependencies = List.copyOf(dependencies);
    }

    public static PreparedApplication prepare(File source, boolean layered, Path directory,
                                               List<LayerBuilder.Dep> runtimeDeps) throws IOException {
        if (!layered) return new PreparedApplication(source, runtimeDeps, false);
        byte[] sourceBytes = Files.readAllBytes(source.toPath());
        StrictZip.validate(sourceBytes);
        try (JarFile archive = new JarFile(source)) {
            Manifest manifest = archive.getManifest();
            Attributes attrs = manifest == null ? new Attributes() : manifest.getMainAttributes();
            List<String> names = archive.stream().map(ZipEntry::getName).toList();
            boolean boot = attrs.getValue("Start-Class") != null
                    || names.stream().anyMatch(n -> n.startsWith("BOOT-INF/"))
                    || (attrs.getValue(Attributes.Name.MAIN_CLASS) != null
                        && LAUNCHERS.contains(attrs.getValue(Attributes.Name.MAIN_CLASS)));
            if (names.stream().anyMatch(n -> n.startsWith("WEB-INF/"))) {
                throw unsupported("WAR/WEB-INF layout");
            }
            if (!boot) {
                if (!JarInspector.embeddedJars(source).isEmpty()) {
                    throw unsupported("unrecognized nested-JAR layout");
                }
                return new PreparedApplication(source, runtimeDeps, false);
            }
            String start = attrs.getValue("Start-Class");
            if (attrs.getValue(Attributes.Name.MAIN_CLASS) == null
                    || !LAUNCHERS.contains(attrs.getValue(Attributes.Name.MAIN_CLASS))
                    || start == null || start.isBlank()) {
                throw unsupported("custom/PropertiesLauncher or missing Start-Class; only Spring Boot JarLauncher is supported");
            }
            checkAttribute(attrs, "Spring-Boot-Classes", CLASSES);
            checkAttribute(attrs, "Spring-Boot-Lib", LIB);
            checkAttribute(attrs, "Spring-Boot-Classpath-Index", "BOOT-INF/classpath.idx");
            checkAttribute(attrs, "Spring-Boot-Layers-Index", "BOOT-INF/layers.idx");
            if (attrs.getValue(Attributes.Name.CLASS_PATH) != null) {
                throw unsupported("manifest Class-Path (external dependencies are not packaged)");
            }
            TreeMap<String, byte[]> application = new TreeMap<>();
            Map<String, byte[]> libraries = new LinkedHashMap<>();
            for (var entry : archive.stream().toList()) {
                String name = entry.getName();
                if (entry.isDirectory()) {
                    if (name.startsWith(CLASSES) && name.length() > CLASSES.length()) {
                        addApplication(application, name.substring(CLASSES.length()), new byte[0]);
                    } else if (name.startsWith("META-INF/")) {
                        addApplication(application, name, new byte[0]);
                    }
                    continue;
                }
                byte[] content;
                try (var in = archive.getInputStream(entry)) {
                    content = in.readAllBytes();
                } catch (SecurityException e) {
                    throw new IOException("Invalid JAR signature in " + name, e);
                }
                CRC32 crc = new CRC32();
                crc.update(content);
                if (content.length != entry.getSize() || crc.getValue() != entry.getCrc()) {
                    throw new IOException("Invalid ZIP size/CRC for " + name);
                }
                if (name.startsWith(CLASSES)) {
                    String target = name.substring(CLASSES.length());
                    if (target.equals("module-info.class") || target.endsWith("/module-info.class")
                            || target.toLowerCase(Locale.ROOT).endsWith(".jar")) {
                        throw unsupported("nested module/JAR within application classes: " + name);
                    }
                    addApplication(application, target, content);
                } else if (name.startsWith(LIB)) {
                    String filename = name.substring(LIB.length());
                    if (filename.contains("/") || !filename.endsWith(".jar")
                            || filename.indexOf(' ') >= 0 || filename.indexOf('*') >= 0) {
                        throw unsupported("non-flat or ambiguous library path: " + name);
                    }
                    if (entry.getComment() != null && entry.getComment().startsWith("UNPACK:")) {
                        throw unsupported("requiresUnpack library: " + name);
                    }
                    StrictZip.validate(content);
                    libraries.put(name, content);
                } else if (name.equals("META-INF/MANIFEST.MF") || isSignature(name)
                        || name.startsWith("org/springframework/boot/loader/")
                        || name.equals("BOOT-INF/classpath.idx") || name.equals("BOOT-INF/layers.idx")) {
                    // The loader and its indexes/signatures describe the original, not the thin JAR.
                } else if (name.startsWith("META-INF/services/")
                        && new String(content, StandardCharsets.UTF_8).lines()
                            .anyMatch(line -> line.strip().startsWith("org.springframework.boot.loader."))) {
                    // For example, Boot's nested-filesystem provider cannot run without its loader.
                    if (new String(content, StandardCharsets.UTF_8).lines()
                            .map(String::strip).anyMatch(line -> !line.isEmpty() && !line.startsWith("#")
                                    && !line.startsWith("org.springframework.boot.loader."))) {
                        throw unsupported("mixed Boot-loader/application service provider: " + name);
                    }
                } else if (name.startsWith("META-INF/") && !name.endsWith(".class")) {
                    addApplication(application, name, content);
                } else {
                    throw unsupported("entry outside the standard Boot application/library layout: " + name);
                }
            }
            if (!application.containsKey(start.replace('.', '/') + ".class")) {
                throw unsupported("Start-Class " + start + " is not in BOOT-INF/classes");
            }
            List<String> order = dependencyOrder(archive, libraries.keySet());
            byte[] thinManifest = thinManifest(manifest, application.keySet(), start);
            addEntry(application, "META-INF/MANIFEST.MF", thinManifest);
            // Package scanners and resource resolvers enumerate directory URLs,
            // not just individual files. Preserve empty directories and synthesize
            // parents even when the source ZIP omitted explicit directory entries.
            for (String name : List.copyOf(application.keySet())) {
                for (int slash = name.indexOf('/'); slash >= 0; slash = name.indexOf('/', slash + 1)) {
                    addEntry(application, name.substring(0, slash + 1), new byte[0]);
                }
            }
            byte[] thinJar = deterministicJar(application);
            Path root = directory.toAbsolutePath().normalize();
            Path app = root.resolve(source.getName());
            if (source.toPath().toAbsolutePath().normalize().startsWith(root)
                    || (Files.exists(app) && Files.isSameFile(app, source.toPath()))) {
                throw new IOException("Prepared application directory must not contain or overwrite source JAR: " + source);
            }
            createDirectory(root);
            if (source.toPath().toRealPath().startsWith(root.toRealPath())) {
                throw new IOException("Prepared application directory must not contain source JAR: " + source);
            }
            writeRegularFile(app, thinJar);
            Path lib = root.resolve("lib");
            createDirectory(lib);
            List<LayerBuilder.Dep> dependencies = new ArrayList<>();
            for (String name : order) {
                String filename = name.substring(LIB.length());
                Path path = lib.resolve(filename);
                writeRegularFile(path, libraries.get(name));
                dependencies.add(new LayerBuilder.Dep(filename, path, filename.contains("-SNAPSHOT")));
            }
            LayerBuilder.validateDependencies(dependencies);
            return new PreparedApplication(app.toFile(), dependencies, true);
        }
    }

    /** Explicit entries preserve Boot's classpath.idx order even when layers are split. */
    public List<String> classPath() {
        List<String> result = new ArrayList<>();
        result.add(jar.getName());
        for (var dep : dependencies) result.add("lib/" + dep.fileName());
        return result;
    }

    private static List<String> dependencyOrder(JarFile archive, Set<String> libraries) throws IOException {
        var index = archive.getJarEntry("BOOT-INF/classpath.idx");
        if (index == null) return List.copyOf(libraries);
        String text;
        try (var in = archive.getInputStream(index)) {
            text = new String(in.readAllBytes(), StandardCharsets.UTF_8);
        }
        List<String> order = new ArrayList<>();
        Set<String> seen = new HashSet<>();
        for (String line : text.lines().toList()) {
            if (!line.startsWith("- \"") || !line.endsWith("\"") || line.length() < 5) {
                throw unsupported("malformed BOOT-INF/classpath.idx; expected - \"BOOT-INF/lib/name.jar\"");
            }
            String name = line.substring(3, line.length() - 1);
            if (!libraries.contains(name) || !seen.add(name)) {
                throw unsupported("unknown/duplicate classpath.idx entry: " + name);
            }
            order.add(name);
        }
        if (!seen.equals(libraries)) throw unsupported("classpath.idx does not list every packaged dependency exactly once");
        return order;
    }

    private static void addApplication(Map<String, byte[]> files, String name, byte[] content) throws IOException {
        StrictZip.validateName(name);
        if (isSignature(name)) return;
        if (name.equalsIgnoreCase("META-INF/MANIFEST.MF")) {
            throw unsupported("application manifest conflicts with the outer Boot manifest");
        }
        addEntry(files, name, content);
    }

    private static void addEntry(Map<String, byte[]> files, String name, byte[] content) throws IOException {
        boolean directory = name.endsWith("/");
        String base = directory ? name.substring(0, name.length() - 1) : name;
        if (files.containsKey(directory ? base : base + "/")) {
            throw unsupported("relocated file/directory collision: " + name);
        }
        if (files.containsKey(name)) {
            if (directory) return;
            throw unsupported("ambiguous relocated application entry: " + name);
        }
        for (int slash = base.indexOf('/'); slash >= 0; slash = base.indexOf('/', slash + 1)) {
            if (files.containsKey(base.substring(0, slash))) {
                throw unsupported("relocated file/directory collision: " + name);
            }
        }
        if (!directory && files.keySet().stream().anyMatch(other -> other.startsWith(base + "/"))) {
            throw unsupported("relocated file/directory collision: " + name);
        }
        files.put(name, content);
    }

    private static boolean isSignature(String name) {
        String upper = name.toUpperCase(Locale.ROOT);
        if (!upper.startsWith("META-INF/") || upper.substring(9).contains("/")) return false;
        return upper.endsWith(".SF") || upper.endsWith(".RSA") || upper.endsWith(".DSA")
                || upper.endsWith(".EC") || upper.startsWith("META-INF/SIG-")
                || upper.equals("META-INF/INDEX.LIST");
    }

    private static byte[] thinManifest(Manifest original, Set<String> files, String start) throws IOException {
        ByteArrayOutputStream out = new ByteArrayOutputStream();
        writeHeader(out, "Manifest-Version", "1.0");
        TreeMap<String, String> attrs = cleanAttributes(original.getMainAttributes());
        attrs.remove("Manifest-Version");
        attrs.put("Main-Class", start);
        for (var attr : attrs.entrySet()) writeHeader(out, attr.getKey(), attr.getValue());
        out.write("\r\n".getBytes(StandardCharsets.UTF_8));
        TreeMap<String, Attributes> entries = new TreeMap<>();
        for (var entry : original.getEntries().entrySet()) {
            String name = entry.getKey();
            if (name.startsWith(CLASSES)) name = name.substring(CLASSES.length());
            if (files.contains(name) || (name.endsWith("/") && hasPrefix(files, name))) {
                if (entries.putIfAbsent(name, entry.getValue()) != null) {
                    throw unsupported("ambiguous manifest section: " + name);
                }
            }
        }
        for (var section : entries.entrySet()) {
            TreeMap<String, String> values = cleanAttributes(section.getValue());
            if (values.isEmpty()) continue;
            writeHeader(out, "Name", section.getKey());
            for (var attr : values.entrySet()) writeHeader(out, attr.getKey(), attr.getValue());
            out.write("\r\n".getBytes(StandardCharsets.UTF_8));
        }
        return out.toByteArray();
    }

    private static boolean hasPrefix(Set<String> files, String prefix) {
        return files.stream().anyMatch(f -> f.startsWith(prefix));
    }

    private static TreeMap<String, String> cleanAttributes(Attributes original) {
        TreeMap<String, String> values = new TreeMap<>(String.CASE_INSENSITIVE_ORDER);
        for (var attr : original.entrySet()) {
            String key = attr.getKey().toString();
            String lower = key.toLowerCase(Locale.ROOT);
            if (!lower.startsWith("spring-boot-") && !lower.equals("start-class")
                    && !lower.equals("signature-version") && !lower.contains("-digest")) {
                values.put(key, attr.getValue().toString());
            }
        }
        return values;
    }

    private static void writeHeader(ByteArrayOutputStream out, String key, String value) throws IOException {
        byte[] bytes = (key + ": " + value).getBytes(StandardCharsets.UTF_8);
        for (int offset = 0; offset < bytes.length;) {
            if (offset > 0) out.write(' ');
            int size = Math.min(offset == 0 ? 70 : 69, bytes.length - offset);
            out.write(bytes, offset, size);
            out.write("\r\n".getBytes(StandardCharsets.UTF_8));
            offset += size;
        }
    }

    private static byte[] deterministicJar(TreeMap<String, byte[]> files) throws IOException {
        ByteArrayOutputStream bytes = new ByteArrayOutputStream();
        try (ZipOutputStream zip = new ZipOutputStream(bytes)) {
            // The manifest must be first for JarInputStream consumers.
            List<String> names = new ArrayList<>(files.keySet());
            names.remove("META-INF/MANIFEST.MF");
            names.add(0, "META-INF/MANIFEST.MF");
            for (String name : names) {
                byte[] content = files.get(name);
                ZipEntry entry = new ZipEntry(name);
                entry.setTimeLocal(java.time.LocalDateTime.of(2000, 1, 1, 0, 0));
                entry.setMethod(ZipEntry.STORED);
                entry.setSize(content.length);
                CRC32 crc = new CRC32();
                crc.update(content);
                entry.setCrc(crc.getValue());
                zip.putNextEntry(entry);
                zip.write(content);
                zip.closeEntry();
            }
        }
        return bytes.toByteArray();
    }

    private static void writeRegularFile(Path path, byte[] content) throws IOException {
        if (Files.isSymbolicLink(path)) throw new IOException("Refusing symlink staging path: " + path);
        if (Files.exists(path) && !Files.isRegularFile(path)) {
            throw new IOException("Refusing non-regular staging path: " + path);
        }
        // Replace the directory entry, not the inode: an old hard link must not mutate an input.
        Files.deleteIfExists(path);
        Files.write(path, content, java.nio.file.StandardOpenOption.CREATE_NEW, java.nio.file.StandardOpenOption.WRITE);
        Files.setLastModifiedTime(path, CANONICAL_MTIME);
    }

    private static void createDirectory(Path path) throws IOException {
        if (Files.isSymbolicLink(path)) throw new IOException("Refusing symlink staging directory: " + path);
        Files.createDirectories(path);
    }

    private static void checkAttribute(Attributes attrs, String key, String expected) throws IOException {
        String value = attrs.getValue(key);
        if (value != null && !value.equals(expected)) throw unsupported("custom " + key + ": " + value);
    }

    private static IOException unsupported(String detail) {
        return new IOException("Unsupported layered application: " + detail
                + ". Use a standard Spring Boot JAR (JarLauncher), disable layered mode for java -jar,"
                + " or supply a genuinely thin JAR with its runtime dependencies.");
    }
}
