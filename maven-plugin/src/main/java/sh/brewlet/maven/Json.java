package sh.brewlet.maven;

import java.util.ArrayList;
import java.util.HashSet;
import java.util.List;
import java.util.Set;

/**
 * Minimal RFC 8259 tree which keeps member order, duplicate members and raw value
 * text. The wire contract depends on all three, which generic object mappers discard.
 */
final class Json {
    enum Kind { OBJECT, ARRAY, STRING, NUMBER, BOOLEAN, NULL }
    record Member(String key, Json value) {}

    final Kind kind;
    final String text;
    final List<Member> members;
    final List<Json> items;
    final String raw;

    private Json(Kind kind, String text, List<Member> members, List<Json> items, String raw) {
        this.kind = kind;
        this.text = text;
        this.members = members;
        this.items = items;
        this.raw = raw;
    }

    /** Parses exactly one value; strict parsing also rejects duplicate object members. */
    static Json parse(String source, boolean strict) {
        Parser parser = new Parser(source, strict);
        parser.space();
        Json value = parser.value(0);
        parser.space();
        if (parser.pos != source.length()) {
            throw new IllegalArgumentException("trailing JSON data");
        }
        return value;
    }

    /** Last member with exactly this key, matching how decoders resolve tolerated duplicates. */
    Json get(String key) {
        Json found = null;
        if (members != null) {
            for (Member member : members) {
                if (member.key().equals(key)) {
                    found = member.value();
                }
            }
        }
        return found;
    }

    boolean containsNull() {
        return switch (kind) {
            case NULL -> true;
            case OBJECT -> members.stream().anyMatch(member -> member.value().containsNull());
            case ARRAY -> items.stream().anyMatch(Json::containsNull);
            default -> false;
        };
    }

    private static final class Parser {
        private final String s;
        private final boolean strict;
        private int pos;

        Parser(String s, boolean strict) {
            this.s = s;
            this.strict = strict;
        }

        private IllegalArgumentException error(String message) {
            return new IllegalArgumentException(message + " at offset " + pos);
        }

        void space() {
            while (pos < s.length() && " \t\r\n".indexOf(s.charAt(pos)) >= 0) {
                pos++;
            }
        }

        Json value(int depth) {
            if (depth > 10000) {
                throw error("exceeded max depth");
            }
            if (pos >= s.length()) {
                throw error("unexpected end of JSON input");
            }
            int start = pos;
            char c = s.charAt(pos);
            if (c == '{') {
                pos++;
                List<Member> members = new ArrayList<>();
                Set<String> seen = new HashSet<>();
                space();
                if (peek('}')) {
                    return new Json(Kind.OBJECT, null, members, null, s.substring(start, pos));
                }
                while (true) {
                    space();
                    if (pos >= s.length() || s.charAt(pos) != '"') {
                        throw error("expected object key");
                    }
                    String key = string();
                    if (strict && !seen.add(key)) {
                        throw new IllegalArgumentException("duplicate JSON member \"" + key + "\"");
                    }
                    space();
                    expect(':');
                    space();
                    members.add(new Member(key, value(depth + 1)));
                    space();
                    if (peek('}')) {
                        return new Json(Kind.OBJECT, null, members, null, s.substring(start, pos));
                    }
                    expect(',');
                }
            }
            if (c == '[') {
                pos++;
                List<Json> items = new ArrayList<>();
                space();
                if (peek(']')) {
                    return new Json(Kind.ARRAY, null, null, items, s.substring(start, pos));
                }
                while (true) {
                    space();
                    items.add(value(depth + 1));
                    space();
                    if (peek(']')) {
                        return new Json(Kind.ARRAY, null, null, items, s.substring(start, pos));
                    }
                    expect(',');
                }
            }
            if (c == '"') {
                String text = string();
                return new Json(Kind.STRING, text, null, null, s.substring(start, pos));
            }
            for (String literal : List.of("true", "false", "null")) {
                if (s.startsWith(literal, pos)) {
                    pos += literal.length();
                    return new Json(literal.equals("null") ? Kind.NULL : Kind.BOOLEAN, literal, null, null, literal);
                }
            }
            if (c == '-' || c >= '0' && c <= '9') {
                return number();
            }
            throw error("invalid character '" + c + "'");
        }

        private boolean peek(char c) {
            if (pos < s.length() && s.charAt(pos) == c) {
                pos++;
                return true;
            }
            return false;
        }

        private void expect(char c) {
            if (!peek(c)) {
                throw error("expected '" + c + "'");
            }
        }

        private Json number() {
            int start = pos;
            peek('-');
            if (peek('0')) {
                // A leading zero is complete by itself.
            } else if (!digits()) {
                throw error("invalid number");
            }
            if (peek('.') && !digits()) {
                throw error("invalid number");
            }
            if (pos < s.length() && (s.charAt(pos) == 'e' || s.charAt(pos) == 'E')) {
                pos++;
                if (!peek('+')) {
                    peek('-');
                }
                if (!digits()) {
                    throw error("invalid number");
                }
            }
            String text = s.substring(start, pos);
            return new Json(Kind.NUMBER, text, null, null, text);
        }

        private boolean digits() {
            int start = pos;
            while (pos < s.length() && s.charAt(pos) >= '0' && s.charAt(pos) <= '9') {
                pos++;
            }
            return pos > start;
        }

        private String string() {
            pos++;
            StringBuilder out = new StringBuilder();
            while (true) {
                if (pos >= s.length()) {
                    throw error("unterminated string");
                }
                char c = s.charAt(pos++);
                if (c == '"') {
                    return out.toString();
                }
                if (c < 0x20) {
                    throw error("control character in string");
                }
                if (c != '\\') {
                    out.append(c);
                    continue;
                }
                if (pos >= s.length()) {
                    throw error("unterminated string");
                }
                char e = s.charAt(pos++);
                switch (e) {
                    case '"', '\\', '/' -> out.append(e);
                    case 'b' -> out.append('\b');
                    case 'f' -> out.append('\f');
                    case 'n' -> out.append('\n');
                    case 'r' -> out.append('\r');
                    case 't' -> out.append('\t');
                    case 'u' -> {
                        if (pos + 4 > s.length() || !s.substring(pos, pos + 4).matches("[0-9a-fA-F]{4}")) {
                            throw error("invalid unicode escape");
                        }
                        char unit = (char) Integer.parseInt(s.substring(pos, pos + 4), 16);
                        pos += 4;
                        // Like encoding/json: a valid escaped pair combines, any lone surrogate becomes U+FFFD.
                        if (Character.isHighSurrogate(unit) && pos + 6 <= s.length() && s.startsWith("\\u", pos)
                                && s.substring(pos + 2, pos + 6).matches("[0-9a-fA-F]{4}")
                                && Character.isLowSurrogate((char) Integer.parseInt(s.substring(pos + 2, pos + 6), 16))) {
                            out.append(unit).append((char) Integer.parseInt(s.substring(pos + 2, pos + 6), 16));
                            pos += 6;
                        } else {
                            out.append(Character.isSurrogate(unit) ? '\uFFFD' : unit);
                        }
                    }
                    default -> throw error("invalid escape");
                }
            }
        }
    }
}
