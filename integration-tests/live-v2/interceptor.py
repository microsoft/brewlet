#!/usr/bin/env python3
"""A real, fail-closed Kubernetes node-update admission interceptor.

The configuration is read on each request from a mounted Secret. Tests wait for
its observed revision before relying on an injected refusal.
"""

import http.server
import json
import os
import ssl


def decision(request, config):
    if request.get("resource", {}).get("resource") != "nodes":
        return True, "not a node"
    old, new = request.get("oldObject") or {}, request.get("object") or {}
    if new.get("metadata", {}).get("name") != config["node"]:
        return True, "not the invocation node"
    before, after = old.get("metadata", {}), new.get("metadata", {})
    if before.get("uid") != config["nodeUID"] or after.get("uid") != config["nodeUID"]:
        return False, "invocation node UID changed"
    for key in config.get("pinnedLabels", []):
        if before.get("labels", {}).get(key) != after.get("labels", {}).get(key):
            initial = config.get("initialPinnedLabels", {})
            if (key not in before.get("labels", {}) and key in initial
                    and after.get("labels", {}).get(key) == initial[key]
                    and config.get("poolOwner")
                    and request.get("userInfo", {}).get("username") == config["poolOwner"]):
                continue
            return False, "provider-pinned label refusal: " + key
    if config.get("denyFence"):
        def fence(obj):
            return [t for t in obj.get("spec", {}).get("taints", [])
                    if t.get("key") == "startup-taint.cluster-autoscaler.kubernetes.io/brewlet"]
        if fence(old) != fence(new):
            return False, "provider startup-taint write refusal"
    return True, "allowed"


def review_response(uid, allowed, message):
    return {"apiVersion": "admission.k8s.io/v1", "kind": "AdmissionReview",
            "response": {"uid": uid, "allowed": allowed,
                         "status": {"status": "Success" if allowed else "Failure",
                                    "code": 200 if allowed else 403,
                                    "reason": "Success" if allowed else "Forbidden",
                                    "message": message}}}


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        config = json.load(open(os.environ["CONFIG"], encoding="utf-8"))
        body = json.dumps({"revision": config["revision"]}).encode()
        self.send_response(200)
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        review = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        config = json.load(open(os.environ["CONFIG"], encoding="utf-8"))
        request = review["request"]
        allowed, message = decision(request, config)
        print(json.dumps({"requestUID": request["uid"], "revision": config["revision"],
                          "allowed": allowed, "message": message}), flush=True)
        response = review_response(request["uid"], allowed, message)
        body = json.dumps(response).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(body)


if __name__ == "__main__":
    server = http.server.ThreadingHTTPServer(("0.0.0.0", 9443), Handler)
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain("/certs/tls.crt", "/certs/tls.key")
    server.socket = context.wrap_socket(server.socket, server_side=True)
    server.serve_forever()
