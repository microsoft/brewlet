package sh.brewlet.maven;

import java.nio.charset.StandardCharsets;
import java.util.ArrayDeque;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Deque;
import java.util.HashMap;
import java.util.HashSet;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Set;

/**
 * Java evaluation of application-layer entry rules and composed overlay link
 * resolution (specification §6). Entries model tar headers; every violation is
 * {@code LayerPathViolation}, matching the CLI and shim.
 */
final class LayerContract {
    static final char REGULAR = '0', HARDLINK = '1', SYMLINK = '2', DIRECTORY = '5', FIFO = '6';
    private static final String VIOLATION = "LayerPathViolation";

    record Entry(String path, char type, long mode, String data, String link, boolean xattrs) {
        static char type(String name) {
            return switch (name) {
                case "file" -> REGULAR;
                case "directory" -> DIRECTORY;
                case "symlink" -> SYMLINK;
                case "hardlink" -> HARDLINK;
                case "fifo" -> FIFO;
                default -> throw new IllegalArgumentException("unknown fixture tar type");
            };
        }
    }
    private record Node(String path, String link, char type, long mode) {}
    private record Compiled(List<Node> entries, Map<String, Node> nodes) {}
    private record Whiteout(String target, boolean opaque) {}

    /** Applies layers bottom-up as overlay lowers and resolves every visible symlink. */
    static void validateComposition(List<List<Entry>> layers) throws ArtifactContract.Violation {
        Map<String, Node> nodes = new HashMap<>();
        for (List<Entry> layer : layers) {
            Compiled compiled = walk(layer);
            for (Node entry : compiled.entries()) {
                Whiteout w = whiteout(entry.path());
                if (w != null) {
                    removeTree(nodes, w.target(), w.opaque());
                }
            }
            for (String name : compiled.nodes().keySet().stream().sorted().toList()) {
                Node node = compiled.nodes().get(name);
                Node lower = nodes.get(name);
                if (lower != null && (node.type() != DIRECTORY || lower.type() != DIRECTORY)) {
                    removeTree(nodes, name, false);
                }
                nodes.put(name, node);
            }
            validateTreeLinks(nodes);
        }
    }

    private static Compiled walk(List<Entry> entries) throws ArtifactContract.Violation {
        List<Node> info = new ArrayList<>();
        Set<String> regular = new HashSet<>();
        Set<String> seen = new HashSet<>();
        Map<String, String> pending = new LinkedHashMap<>();
        for (Entry entry : entries) {
            String name = archivePath(entry.path());
            if (!seen.add(name)) {
                throw fail("duplicate entry " + ArtifactContract.quote(name));
            }
            if (entry.xattrs() || (entry.mode() & 06000) != 0) {
                throw fail("xattrs or set-id bits forbidden");
            }
            long size = entry.type() == REGULAR ? entry.data().getBytes(StandardCharsets.UTF_8).length : 0;
            Whiteout w = whiteout(name);
            if (w != null) {
                String base = base(w.target());
                if (entry.type() != REGULAR || size != 0 || w.target().equals("app") && !w.opaque()
                        || base.equals(".") || base.isEmpty()) {
                    throw fail("invalid whiteout");
                }
                archivePath(w.target());
            } else {
                long mode = entry.mode();
                switch (entry.type()) {
                    case REGULAR -> {
                        if ((mode & 04) == 0 || (mode & 0111) != 0 && (mode & 01) == 0) {
                            throw fail("file not accessible to arbitrary UID: " + name);
                        }
                        regular.add(name);
                    }
                    case DIRECTORY -> {
                        if ((mode & 05) != 05) {
                            throw fail("directory not accessible: " + name);
                        }
                    }
                    case SYMLINK, HARDLINK -> {
                        String target = confinedLink(name, entry.link(), entry.type() == HARDLINK);
                        if (entry.type() == HARDLINK) {
                            pending.put(name, target);
                        }
                    }
                    default -> throw fail("unsupported entry type " + entry.type());
                }
            }
            info.add(new Node(name, entry.link(), entry.type(), entry.mode()));
        }
        for (var link : pending.entrySet()) {
            if (!regular.contains(link.getValue())) {
                throw fail("hardlink " + link.getKey() + " does not reference same-layer regular file");
            }
        }
        return compile(info);
    }

