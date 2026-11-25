<%@ page contentType="text/html; charset=UTF-8" %>
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Apache Tomcat WAR | Brewlet</title>
  <link rel="stylesheet" href="style.css">
  <script src="app.js" defer></script>
</head>
<body>
  <main>
    <h1>Apache Tomcat WAR is running</h1>
    <p>A standard web app, served by a Tomcat runtime through Brewlet.</p>
    <div class="details">
      <p>WAR deployed on <%= application.getServerInfo() %></p>
      <p>Java <%= System.getProperty("java.version") %></p>
    </div>
    <p>JSP compilation works with a read-only application and runtime.</p>
    <a class="button" id="runtime-link" href="runtime">View runtime details</a>
    <p class="hint">Shows the servlet's JSON response.</p>
    <section class="result" id="runtime-result" aria-live="polite" aria-atomic="true" hidden>
      <h2>Servlet response</h2>
      <pre id="runtime-output"></pre>
    </section>
  </main>
</body>
</html>
