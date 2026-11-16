package demo;

import com.sun.net.httpserver.HttpServer;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import org.apache.logging.log4j.LogManager;
import org.apache.logging.log4j.Logger;

public final class Main {
    private static final Logger LOG = LogManager.getLogger(Main.class);

    public static void main(String[] args) throws Exception {
        // The Log4j that actually loaded, not the one this code was compiled against.
        String log4j = LogManager.getContext(false).getClass().getPackage().getImplementationVersion();
        HttpServer server = HttpServer.create(new InetSocketAddress(8000), 0);
        server.createContext("/", exchange -> {
            LOG.info("GET {}", exchange.getRequestURI());
            byte[] body = String.format("{\"java\": \"%s\", \"log4j\": \"%s\"}%n",
                    Runtime.version(), log4j).getBytes(StandardCharsets.UTF_8);
            exchange.sendResponseHeaders(200, body.length);
            try (var out = exchange.getResponseBody()) {
                out.write(body);
            }
        });
        server.start();
        LOG.info("Listening on port 8000 with Log4j {}", log4j);
    }
}
