// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sh.brewlet.maven.plugin;

import com.sun.net.httpserver.HttpServer;
import sh.brewlet.maven.plugin.oci.LocalStore;

import java.io.IOException;
import java.net.InetSocketAddress;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.atomic.AtomicInteger;

/** Loopback registry recording the exact application blobs and manifests uploaded. */
final class TestImageRegistry implements AutoCloseable {
    final Map<String, byte[]> content = new ConcurrentHashMap<>();
    final AtomicInteger requests = new AtomicInteger();
    final AtomicInteger taggedWrites = new AtomicInteger();
    volatile boolean fail;
    volatile byte[] tagged;
    private final HttpServer server;

    TestImageRegistry() throws IOException {
        server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/", exchange -> {
            try (exchange) {
                requests.incrementAndGet();
                String path = exchange.getRequestURI().getPath();
                String method = exchange.getRequestMethod();
                if (fail) {
                    exchange.sendResponseHeaders(500, -1);
                } else if ("HEAD".equals(method)) {
                    exchange.sendResponseHeaders(404, -1);
                } else if ("POST".equals(method)) {
                    exchange.getResponseHeaders().set("Location", "/v2/app/blobs/uploads/test");
                    exchange.sendResponseHeaders(202, -1);
                } else if ("PUT".equals(method)) {
                    byte[] bytes = exchange.getRequestBody().readAllBytes();
                    content.put(LocalStore.sha256Hex(bytes), bytes);
                    if (path.equals("/v2/app/manifests/1")) {
                        tagged = bytes;
                        taggedWrites.incrementAndGet();
                    }
                    exchange.sendResponseHeaders(201, -1);
                } else {
                    exchange.sendResponseHeaders(404, -1);
                }
            }
        });
        server.start();
    }

    String authority() {
        return "127.0.0.1:" + server.getAddress().getPort();
    }

    String image() {
        return authority() + "/app:1";
    }

    @Override public void close() {
        server.stop(0);
    }
}
