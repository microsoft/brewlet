package sh.brewlet.fixture;

import com.sun.net.httpserver.HttpServer;
import com.sun.security.auth.module.UnixSystem;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.nio.file.Path;
import java.util.Arrays;

public final class Main {
    private static String quote(String value) {
        if (value == null) return "null";
        StringBuilder result = new StringBuilder("\"");
        for (char character : value.toCharArray()) {
            switch (character) {
                case '"' -> result.append("\\\"");
                case '\\' -> result.append("\\\\");
                case '\n' -> result.append("\\n");
                case '\r' -> result.append("\\r");
                case '\t' -> result.append("\\t");
                default -> {
                    if (character < 32) result.append(String.format("\\u%04x", (int) character));
                    else result.append(character);
                }
            }
        }
        return result.append('"').toString();
    }

    private static String report(String[] args) {
        UnixSystem identity = new UnixSystem();
        return "{\"language\":\"java\",\"runtime\":" + quote(Runtime.version().toString())
                + ",\"dependency\":" + quote(Greeting.message())
                + ",\"arguments\":[" + String.join(",", Arrays.stream(args).map(Main::quote).toList()) + "]"
                + ",\"cwd\":" + quote(Path.of("").toAbsolutePath().toString())
                + ",\"uid\":" + identity.getUid() + ",\"gid\":" + identity.getGid()
                + ",\"path\":" + quote(System.getenv("PATH"))
                + ",\"home\":" + quote(System.getenv("HOME"))
                + ",\"value\":" + quote(System.getenv("BREWLET_TEST_VALUE")) + "}";
    }

    public static void main(String[] args) throws Exception {
        if (Arrays.asList(args).contains("--probe")) {
            System.out.println(report(args));
            return;
        }
        HttpServer server = HttpServer.create(new InetSocketAddress("0.0.0.0", 8000), 0);
        server.createContext("/", exchange -> {
            byte[] body = report(args).getBytes(StandardCharsets.UTF_8);
            exchange.getResponseHeaders().set("Content-Type", "application/json");
            exchange.sendResponseHeaders(200, body.length);
            try (var output = exchange.getResponseBody()) {
                output.write(body);
            }
        });
        server.start();
    }

    private Main() {}
}
