package example.brewlet;

import jakarta.servlet.annotation.WebServlet;
import jakarta.servlet.http.HttpServlet;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.servlet.http.HttpServletResponse;
import java.io.IOException;
import java.time.Instant;
import java.util.logging.Logger;

@WebServlet("/runtime")
public final class RuntimeServlet extends HttpServlet {
    private static final Logger LOGGER = Logger.getLogger(RuntimeServlet.class.getName());

    @Override
    protected void doGet(HttpServletRequest request, HttpServletResponse response)
            throws IOException {
        String timestamp = Instant.now().toString();
        LOGGER.info("RuntimeServlet called. timestamp=" + timestamp);
        response.setContentType("application/json");
        response.setCharacterEncoding("UTF-8");
        response.getWriter().printf(
                "{\"application\":\"tomcat-war\",\"java\":\"%s\",\"server\":\"%s\",\"timestamp\":\"%s\"}%n",
                System.getProperty("java.version"), getServletContext().getServerInfo(), timestamp);
        LOGGER.info("RuntimeServlet responded.");
    }
}
