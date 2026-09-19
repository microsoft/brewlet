// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package com.example;

import com.sun.net.httpserver.HttpServer;
import java.io.IOException;
import java.io.OutputStream;
import java.lang.management.ManagementFactory;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.Arrays;
import java.util.concurrent.atomic.AtomicLong;

/**
 * Minimal dependency-free HTTP server used to prove the Brewlet PoC:
 * it is a self-executable JAR launched via {@code java -jar app.jar}.
 */
public final class Hello {

    private static final AtomicLong loadUntil = new AtomicLong();
    private static volatile long loadResult;

    public static void main(String[] args) throws IOException {
        int port = Integer.getInteger("server.port", 8080);
        HttpServer server = HttpServer.create(new InetSocketAddress(port), 0);

        server.createContext("/hello", exchange -> {
            String body = "Hello from a JAR running directly on the node via Brewlet!\n"
                    + managedDependencyGreeting();
            respond(exchange, 200, body);
        });

        server.createContext("/info", exchange -> {
            Runtime rt = Runtime.getRuntime();
            long maxMb = rt.maxMemory() / (1024 * 1024);
            String body = String.format(
                    "java.version       = %s%n"
                  + "java.vendor        = %s%n"
                  + "process.uid        = %s%n"
                  + "process.gid        = %s%n"
                  + "availableProcessors= %d   (cgroup/JVM aware)%n"
                  + "Runtime.maxMemory  = %d MB (driven by -XX:MaxRAMPercentage)%n"
                  + "jvm.input.args     = %s%n",
                   System.getProperty("java.version"),
                   System.getProperty("java.vendor"),
                   processStatusID("Uid:"),
                   processStatusID("Gid:"),
                   rt.availableProcessors(),
                   maxMb,
                   ManagementFactory.getRuntimeMXBean().getInputArguments());
            respond(exchange, 200, body);
        });

        server.createContext("/healthz", exchange -> respond(exchange, 200, "ok\n"));

        // Opt-in, leased CPU work: a failed client cannot leave permanent load.
        if (Boolean.getBoolean("brewlet.test.cpuLoad")) {
            server.createContext("/load", exchange -> {
                if (!"POST".equals(exchange.getRequestMethod())) {
                    respond(exchange, 405, "POST required\n");
                    return;
                }
                String rawQuery = exchange.getRequestURI().getRawQuery();
                String[] parameters = rawQuery == null ? new String[0] : rawQuery.split("&");
                String[] leases = Arrays.stream(parameters)
                        .filter(parameter -> parameter.startsWith("seconds="))
                        .toArray(String[]::new);
                String query = leases.length == 1 ? leases[0] : "";
                if ("seconds=0".equals(query)) {
                    loadUntil.set(0);
                } else if ("seconds=20".equals(query)) {
                    loadUntil.set(System.nanoTime() + 20_000_000_000L);
                } else {
                    respond(exchange, 400, "seconds must be 0 or 20\n");
                    return;
                }
                respond(exchange, 200, "ok\n");
            });
            Thread.ofPlatform().daemon().name("fixture-cpu-load").start(() -> {
                long value = 1;
                while (!Thread.currentThread().isInterrupted()) {
                    if (System.nanoTime() < loadUntil.get()) {
                        for (int i = 0; i < 10_000; i++) {
                            value = value * 2862933555777941757L + 3037000493L;
                        }
                        loadResult = value;
                    } else {
                        try {
                            Thread.sleep(50);
                        } catch (InterruptedException interrupted) {
                            Thread.currentThread().interrupt();
                        }
                    }
                }
            });
        }

        server.setExecutor(null);
        System.out.printf("[demo-app] listening on :%d  (pid=%d, java=%s)%n",
                port, ProcessHandle.current().pid(), System.getProperty("java.version"));
        server.start();
    }

    private static void respond(com.sun.net.httpserver.HttpExchange exchange, int code, String body)
            throws IOException {
        byte[] bytes = body.getBytes(StandardCharsets.UTF_8);
        exchange.getResponseHeaders().add("Content-Type", "text/plain; charset=utf-8");
        exchange.sendResponseHeaders(code, bytes.length);
        try (OutputStream os = exchange.getResponseBody()) {
            os.write(bytes);
        }
    }

    private static String processStatusID(String key) {
        try {
            for (String line : Files.readAllLines(Path.of("/proc/self/status"))) {
                if (line.startsWith(key)) {
                    String[] values = line.substring(key.length()).trim().split("\\s+");
                    return values.length == 0 ? "unknown" : values[0];
                }
            }
        } catch (IOException ignored) {
            // /proc is Linux-specific; local non-Linux runs report unknown.
        }
        return "unknown";
    }

    private static String managedDependencyGreeting() {
        try {
            Class<?> greeting = Class.forName("com.example.approved.Greeting");
            return greeting.getMethod("message").invoke(null) + "\n";
        } catch (ClassNotFoundException absent) {
            return "";
        } catch (ReflectiveOperationException invalidDependency) {
            throw new IllegalStateException("Could not invoke managed dependency", invalidDependency);
        }
    }

    private Hello() {
    }
}