    // Absent parents are implicit upper directories, not references into a lower
    // layer, so one extracted layer serves every image which reuses it.
    private static Compiled compile(List<Node> info) throws ArtifactContract.Violation {
        Map<String, Node> nodes = new HashMap<>();
        List<Node> entries = new ArrayList<>();
        List<Integer> hardlinks = new ArrayList<>();
        for (Node entry : info) {
            String parent = resolveTree(nodes, ".", dir(entry.path()));
            entry = new Node(join(parent, base(entry.path())), entry.link(), entry.type(), entry.mode());
            archivePath(entry.path());
            ensureTreeParents(nodes, entry.path());
            if (whiteout(entry.path()) != null) {
                entries.add(entry);
                continue;
            }
            if (entry.type() == SYMLINK) {
                confinedLink(entry.path(), entry.link(), false);
            }
            if (entry.type() == HARDLINK) {
                hardlinks.add(entries.size());
            } else {
                Node old = nodes.get(entry.path());
                if (old != null && (entry.type() != DIRECTORY || old.type() != DIRECTORY)) {
                    removeTree(nodes, entry.path(), false);
                }
                nodes.put(entry.path(), entry);
            }
            entries.add(entry);
        }
        for (int i : hardlinks) {
            Node entry = entries.get(i);
            String target = resolveTree(nodes, ".", entry.link());
            Node node = nodes.get(target);
            if (node == null || node.type() != REGULAR) {
                throw fail("hardlink must resolve to a same-layer regular file");
            }
            entries.set(i, new Node(entry.path(), target, entry.type(), entry.mode()));
            removeTree(nodes, entry.path(), false);
            nodes.put(entry.path(), new Node(entry.path(), node.link(), node.type(), node.mode()));
        }
        validateTreeLinks(nodes);
        return new Compiled(entries, nodes);
    }

    private static void validateTreeLinks(Map<String, Node> nodes) throws ArtifactContract.Violation {
        for (var node : nodes.entrySet()) {
            if (node.getValue().type() == SYMLINK) {
                resolveTree(nodes, dir(node.getKey()), node.getValue().link());
            }
        }
    }

    // ".." is applied only after preceding symlinks resolve; cleaning
    // "alias/../file" first would change kernel path semantics.
    private static String resolveTree(Map<String, Node> nodes, String start, String target)
            throws ArtifactContract.Violation {
        if (invalidLink(target)) {
            throw fail("invalid symlink target");
        }
        String cur = start;
        Deque<String> pending = new ArrayDeque<>(Arrays.asList(target.split("/", -1)));
        int links = 0;
        while (!pending.isEmpty()) {
            String part = pending.removeFirst();
            if (part.isEmpty() || part.equals(".")) {
                continue;
            }
            if (part.equals("..")) {
                cur = dir(cur);
                if (!inApp(cur)) {
                    throw fail("link escapes /app");
                }
                continue;
            }
            cur = join(cur, part);
            archivePath(cur);
            Node node = nodes.get(cur);
            if (node != null && node.type() == SYMLINK) {
                if (++links > 40) {
                    throw fail("symlink cycle");
                }
                if (invalidLink(node.link())) {
                    throw fail("invalid symlink target");
                }
                cur = dir(cur);
                String[] parts = node.link().split("/", -1);
                for (int i = parts.length - 1; i >= 0; i--) {
                    pending.addFirst(parts[i]);
                }
            }
        }
        return cur;
    }

    private static void removeTree(Map<String, Node> nodes, String name, boolean childrenOnly) {
        nodes.keySet().removeIf(p -> p.equals(name) && !childrenOnly || p.startsWith(name + "/"));
    }

