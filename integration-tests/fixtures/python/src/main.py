import http.server
import json
import os
import platform
import sys

from greeting import message


def report():
    return {
        "language": "python",
        "runtime": platform.python_version(),
        "dependency": message(),
        "arguments": sys.argv[1:],
        "cwd": os.getcwd(),
        "uid": os.getuid(),
        "gid": os.getgid(),
        "path": os.environ.get("PATH"),
        "home": os.environ.get("HOME"),
        "value": os.environ.get("BREWLET_TEST_VALUE"),
    }


if "--probe" in sys.argv:
    print(json.dumps(report()))
else:
    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            body = json.dumps(report()).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    server = http.server.HTTPServer(("0.0.0.0", int(os.environ.get("BREWLET_TEST_PORT", "8000"))), Handler)
    print(json.dumps({"listening": server.server_port}), flush=True)
    server.serve_forever()