    private static void ensureTreeParents(Map<String, Node> nodes, String name) throws ArtifactContract.Violation {
        List<String> dirs = new ArrayList<>();
        for (String parent = dir(name); !parent.equals("."); parent = dir(parent)) {
            dirs.add(parent);
        }
        for (int i = dirs.size() - 1; i >= 0; i--) {
            Node node = nodes.get(dirs.get(i));
            if (node == null) {
                nodes.put(dirs.get(i), new Node(dirs.get(i), "", DIRECTORY, 0755));
            } else if (node.type() != DIRECTORY) {
                throw fail("non-directory layer parent " + dirs.get(i));
            }
        }
    }

    private static String archivePath(String s) throws ArtifactContract.Violation {
        if (s.isEmpty() || s.startsWith("/") || s.indexOf(0) >= 0 || s.contains("\\")) {
            throw fail("invalid path " + ArtifactContract.quote(s));
        }
        String[] parts = (s.endsWith("/") ? s.substring(0, s.length() - 1) : s).split("/", -1);
        for (int i = 0; i < parts.length; i++) {
            if (parts[i].equals("..")) {
                throw fail("parent traversal " + ArtifactContract.quote(s));
            }
            if (i < parts.length - 1 && parts[i].startsWith(".wh.")) {
                throw fail("whiteout name used as directory " + ArtifactContract.quote(s));
            }
        }
        s = clean(s);
        if (!inApp(s)) {
            throw fail("outside app: " + ArtifactContract.quote(s));
        }
        return s;
    }

    private static String confinedLink(String name, String target, boolean hard) throws ArtifactContract.Violation {
        if (target.isEmpty() || invalidLink(target)) {
            throw fail("invalid link " + ArtifactContract.quote(target));
        }
        String dest = clean(hard ? target : join(dir(name), target));
        if (!inApp(dest)) {
            throw fail("escaping link " + ArtifactContract.quote(target));
        }
        return dest;
    }

    private static Whiteout whiteout(String name) {
        String base = base(name);
        if (base.equals(".wh..wh..opq")) {
            return new Whiteout(dir(name), true);
        }
        if (base.startsWith(".wh.")) {
            return new Whiteout(join(dir(name), base.substring(4)), false);
        }
        return null;
    }

    private static boolean invalidLink(String target) {
        return target.startsWith("/") || target.indexOf(0) >= 0 || target.contains("\\");
    }

    private static boolean inApp(String s) {
        return s.equals("app") || s.startsWith("app/");
    }

    private static ArtifactContract.Violation fail(String detail) {
        return new ArtifactContract.Violation(VIOLATION, detail);
    }

    // Slash-separated (not host filesystem) path operations with POSIX lexical semantics.
    static String clean(String path) {
        if (path.isEmpty()) {
            return ".";
        }
        boolean rooted = path.startsWith("/");
        Deque<String> out = new ArrayDeque<>();
        for (String part : path.split("/")) {
            if (part.isEmpty() || part.equals(".")) {
                continue;
            }
            if (part.equals("..")) {
                if (!out.isEmpty() && !out.peekLast().equals("..")) {
                    out.removeLast();
                } else if (!rooted) {
                    out.addLast("..");
                }
                continue;
            }
            out.addLast(part);
        }
        String joined = String.join("/", out);
        return rooted ? "/" + joined : joined.isEmpty() ? "." : joined;
    }

    static String dir(String path) {
        return clean(path.substring(0, path.lastIndexOf('/') + 1));
    }

    static String base(String path) {
        if (path.isEmpty()) {
            return ".";
        }
        while (path.endsWith("/")) {
            path = path.substring(0, path.length() - 1);
        }
        return path.isEmpty() ? "/" : path.substring(path.lastIndexOf('/') + 1);
    }

    static String join(String a, String b) {
        if (a.isEmpty() && b.isEmpty()) {
            return "";
        }
        return clean(a.isEmpty() ? b : b.isEmpty() ? a : a + "/" + b);
    }

    private LayerContract() {}
}
