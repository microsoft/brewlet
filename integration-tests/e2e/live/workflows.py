#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Executable workflow coverage built entirely from the checkout under test.

Runs the shipped `brewlet` CLI and the checkout's Maven plugin against an
invocation-owned registry and disposable kind cluster:

  push      remote `brewlet push` (anonymous, Docker config, environment and
            rejected credentials; --store; unqualified refs; repeat publication)
  apps      `brewlet k8s app status|wait` (namespaces, outputs, generations,
            missing resources, rollout and API timeouts)
  inventory `brewlet k8s status|doctor`, `jdk|launcher list`, `profile
            list|inspect`, `inspect app` (outputs, namespaces, ground truth),
            `jdk|launcher add` (client/server dry runs without mutation,
            offline --file, Helm refusal, live add on a disposable profile)
            and `install` validation plus its fresh-install-only guard
  maven     `mvn package brewlet:push` (immutable handoff -> kubectl apply ->
            CLI Ready -> response, encrypted settings, dry run)
  profiles  `brewlet k8s profile delete` (guards, dry runs, timeout, attach and
            real operator/provisioner host cleanup)

Deterministic API-layer permutations live in
kubernetes/internal/cli/profile_delete_integration_test.go; protocol
permutations live in the registry conformance tests.
"""

import json
from pathlib import Path
import re
import secrets
import shutil
import subprocess
import threading
import time

from common import (JDK_IMAGE, OWNER_LABEL, REGISTRY_IMAGE, ROOT, owned_container,
                    retry_transient, run, sha256, wait)
from checkout import CheckoutFixture as CheckoutRuntimeFixture
import workflows_helpers as h

APP_NS, OTHER_NS, MAVEN_NS, PROFILE_NS = "wf-alpha", "wf-beta", "wf-maven", "wf-profile"
OPERATOR = ["deployment/brewlet-operator", "-n", "brewlet"]
SHIM_PATHS = ("/opt/brewlet/bin/containerd-shim-brewlet-v2", "/usr/local/bin/containerd-shim-brewlet-v2")
JDK_ROOT = "/opt/brewlet/jdks"
FINALIZER = "node.brewlet.sh/cleanup"
# Documented tolerance above a requested deadline: process start, one in-flight
# kubectl call and output flushing.
CLI_TOLERANCE = 15


class CheckoutFixture(CheckoutRuntimeFixture):
    """The shared strict fixture, provisioned from checkout-built components."""

    def __init__(self):
        super().__init__("workflows")
        self.secrets = set()
        self.commands = []
        self.auth_registry_id = None
        self.cleanups.append(self.remove_owned)

    def secret(self, value):
        self.secrets.add(value)
        return value

    # ---- recording -------------------------------------------------------

    def cmd(self, name, argv, *, env=None, expect=0, timeout=600, input=None, cwd=None,
            target=None, sensitive_stdout=False):
        """Run one command; keep sanitized argv, exit code and separate streams."""
        argv = [str(a) for a in argv]
        start = time.monotonic()
        result = run(argv, env=dict(self.env, **(env or {})), check=False, timeout=timeout,
                     input=input, cwd=cwd)
        result.elapsed = time.monotonic() - start
        number = len(self.commands) + 1
        base = f"cmd-{number:03d}-{name}"
        for stream in ("stdout", "stderr"):
            text = getattr(result, stream)
            if stream == "stdout" and sensitive_stdout:
                text = h.REDACTED + " (generated security material)\n"
            (self.work / f"{base}.{stream}").write_text(h.redact_secrets(text, self.secrets))
        self.commands.append({"name": name, **h.sanitized_command(argv, env, self.secrets),
                              "target": target, "exitCode": result.returncode,
                              "elapsedSeconds": round(result.elapsed, 3),
                              "stdout": f"{base}.stdout", "stderr": f"{base}.stderr"})
        self.save("commands.json", self.commands)
        for secret in self.secrets:
            if secret and (secret in result.stdout or secret in result.stderr):
                raise AssertionError(f"{name} disclosed generated secret material")
        if expect == 0 and result.returncode:
            raise AssertionError(f"{name} exited {result.returncode}: "
                                 + h.redact_secrets(result.stderr[-2000:] or result.stdout[-2000:],
                                                    self.secrets))
        if expect == "fail" and result.returncode == 0:
            raise AssertionError(f"{name} unexpectedly succeeded")
        return result

    def k8s(self, name, kubeconfig, *args, expect=0, timeout=600, context=None):
        connection = ["--kubeconfig", kubeconfig] + (["--context", context] if context else [])
        return self.cmd(name, [self.cli, "k8s", *connection, *args], expect=expect,
                        timeout=timeout, target={"kubeconfig": Path(kubeconfig).name,
                                                 "context": context or "current-context"})

    # ---- checkout-built components -----------------------------------------

    def start(self):
        for tool in ("go", "htpasswd"):
            if not shutil.which(tool):
                raise RuntimeError(f"Required prerequisite missing: {tool}; no assertions skipped")
        super().start()

    def setup_cmd(self, name, argv, *, resolution_only=False, **kwargs):
        """Recorded infrastructure step: transient network failures are retried and an
        exhausted retry raises InfrastructureError instead of an assertion failure."""
        result = retry_transient(name, lambda: self.cmd(name, argv, expect=None, **kwargs),
                                 resolution_only=resolution_only)
        if result.returncode:
            raise RuntimeError(f"{name} failed ({result.returncode}); see {self.work}")
        return result

    def build_command(self, name, argv, **kwargs):
        return self.setup_cmd(name, argv, **kwargs)

    def start_auth_registry(self, htpasswd):
        name = self.name + "-auth-registry"
        try:
            self.run(["docker", "create", "--platform", f"linux/{self.arch}", "--name", name,
                      "--label", f"{OWNER_LABEL}={self.name}", "--cpus", "0.5", "--memory", "256m",
                      "-e", "REGISTRY_AUTH=htpasswd", "-e", "REGISTRY_AUTH_HTPASSWD_REALM=brewlet-live",
                      "-e", "REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd",
                      "-p", "127.0.0.1::5000", REGISTRY_IMAGE])
        finally:
            result = self.run(["docker", "inspect", name], check=False)
            if result.returncode == 0:
                info = json.loads(result.stdout)[0]
                if info["Config"].get("Labels", {}).get(OWNER_LABEL) != self.name:
                    raise RuntimeError("Refusing to adopt a foreign auth registry")
                self.auth_registry_id = info["Id"]
        self.run(["docker", "cp", htpasswd.parent, f"{self.auth_registry_id}:/auth"])
        self.run(["docker", "start", self.auth_registry_id])
        info = json.loads(self.run(["docker", "inspect", self.auth_registry_id]).stdout)[0]
        registry = f"localhost:{info['NetworkSettings']['Ports']['5000/tcp'][0]['HostPort']}"
        wait("auth registry challenge", lambda: h.fetch(registry, "/v2/")[0] == 401, timeout=60, interval=1)
        return registry

    def remove_owned(self):
        errors = []
        if self.auth_registry_id:
            result = self.run(["docker", "inspect", self.auth_registry_id], check=False)
            if result.returncode == 0:
                info = json.loads(result.stdout)[0]
                if owned_container(info, self.auth_registry_id, OWNER_LABEL, self.name):
                    self.run(["docker", "logs", "--tail", "200", self.auth_registry_id], check=False)
                    self.run(["docker", "rm", "-f", "--volumes", self.auth_registry_id])
                else:
                    errors.append("refusing to remove foreign auth registry")
        if errors:
            raise RuntimeError("; ".join(errors))

    # ---- cluster helpers ---------------------------------------------------

    def base_kubeconfig(self):
        return json.loads(self.kube("config", "view", "--raw", "--minify", "--flatten",
                                    "-o", "json").stdout)

    def write_kubeconfig(self, name, config):
        path = self.private / f"kubeconfig-{name}.json"
        h.write_private(path, json.dumps(config))
        return path

    def ensure_namespace(self, name):
        self.apply({"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": name}})

    def pause_operator(self):
        self.kube("scale", *OPERATOR, "--replicas=0")
        self.kube("wait", "--for=delete", "pod", "-l", "app=brewlet-operator", "-n", "brewlet",
                  "--timeout=120s")

    def resume_operator(self):
        self.kube("scale", *OPERATOR, "--replicas=1")
        self.kube("rollout", "status", *OPERATOR, "--timeout=180s")

    def application(self, name, namespace, image, readiness="/healthz", args=None):
        obj = {"apiVersion": "apps.brewlet.sh/v1alpha1", "kind": "JavaApplication",
               "metadata": {"name": name, "namespace": namespace},
               "spec": {"artifact": {"image": image, "pullPolicy": "IfNotPresent"},
                        "replicas": 1, "jvm": {"version": 21, "distribution": "temurin"},
                        "ports": [{"name": "http", "containerPort": 8080}],
                        "resources": {"requests": {"cpu": "100m", "memory": "128Mi"},
                                      "limits": {"cpu": "500m", "memory": "256Mi"}},
                        "probes": {"readiness": {"httpGet": {"path": readiness, "port": 8080},
                                                  "periodSeconds": 2, "timeoutSeconds": 2}}}}
        if args:
            obj["spec"]["jvm"]["args"] = args
        self.apply(obj)

    def hello(self, namespace, name):
        return self.kube("get", "--raw", f"/api/v1/namespaces/{namespace}/services/{name}:8080/proxy/hello",
                         timeout=60).stdout

    def node_path(self, path):
        self.own_container(self.node)
        return self.run(["docker", "exec", self.node_id, "test", "-e", path], check=False).returncode == 0

    def host_state(self):
        self.own_container(self.node)
        labels = self.get("node", self.node)["metadata"]["labels"]
        runtime = self.run(["docker", "exec", self.node_id, "sh", "-c",
                            "grep -rl 'runtimes.brewlet' /etc/containerd || true"]).stdout.split()
        return {"shim": [p for p in SHIM_PATHS if self.node_path(p)],
                "jdkRoots": self.node_path(JDK_ROOT), "containerdRuntimeConfig": runtime,
                "ownerUID": labels.get("brewlet.sh/owner-uid"),
                "jdkLabels": sorted(k for k in labels if k.startswith("brewlet.sh/jdk."))}

    def brewlet_pods(self):
        return [p for p in self.get("pods", "-A")["items"]
                if p["spec"].get("runtimeClassName") == "brewlet"]

    def provision_profile(self, name):
        self.apply({
            "apiVersion": "node.brewlet.sh/v1alpha1", "kind": "NodeProfile", "metadata": {"name": name},
            "spec": {"nodePool": {"key": "brewlet.sh/e2e-pool", "names": ["live"], "includeControlPlane": True},
                     "jdks": [{"distribution": "temurin", "feature": 21,
                               "source": {"image": JDK_IMAGE,
                                          "javaHome": "/opt/java/openjdk"}}],
                     "rollout": {"validate": True, "containerdRestart": "validated"}}})
        wait(f"{name} provisions temurin-21", lambda: self.get("node", self.node)["metadata"]["labels"].get(
            "brewlet.sh/jdk.temurin-21") == "true", timeout=480)


def snapshot(f, name):
    meta = f.get("nodeprofile", name)["metadata"]
    return {k: meta.get(k) for k in ("uid", "resourceVersion", "deletionTimestamp")}


def unchanged(f, name, before, what):
    after = snapshot(f, name)
    if after != before:
        raise AssertionError(f"{what} changed NodeProfile {name}: {before} -> {after}")


def require(condition, message):
    if not condition:
        raise AssertionError(message)


# ---- 1. remote CLI push ---------------------------------------------------------

def build_app(f, name):
    app = f.private / name
    shutil.copytree(ROOT / "integration-tests/fixtures/demo-app", app, ignore=shutil.ignore_patterns("target"))
    return app


def maven_app(f, name, configuration=""):
    """Copy the fixture and select the checkout-built publishing plugin."""
    app = build_app(f, name)
    pom = app / "pom.xml"
    text = pom.read_text()
    plugin = (f"      <plugin>\n        <groupId>sh.brewlet</groupId>\n"
              f"        <artifactId>brewlet-maven-plugin</artifactId>\n"
              f"        <version>{f.plugin_version}</version>\n"
              f"        <configuration>{configuration}</configuration>\n      </plugin>\n    </plugins>")
    require(text.count("</plugins>") == 1, "fixture pom layout changed; cannot declare publishing plugin")
    pom.write_text(text.replace("    </plugins>", plugin, 1))
    return app


def push_scenario(f):
    app = build_app(f, "push-app")
    f.setup_cmd("package-demo", [*f.maven_args, "-f", app / "pom.xml", "package"], timeout=600)
    jar = app / "target/app.jar"
    cwd = f.private / "push-cwd"
    cwd.mkdir()
    reg, repo = f.registry, "wf/cli"

    def push(name, ref, handoff=None, expect=0, env=None, extra=()):
        argv = [f.cli, "push", *extra] + (["--push-result", handoff] if handoff else []) + [jar, ref]
        return f.cmd(name, argv, expect=expect, env=env, cwd=cwd, timeout=600)

    first = push("push-anonymous", f"{reg}/{repo}:v1", cwd / "anonymous.json")
    reported = h.push_stdout(first.stdout)
    handoff = json.loads((cwd / "anonymous.json").read_text())
    h.validate_handoff(handoff, reg, repo, "v1", reported["digest"])
    published = h.verify_published(reg, repo, "v1")
    require(published["digest"] == reported["digest"] == handoff["digest"], "registry digest differs from handoff")
    require(published["mediaType"] == h.INDEX, "runnable push did not publish an OCI index")
    arches = sorted(m["architecture"] for m in published["manifests"])
    require({"amd64", "arm64"} <= set(arches), f"index lacks amd64+arm64 manifests: {arches}")
    require("credentials: none found for " + reg + " (anonymous)" in first.stderr, "anonymous credential source not reported")
    require("credentials:" not in first.stdout and reported["deployImage"] == handoff["deployImage"],
            "push progress leaked to stdout or deploy image mismatch")
    require(not (cwd / "oci").exists(), "registry push wrote the default ./oci store")
    f.record("cli-push-anonymous-verifiable-index", {"handoff": handoff, "index": published})

    second = push("push-repeat", f"{reg}/{repo}:v1", cwd / "repeat.json")
    repeat = json.loads((cwd / "repeat.json").read_text())
    h.validate_handoff(repeat, reg, repo, "v1", h.push_stdout(second.stdout)["digest"])
    again = h.verify_published(reg, repo, "v1")
    require(again["digest"] == repeat["digest"], "repeat publication tag does not match its handoff")
    h.verify_published(reg, repo, handoff["digest"])
    counts = re.search(r"\((\d+) blob\(s\) uploaded, (\d+) already present\)", second.stdout)
    require(counts and int(counts[2]) > 0, "repeat publication did not reuse existing blobs")
    f.record("cli-push-repeat-publication-intact", {"first": handoff["digest"], "repeat": repeat["digest"],
             "identical": repeat["digest"] == handoff["digest"], "uploaded": int(counts[1]),
             "alreadyPresent": int(counts[2])})

    store = f.private / "explicit-store"
    push("push-explicit-store", f"{reg}/wf/local:v1", extra=["--store", store])
    require((store / "index.json").is_file() and h.manifest(reg, "wf/local", "v1") is None,
            "--store published remotely or wrote no local layout")
    failed = push("push-unqualified-handoff", "wf/unqualified:v1", cwd / "unqualified.json", expect="fail")
    require("Brewlet never defaults to Docker Hub" in failed.stderr and not failed.stdout.strip() and
            not (cwd / "unqualified.json").exists(), "unqualified handoff was not rejected cleanly")
    local = push("push-unqualified-local", "wf/unqualified:v1")
    require((cwd / "oci/index.json").is_file() and "registry:" not in local.stdout,
            "unqualified ref did not stay in the local ./oci store")
    f.record("cli-push-store-and-unqualified-targets", {"explicitStore": str(store.name),
             "unqualifiedHandoffRejected": True, "unqualifiedLocalStore": "oci"})

    user, password = "wf-user", f.secret(secrets.token_urlsafe(24))
    wrong = f.secret(secrets.token_urlsafe(24))
    htpasswd = f.private / "auth" / "htpasswd"
    htpasswd.parent.mkdir()
    entry = run(["htpasswd", "-niB", user], input=password + "\n").stdout.strip()
    h.write_private(htpasswd, entry + "\n")
    htpasswd.chmod(0o644)  # read by the registry container user
    auth = f.start_auth_registry(htpasswd)
    f.auth_registry = auth
    f.auth = (user, password)

    rejected = push("push-auth-anonymous", f"{auth}/wf/auth:anonymous", cwd / "auth-anon.json", expect="fail")
    require(not (cwd / "auth-anon.json").exists() and "deploy image" not in rejected.stdout,
            "failed anonymous publication reported a handoff")
    docker = f.private / "docker-auth"
    docker.mkdir()
    token = f.secret(h.basic_auth(user, password).removeprefix("Basic "))
    h.write_private(docker / "config.json", json.dumps({"auths": {auth: {"auth": token}}}))
    via_docker = push("push-auth-docker-config", f"{auth}/wf/auth:docker", cwd / "auth-docker.json",
                      env={"DOCKER_CONFIG": str(docker)})
    require(f"credentials: {docker / 'config.json'} (auths)" in via_docker.stderr, "Docker config source not reported")
    via_env = push("push-auth-environment", f"{auth}/wf/auth:env", cwd / "auth-env.json",
                   env={"BREWLET_REGISTRY_USERNAME": user, "BREWLET_REGISTRY_PASSWORD": password})
    require("credentials: BREWLET_REGISTRY_USERNAME/BREWLET_REGISTRY_PASSWORD" in via_env.stderr,
            "environment credential source not reported")
    for tag, result in (("docker", via_docker), ("env", via_env)):
        data = json.loads((cwd / f"auth-{tag}.json").read_text())
        h.validate_handoff(data, auth, "wf/auth", tag, h.push_stdout(result.stdout)["digest"])
        require(h.verify_published(auth, "wf/auth", tag, f.auth)["digest"] == data["digest"],
                "authenticated publication does not match handoff")
    bad = push("push-auth-rejected", f"{auth}/wf/auth:rejected", cwd / "auth-bad.json", expect="fail",
               env={"BREWLET_REGISTRY_USERNAME": user, "BREWLET_REGISTRY_PASSWORD": wrong})
    require(not (cwd / "auth-bad.json").exists() and "deploy image" not in bad.stdout and
            h.manifest(auth, "wf/auth", "rejected", f.auth) is None, "rejected credentials published content")
    f.record("cli-push-authenticated-and-rejected", {"registry": "invocation-owned htpasswd distribution",
             "dockerConfig": "isolated", "environment": "BREWLET_REGISTRY_USERNAME/PASSWORD",
             "anonymousRejected": True, "wrongPasswordRejected": True})
    return handoff["deployImage"], published


# ---- 2. application status and wait -----------------------------------------

def app_scenario(f, image, published):
    for namespace in (APP_NS, OTHER_NS):
        f.ensure_namespace(namespace)
    base = f.base_kubeconfig()
    kc = f.write_kubeconfig("alpha", h.derive_kubeconfig(base, context="wf-target", namespace=APP_NS))
    stdout, stderr = f.work / "app-wait-missing.stdout", f.work / "app-wait-missing.stderr"
    argv = [f.cli, "k8s", "--kubeconfig", kc, "app", "wait", "wf-app", "--wait-timeout", "420s"]
    started = time.monotonic()
    with open(stdout, "w") as out, open(stderr, "w") as err:
        waiter = subprocess.Popen([str(a) for a in argv], stdout=out, stderr=err, env=f.env,
                                  start_new_session=True)
    f.children.append(waiter)
    f.commands.append({"name": "app-wait-before-create", **h.sanitized_command([str(a) for a in argv], {}, f.secrets),
                       "target": {"kubeconfig": kc.name, "context": "current-context (namespace wf-alpha)"},
                       "stdout": stdout.name, "stderr": stderr.name, "background": True})
    wait("app wait reports the missing JavaApplication", lambda: "not found" in stderr.read_text(),
         timeout=60, interval=1)
    require(waiter.poll() is None, "app wait exited instead of retrying a missing JavaApplication")
    f.application("wf-app", APP_NS, image)
    f.application("wf-app", OTHER_NS, image, readiness="/not-ready")
    code = waiter.wait(timeout=480)
    elapsed = time.monotonic() - started
    f.commands[-1].update({"exitCode": code, "elapsedSeconds": round(elapsed, 3)})
    f.save("commands.json", f.commands)
    out, err = stdout.read_text(), stderr.read_text()
    require(code == 0, f"app wait failed after the app appeared: {err[-2000:]}")
    lines = out.splitlines()
    require(len(lines) == 1 and lines[0].startswith(f"wf-app is Ready in namespace {APP_NS} (JDK "),
            f"app wait stdout is not a single Ready line: {out!r}")
    require("Waiting up to" in err and "Ready" not in out.replace(lines[0], ""), "progress not on stderr")
    f.record("app-wait-retries-missing-resource", {"stdout": lines[0], "elapsedSeconds": round(elapsed, 1)})

    status = json.loads(f.k8s("app-status-json-default-ns", kc, "app", "status", "wf-app", "--output", "json").stdout)
    live = f.get("javaapplication", "wf-app", "-n", APP_NS)
    require(status["namespace"] == APP_NS and status["ready"] and
            status["generation"] == status["observedGeneration"] == live["metadata"]["generation"] and
            status["readyReplicas"] == 1 and "21" in status.get("selectedJdk", "") and
            status["nodes"] == [f.node] and status["pods"] and all(p["node"] == f.node for p in status["pods"]),
            f"Ready status report is incomplete: {status}")
    yaml = f.k8s("app-status-yaml", kc, "app", "status", "wf-app", "--output", "yaml").stdout
    top = h.yaml_top_level(yaml)
    require(top.get("name") == "wf-app" and top.get("namespace") == APP_NS and top.get("ready") == "true",
            f"YAML status not machine-readable: {top}")
    table = f.k8s("app-status-table", kc, "app", "status", "wf-app").stdout
    require(re.search(rf"^Namespace:\s+{APP_NS}$", table, re.M) and "Selected JDK:" in table, "table status incomplete")
    pending = f.k8s("app-status-unready-explicit-ns", kc, "app", "status", "wf-app", "--namespace", OTHER_NS, "--output", "json")
    other = json.loads(pending.stdout)
    require(other["namespace"] == OTHER_NS and not other["ready"], f"explicit namespace not targeted: {other}")
    f.record("app-status-namespaces-and-outputs", {"default": {k: status[k] for k in (
        "namespace", "ready", "generation", "observedGeneration", "readyReplicas", "selectedJdk", "nodes")},
        "explicit": {k: other[k] for k in ("namespace", "ready", "reason", "readyReplicas")},
        "pods": status["pods"], "formats": ["json", "yaml", "table"]})

    timeout = f.k8s("app-wait-nonready-timeout", kc, "app", "wait", "wf-app", "--namespace", OTHER_NS,
                    "--wait-timeout", "20s", expect="fail")
    bound = h.assert_bounded("nonready app wait", timeout.elapsed, 20, CLI_TOLERANCE)
    require(f"JavaApplication {OTHER_NS}/wf-app was not Ready after 20s" in timeout.stderr and
            "Inspect it with: kubectl" in timeout.stderr and f"--namespace {OTHER_NS}" in timeout.stderr and
            not timeout.stdout.strip(), "timeout diagnostics incomplete or on stdout")
    f.record("app-wait-nonready-rollout-timeout", bound)

    f.pause_operator()
    try:
        f.kube("patch", "javaapplication", "wf-app", "-n", APP_NS, "--type=merge",
               "-p", json.dumps({"spec": {"jvm": {"args": ["-Dwf.generation=2"]}}}))
        generation = f.get("javaapplication", "wf-app", "-n", APP_NS)["metadata"]["generation"]
        stale = json.loads(f.k8s("app-status-stale-generation", kc, "app", "status", "wf-app", "--output", "json").stdout)
        require(not stale["ready"] and stale["generation"] == generation > stale["observedGeneration"],
                f"stale Ready condition reported as Ready: {stale}")
        old = f.k8s("app-wait-stale-generation", kc, "app", "wait", "wf-app", "--wait-timeout", "10s", expect="fail")
        require(f"reconcile generation {generation}" in old.stderr, "stale generation not explained")
        h.assert_bounded("stale-generation wait", old.elapsed, 10, CLI_TOLERANCE)
    finally:
        f.resume_operator()
    f.k8s("app-wait-current-generation", kc, "app", "wait", "wf-app", "--wait-timeout", "300s")
    current = json.loads(f.k8s("app-status-current-generation", kc, "app", "status", "wf-app", "--output", "json").stdout)
    require(current["ready"] and current["observedGeneration"] == generation, "new generation not Ready")
    f.record("app-wait-requires-current-generation", {"staleGeneration": generation,
             "staleObserved": stale["observedGeneration"], "operator": "paused via replicas=0, then resumed"})

    with h.StalledEndpoint() as stalled:
        config = h.derive_kubeconfig(base, context="wf-target", namespace=APP_NS)
        config["clusters"][0]["cluster"]["server"] = f"https://127.0.0.1:{stalled.port}"
        hung = f.write_kubeconfig("stalled", config)
        result = f.k8s("app-wait-stalled-api", hung, "app", "wait", "wf-app", "--timeout", "3s",
                       "--wait-timeout", "10s", expect="fail", timeout=120)
        stalled_wait = h.assert_bounded("stalled API app wait", result.elapsed, 10, CLI_TOLERANCE)
        require("was not Ready after 10s" in result.stderr, "stalled API wait lacks diagnostics")
        result = f.k8s("app-status-stalled-api", hung, "app", "status", "wf-app", "--timeout", "3s",
                       expect="fail", timeout=120)
        stalled_status = h.assert_bounded("stalled API app status", result.elapsed, 3, CLI_TOLERANCE)
    config = h.derive_kubeconfig(base, context="wf-target", namespace=APP_NS)
    config["clusters"][0]["cluster"]["server"] = f"https://127.0.0.1:{h.unused_port()}"
    result = f.k8s("app-status-unreachable-api", f.write_kubeconfig("closed", config), "app", "status", "wf-app",
                   "--timeout", "3s", expect="fail", timeout=120)
    require(result.stderr.strip() and not result.stdout.strip(), "unreachable API error not on stderr")
    f.record("app-commands-bounded-on-api-failure", {"stalledWait": stalled_wait, "stalledStatus": stalled_status,
             "unreachableStatusSeconds": round(result.elapsed, 3), "sharedCluster": "untouched (loopback fake endpoints)"})

    response = f.hello(APP_NS, "wf-app")
    require("Hello from a JAR" in response, "digest-pinned CLI image did not serve")
    f.record("cli-push-digest-pinned-workload-serves", {"image": image, "indexPlatforms": sorted(
        m["architecture"] for m in published["manifests"]), "runtimeArchitectureExercised": f.arch,
        "crossArchitectureRuntime": "not exercised; index children verified by registry fetch only"})


# ---- 3. cluster inventory, inspection and profile additions -------------------

INVENTORY_PROFILE = "wf-inventory"
JDK17 = ("--distribution", "temurin", "--feature", "17", "--image", JDK_IMAGE, "--java-home", "/opt/java/openjdk")
JAZ = ("--name", "jaz", "--image", JDK_IMAGE, "--path", "/opt/java/openjdk/bin/java")


def quiet_failure(result, needle, what):
    require(needle in result.stderr and not result.stdout.strip(),
            f"{what}: expected {needle!r} on stderr and empty stdout, got stderr={result.stderr[-1000:]!r} "
            f"stdout={result.stdout[-500:]!r}")


def node_jdks(f):
    """Ground truth: the provisioner's structured inventory on the owned node."""
    raw = f.get("node", f.node)["metadata"].get("annotations", {}).get("brewlet.sh/jdks-info", "")
    require(raw.strip(), "node does not advertise brewlet.sh/jdks-info")
    return json.loads(raw)


def inventory_scenario(f, image):
    kc = f.write_kubeconfig("inventory", h.derive_kubeconfig(f.base_kubeconfig(), context="wf-target"))
    alpha = f.write_kubeconfig("inventory-alpha", h.derive_kubeconfig(f.base_kubeconfig(), context="wf-target",
                                                                       namespace=APP_NS))
    profile = f.get("nodeprofile", "live")
    uid, generation = profile["metadata"]["uid"], profile["metadata"]["generation"]
    node = f.get("node", f.node)
    labels, annotations = node["metadata"]["labels"], node["metadata"].get("annotations", {})
    require(labels.get("brewlet.sh/owner-uid") == uid, "live profile does not own the node before inventory checks")
    advertised = node_jdks(f)
    require(any(j["distribution"] == "temurin" and j["feature"] == 21 for j in advertised),
            f"node inventory lacks temurin 21: {advertised}")
    inventory_reads(f, kc, alpha, profile, advertised, annotations)
    inventory_status(f, kc, alpha, profile, annotations)
    inspect_app(f, alpha, image)
    # Paused so a controller status write cannot hide or fake a dry-run mutation.
    f.pause_operator()
    try:
        profile_add(f, kc, uid)
    finally:
        f.resume_operator()
    disposable_add(f, kc)
    install_guards(f, kc)
    unchanged_after = f.get("nodeprofile", "live")["metadata"]
    require(unchanged_after["uid"] == uid and unchanged_after["generation"] == generation,
            "inventory section changed the live profile generation")


def inventory_reads(f, kc, alpha, profile, advertised, annotations):
    jdks = json.loads(f.k8s("jdk-list-json", kc, "jdk", "list", "--output", "json").stdout)
    expected = sorted((j["distribution"], j["vendor"], j["feature"], j["version"], j["arch"]) for j in advertised)
    require(sorted((j["distribution"], j["vendor"], j["feature"], j["version"], j["arch"]) for j in jdks) == expected and
            all(j["nodes"] == [f.node] for j in jdks), f"jdk list differs from the node annotation: {jdks} vs {advertised}")
    require({(j["distribution"], j["feature"]) for j in jdks} ==
            {(j["distribution"], j["feature"]) for j in profile["spec"]["jdks"]},
            "advertised JDKs differ from the live NodeProfile declaration")
    # The JDK reports its own release-file architecture, not Go's GOARCH name.
    jdk_arch = {"amd64": {"amd64", "x86_64", "x64"}, "arm64": {"arm64", "aarch64"}}.get(f.arch, {f.arch})
    require(all(j["arch"] in jdk_arch for j in jdks), f"advertised JDK architecture is not {f.arch}: {jdks}")
    selected = json.loads(f.k8s("jdk-list-selector-json", kc, "jdk", "list", "--selector",
                                "brewlet.sh/e2e-pool=live", "--output", "json").stdout)
    require(selected == jdks, "pool selector changed the inventory of the only pool")
    empty = f.k8s("jdk-list-selector-empty", kc, "jdk", "list", "--selector", "brewlet.sh/e2e-pool=wf-none",
                  "--output", "json").stdout
    require(json.loads(empty) == [], f"non-matching selector returned inventory: {empty}")
    table = f.k8s("jdk-list-table-empty", kc, "jdk", "list", "--selector", "brewlet.sh/e2e-pool=wf-none").stdout
    require(table.startswith("No Brewlet JDK inventory found"), "empty inventory table lacks guidance")
    wide = f.k8s("jdk-list-wide", kc, "jdk", "list", "--output", "wide").stdout.splitlines()
    require(wide[0].split() == ["NODE", "VENDOR", "DISTRIBUTION", "MAJOR", "VERSION", "ARCH"] and
            len(wide) == 1 + len(advertised) and
            all(any(re.fullmatch(rf"{re.escape(f.node)}\s+{re.escape(j['vendor'])}\s+{j['distribution']}\s+"
                                 rf"{j['feature']}\s+{re.escape(j['version'])}\s+{j['arch']}", r) for r in wide[1:])
                for j in advertised), f"wide inventory rows differ from the node annotation: {wide}")
    table = f.k8s("jdk-list-table", kc, "jdk", "list").stdout.splitlines()
    require(table[0].split()[:2] == ["VENDOR", "DISTRIBUTION"] and len(table) == 1 + len(expected) and
            all(r.split()[-1] == "1" for r in table[1:]), f"aggregated JDK table: {table}")
    quiet_failure(f.k8s("jdk-list-yaml-rejected", kc, "jdk", "list", "--output", "yaml", expect="fail"),
                  'invalid --output "yaml"', "jdk list --output yaml")
    f.record("k8s-jdk-list-node-inventory", {"node": f.node, "jdks": jdks, "formats": ["json", "wide", "table"],
             "selectors": {"brewlet.sh/e2e-pool=live": len(selected), "brewlet.sh/e2e-pool=wf-none": 0}})

    launchers = json.loads(f.k8s("launcher-list-json", kc, "launcher", "list", "--output", "json").stdout)
    names = sorted({n.strip() for n in annotations.get("brewlet.sh/launchers", "").split(",") if n.strip()})
    require(launchers == [{"name": n, "nodes": [f.node]} for n in names],
            f"launcher list differs from brewlet.sh/launchers {names}: {launchers}")
    # Every JDK supplies the implicit "java" launcher next to any declared ones.
    declared = sorted({"java", *(l["name"] for l in profile["spec"].get("launchers", []))})
    require(names == declared, f"advertised launchers {names} differ from the live NodeProfile declaration {declared}")
    wide = f.k8s("launcher-list-wide", kc, "launcher", "list", "--output", "wide").stdout.splitlines()
    require(wide[0].split() == ["LAUNCHER", "NODE"] and len(wide) == 1 + len(names), f"launcher wide table: {wide}")
    selected = json.loads(f.k8s("launcher-list-selector-empty", kc, "launcher", "list", "--selector",
                                "brewlet.sh/e2e-pool=wf-none", "--output", "json").stdout)
    require(selected == [], "non-matching launcher selector returned inventory")
    f.record("k8s-launcher-list-node-inventory", {"launchers": launchers, "nodeAnnotation": names,
             "declared": profile["spec"].get("launchers", [])})

    rows = json.loads(f.k8s("profile-list-json", kc, "profile", "list", "--output", "json").stdout)
    declared = sorted(p["metadata"]["name"] for p in f.get("nodeprofile")["items"])
    require([r["name"] for r in rows] == declared == ["live"], f"profile list names {rows} vs {declared}")
    row = rows[0]
    ready = h.condition(profile, "Ready") or {}
    require(row["ready"] and ready.get("status") == "True" and row["reason"] == ready.get("reason") and
            row["generation"] == row["observedGeneration"] == profile["metadata"]["generation"] and
            row["assignedNodes"] == row["readyNodes"] == 1 and row["spec"] == profile["spec"] and
            "managedBy" not in row, f"profile list row differs from the live profile: {row}")
    table = f.k8s("profile-list-table", kc, "profile", "list").stdout.splitlines()
    require(table[0].split()[0] == "PROFILE" and len(table) == 2 and
            re.match(r"^live\s+live\s+temurin-21\s+true\s+1/1\s", table[1]), f"profile table: {table}")

    report = json.loads(f.k8s("profile-inspect-json", kc, "profile", "inspect", "live", "--output", "json").stdout)
    owners = [n["metadata"]["name"] for n in f.get("nodes", "-l", f"brewlet.sh/owner-uid={profile['metadata']['uid']}")["items"]]
    require(report["profile"]["name"] == "live" and report["profile"]["spec"] == profile["spec"] and
            report["profile"]["ready"] and owners == [f.node] and [n["name"] for n in report["nodes"]] == owners,
            f"profile inspect does not show the uid-claimed node: {report}")
    claimed = report["nodes"][0]
    require(claimed["profile"] == "live" and claimed["runtimeReady"] and claimed["nodeReady"] and
            not claimed.get("error") and "temurin-21" in claimed.get("advertisedJdks", "").split(","),
            f"claimed node summary incomplete: {claimed}")
    for name, args in (("profile-inspect-yaml", ["--output", "yaml"]), ("profile-inspect-default", [])):
        text = f.k8s(name, kc, "profile", "inspect", "live", *args).stdout
        require(re.search(r"^profile:$", text, re.M) and re.search(r"^nodes:$", text, re.M) and
                re.search(r"^  name: live$", text, re.M) and
                re.search(rf"^\s*(- )?name: {re.escape(f.node)}$", text, re.M) and not text.lstrip().startswith("{"),
                f"{name} is not the YAML report: {text[:500]}")
    quiet_failure(f.k8s("profile-inspect-missing", kc, "profile", "inspect", "wf-missing-profile", expect="fail"),
                  "NotFound", "profile inspect of a missing profile")
    f.record("k8s-profile-list-and-inspect", {"profiles": [r["name"] for r in rows], "uid": profile["metadata"]["uid"],
             "claimedNodes": owners, "formats": {"list": ["json", "table"], "inspect": ["json", "yaml", "default"]}})

    rejected = {}
    for command in (["jdk", "list"], ["launcher", "list"], ["profile", "list"], ["profile", "inspect", "live"],
                    ["jdk", "add", "--profile", "live", *JDK17, "--dry-run"],
                    ["launcher", "add", "--profile", "live", *JAZ, "--dry-run"]):
        verb = " ".join(command[:2])
        result = f.k8s(f"{verb.replace(' ', '-')}-namespace-rejected", alpha, *command, "--namespace", APP_NS,
                       expect="fail")
        quiet_failure(result, f'--namespace is not supported by "{verb}"', f"{verb} --namespace")
        rejected[verb] = result.stderr.strip()
    result = f.k8s("jdk-list-root-namespace-rejected", kc, "--namespace", APP_NS, "jdk", "list", expect="fail")
    quiet_failure(result, '--namespace is not supported by "jdk list"', "root --namespace on jdk list")
    f.record("k8s-cluster-scoped-commands-reject-namespace", rejected)


def inventory_status(f, kc, alpha, profile, annotations):
    report = json.loads(f.k8s("status-json", alpha, "status", "--output", "json").stdout)
    deployments = {d["metadata"]["name"]: d for d in f.get("deployments", "-n", "brewlet")["items"]}
    components = {c["name"]: c for c in report["components"]}
    require(report["healthy"] and report["namespace"] == "brewlet" and
            list(components) == ["brewlet-operator", "brewlet-admission"], f"status report: {report}")
    for name, component in components.items():
        live = deployments[name]
        desired = live["spec"].get("replicas", 1)
        require(component["present"] and component["ready"] and desired >= 1 and
                component["desired"] == component["updated"] == component["available"] == desired ==
                live["status"].get("availableReplicas") and
                component["generation"] == component["observedGeneration"] == live["metadata"]["generation"],
                f"{name} rollout differs from the Deployment: {component}")
    require([p["name"] for p in report["profiles"]] == ["live"] and report["profiles"][0]["ready"],
            f"status profiles: {report['profiles']}")
    nodes = report["nodes"]
    require(len(nodes) == 1 and nodes[0]["name"] == f.node and nodes[0]["profile"] == "live" and
            nodes[0]["runtimeReady"] and nodes[0]["nodeReady"] and not nodes[0].get("error") and
            nodes[0].get("advertisedJdks") == annotations.get("brewlet.sh/jdks") and
            nodes[0].get("profileGeneration") == annotations.get("brewlet.sh/profile-generation"),
            f"status node summary differs from the node: {nodes}")
    explicit = json.loads(f.k8s("status-explicit-namespace", kc, "status", "--namespace", "brewlet",
                                "--output", "json").stdout)
    require(explicit["namespace"] == "brewlet" and explicit["healthy"], f"explicit status namespace: {explicit}")
    table = f.k8s("status-table", kc, "status").stdout
    require(re.search(r"^namespace: brewlet$", table, re.M) and
            all(re.search(rf"^{n}: present=true ready=true updated=(\d+)/\1 available=\1$", table, re.M)
                for n in components) and re.search(r"^live\s+live\s+temurin-21\s+true\s+1/1", table, re.M) and
            re.search(rf"^{re.escape(f.node)}\s+live\s+\d+\s+true\s+true\s+false\s", table, re.M),
            f"status table incomplete: {table}")
    wrong = f.k8s("status-wrong-namespace", kc, "status", "--namespace", APP_NS, "--output", "json", expect="fail")
    missing = json.loads(wrong.stdout)
    require(f'no Brewlet control plane found in namespace "{APP_NS}"; pass --namespace' in wrong.stderr and
            missing["namespace"] == APP_NS and not missing["healthy"] and
            not any(c["present"] for c in missing["components"]), f"wrong-namespace status: {wrong.stderr}")
    f.record("k8s-status-control-plane-and-profiles", {"namespace": "auto-discovered brewlet (kubeconfig ns wf-alpha)",
             "components": components, "profiles": ["live"], "node": nodes[0],
             "wrongNamespace": wrong.stderr.strip(), "formats": ["json", "table"]})

    names = ["cluster-context", "api-server", "runtimeclass", "javaapplication-crd", "brewlet-nodes",
             "jdk-inventory", "developer-rbac"]
    distinct = len(node_jdks(f))
    checks = {}
    for label, namespace, args in (("doctor-json-context-namespace", APP_NS, []),
                                   ("doctor-json-explicit-namespace", OTHER_NS, ["--namespace", OTHER_NS])):
        doctor = json.loads(f.k8s(label, alpha, "doctor", *args, "--output", "json").stdout)
        by_name = {c["name"]: c for c in doctor["checks"]}
        require([c["name"] for c in doctor["checks"]] == names and all(c["status"] == "pass" for c in doctor["checks"]),
                f"{label} has failing or missing checks: {doctor}")
        require(by_name["cluster-context"]["detail"] == "wf-target" and
                by_name["runtimeclass"]["detail"] == "runtimeclass.node.k8s.io/brewlet" and
                by_name["javaapplication-crd"]["detail"].endswith("/javaapplications.apps.brewlet.sh") and
                by_name["brewlet-nodes"]["detail"] == "1 schedulable Brewlet-ready node(s)" and
                by_name["jdk-inventory"]["detail"] == f"{distinct} distinct JDK runtime(s) advertised" and
                by_name["developer-rbac"]["detail"] ==
                f'can create JavaApplication resources in namespace "{namespace}"', f"{label} details: {doctor}")
        checks[label] = doctor["checks"]
    table = f.k8s("doctor-table", alpha, "doctor").stdout.splitlines()
    require([line.split()[:2] for line in table] == [["[PASS]", n] for n in names], f"doctor table: {table}")
    restricted_doctor(f, names)
    f.record("k8s-doctor-checks-and-namespace", {"contextNamespace": checks["doctor-json-context-namespace"],
             "explicitNamespace": checks["doctor-json-explicit-namespace"], "formats": ["json", "table"]})


def restricted_doctor(f, names):
    name = "wf-doctor"
    f.apply({"apiVersion": "v1", "kind": "ServiceAccount", "metadata": {"name": name, "namespace": APP_NS}})
    f.apply({"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole", "metadata": {"name": name},
             "rules": [{"apiGroups": ["node.k8s.io"], "resources": ["runtimeclasses"], "verbs": ["get"]},
                       {"apiGroups": ["apiextensions.k8s.io"], "resources": ["customresourcedefinitions"],
                        "verbs": ["get"]},
                       {"apiGroups": [""], "resources": ["nodes"], "verbs": ["get", "list"]}]})
    f.apply({"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding", "metadata": {"name": name},
             "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": name},
             "subjects": [{"kind": "ServiceAccount", "name": name, "namespace": APP_NS}]})
    try:
        token = f.secret(f.kube("create", "token", name, "-n", APP_NS, "--duration=10m").stdout.strip())
        kc = f.write_kubeconfig("doctor-restricted", h.derive_kubeconfig(
            f.base_kubeconfig(), context="wf-doctor", namespace=APP_NS, token=token))
        result = f.k8s("doctor-restricted-identity", kc, "doctor", "--output", "json", expect="fail")
        doctor = json.loads(result.stdout)
        statuses = {c["name"]: c["status"] for c in doctor["checks"]}
        require(list(statuses) == names and statuses.pop("developer-rbac") == "fail" and
                set(statuses.values()) == {"pass"} and "doctor found one or more blocking checks" in result.stderr,
                f"restricted doctor did not isolate the RBAC failure: {doctor} {result.stderr}")
        rbac = doctor["checks"][-1]
        require(rbac.get("remediation"), "failing developer-rbac check has no remediation")
        f.record("k8s-doctor-restricted-identity-fails", {"identity": f"serviceaccount {APP_NS}/{name}",
                 "missing": "create javaapplications", "developerRBAC": rbac})
    finally:
        f.kube("delete", "clusterrolebinding,clusterrole", name, "--ignore-not-found")
        f.kube("delete", "serviceaccount", name, "-n", APP_NS, "--ignore-not-found")


def inspect_app(f, alpha, image):
    app = f.get("javaapplication", "wf-app", "-n", APP_NS)
    report = json.loads(f.k8s("inspect-app-json", alpha, "inspect", "app", "wf-app", "--output", "json").stdout)
    owned = sorted(d["metadata"]["name"] for d in f.get("deployments", "-n", APP_NS)["items"]
                   if any(o.get("uid") == app["metadata"]["uid"] for o in d["metadata"].get("ownerReferences", [])))
    ready_pods = [p for p in report["pods"] if p["ready"]]
    require(report["name"] == "wf-app" and report["namespace"] == APP_NS and report["ready"] and
            report["image"] == app["spec"]["artifact"]["image"] == image and report["jdkRequest"] == "temurin-21" and
            report["launcher"] == "java" and owned and [d["name"] for d in report["deployments"]] == owned and
            all(d["ready"] for d in report["deployments"]) and ready_pods and
            all(p["node"] == f.node for p in report["pods"]) and
            all(p["jdkRequest"] == "temurin-21" for p in ready_pods),
            f"inspect app report differs from the live application: {report}")
    require(isinstance(report["events"], list) and (h.condition(app, "Ready") or {}).get("status") == "True",
            "inspect app events/conditions missing")
    text = f.k8s("inspect-app-default", alpha, "inspect", "app", "wf-app").stdout
    require(re.search(rf"^namespace: {APP_NS}$", text, re.M) and re.search(r"^ready: true$", text, re.M),
            f"inspect app default output is not the YAML report: {text[:500]}")
    other = json.loads(f.k8s("inspect-app-explicit-namespace", alpha, "inspect", "app", "wf-app", "--namespace",
                             OTHER_NS, "--output", "json").stdout)
    require(other["namespace"] == OTHER_NS and not other["ready"] and
            all(p["node"] == f.node for p in other["pods"]), f"--namespace not targeted: {other}")
    quiet_failure(f.k8s("inspect-app-missing", alpha, "inspect", "app", "wf-missing-app", expect="fail"),
                  "NotFound", "inspect app of a missing application")
    f.record("k8s-inspect-app-workloads", {"default": {k: report[k] for k in ("namespace", "ready", "jdkRequest")},
             "deployments": owned, "pods": report["pods"], "explicit": {k: other[k] for k in ("namespace", "ready",
                                                                                           "reason")}})


def profile_add(f, kc, uid):
    live = f.get("nodeprofile", "live")
    before = snapshot(f, "live")

    def add(name, kind, *args, expect=0):
        result = f.k8s(name, kc, kind, "add", "--profile", "live", *args, expect=expect)
        unchanged(f, "live", before, name)
        return result

    def check_doc(doc, what):
        meta = doc["metadata"]
        require(doc["apiVersion"] == "node.brewlet.sh/v1alpha1" and doc["kind"] == "NodeProfile" and
                meta["name"] == "live" and "status" not in doc and
                not {"uid", "resourceVersion", "generation", "managedFields", "finalizers"} & set(meta) and
                doc["spec"]["nodePool"] == live["spec"]["nodePool"], f"{what} is not a clean declaration: {doc}")

    jdk17 = {"distribution": "temurin", "feature": 17, "source": {"image": JDK_IMAGE, "javaHome": "/opt/java/openjdk"}}
    client = add("jdk-add-client-dry-run", "jdk", *JDK17, "--dry-run", "--output", "json")
    doc = json.loads(client.stdout)
    check_doc(doc, "jdk add --dry-run")
    require(doc["spec"]["jdks"] == live["spec"]["jdks"] + [jdk17], f"client dry run jdks: {doc['spec']['jdks']}")
    require("Client dry run succeeded; no inventory was changed. Server validation was not performed." in client.stderr,
            "client dry-run notice missing from stderr")
    explicit = json.loads(add("jdk-add-client-dry-run-explicit", "jdk", *JDK17, "--dry-run=client", "--output",
                              "json").stdout)
    require(explicit == doc, "--dry-run=client differs from bare --dry-run")
    server = add("jdk-add-server-dry-run", "jdk", *JDK17, "--dry-run=server", "--output", "json")
    served = json.loads(server.stdout)
    check_doc(served, "jdk add --dry-run=server")
    require([(j["distribution"], j["feature"]) for j in served["spec"]["jdks"]] == [("temurin", 21), ("temurin", 17)] and
            served["spec"]["jdks"][1]["source"] == jdk17["source"] and
            "Server dry run succeeded; no inventory was changed." in server.stderr, f"server dry run: {served}")
    yaml_out = add("jdk-add-server-dry-run-yaml", "jdk", *JDK17, "--dry-run=server").stdout
    require(re.search(r"^kind: NodeProfile$", yaml_out, re.M) and re.search(r"^\s+feature: 17$", yaml_out, re.M),
            "server dry run YAML output incomplete")
    conflict = add("jdk-add-conflict", "jdk", "--distribution", "temurin", "--feature", "21", "--image", JDK_IMAGE,
                   "--java-home", "/opt/java/other", "--dry-run", expect="fail")
    quiet_failure(conflict, 'jdks "temurin-21" already exists with a different source; use --replace deliberately',
                  "conflicting jdk add")
    replaced = json.loads(add("jdk-add-replace-dry-run", "jdk", "--distribution", "temurin", "--feature", "21",
                              "--image", JDK_IMAGE, "--java-home", "/opt/java/other", "--replace", "--dry-run",
                              "--output", "json").stdout)
    require([j["source"]["javaHome"] for j in replaced["spec"]["jdks"]] == ["/opt/java/other"],
            f"--replace dry run: {replaced['spec']['jdks']}")
    quiet_failure(add("jdk-add-tagged-image", "jdk", "--distribution", "temurin", "--feature", "17", "--image",
                      "docker.io/library/eclipse-temurin:17", "--java-home", "/opt/java/openjdk", "--dry-run=server",
                      expect="fail"), "must contain exactly one digest separator", "tag-only JDK image")
    quiet_failure(add("jdk-add-bad-dry-run-mode", "jdk", *JDK17, "--dry-run=none", expect="fail"),
                  "--dry-run accepts client or server", "invalid --dry-run mode")
    f.record("k8s-jdk-add-dry-runs-nonmutating", {"profile": before, "client": doc["spec"]["jdks"],
             "server": served["spec"]["jdks"], "conflictRefused": True, "replaceDryRun": True})

    jaz = {"name": "jaz", "source": {"image": JDK_IMAGE, "path": "/opt/java/openjdk/bin/java"}}
    client = add("launcher-add-client-dry-run", "launcher", *JAZ, "--dry-run", "--output", "json")
    ldoc = json.loads(client.stdout)
    check_doc(ldoc, "launcher add --dry-run")
    require(ldoc["spec"].get("launchers") == live["spec"].get("launchers", []) + [jaz] and
            ldoc["spec"]["jdks"] == live["spec"]["jdks"] and "Server validation was not performed" in client.stderr,
            f"launcher client dry run: {ldoc['spec']}")
    server = add("launcher-add-server-dry-run", "launcher", *JAZ, "--dry-run=server", "--output", "json")
    lserved = json.loads(server.stdout)
    check_doc(lserved, "launcher add --dry-run=server")
    require(lserved["spec"].get("launchers") == [jaz] and "Server dry run succeeded" in server.stderr,
            f"launcher server dry run: {lserved['spec']}")
    quiet_failure(add("launcher-add-java-refused", "launcher", "--name", "java", "--image", JDK_IMAGE, "--path",
                      "/opt/java/openjdk/bin/java", "--dry-run", expect="fail"),
                  "java is supplied by each JDK", "launcher add java")
    quiet_failure(add("launcher-add-relative-path", "launcher", "--name", "jaz", "--image", JDK_IMAGE, "--path",
                      "bin/java", "--dry-run=server", expect="fail"), "must be a clean absolute path below /",
                  "relative launcher path")
    f.record("k8s-launcher-add-dry-runs-nonmutating", {"profile": before, "client": ldoc["spec"]["launchers"],
             "server": lserved["spec"]["launchers"], "javaRefused": True})

    source = f.private / "live-nodeprofile.json"
    h.write_private(source, json.dumps(live))
    digest = sha256(source)
    offline = json.loads(add("jdk-add-offline-file", "jdk", *JDK17, "--file", source, "--dry-run", "--output",
                             "json").stdout)
    check_doc(offline, "jdk add --file")
    require(offline["spec"] == doc["spec"], "offline file input differs from the live client dry run")
    yaml_file = f.private / "live-server-dry-run.yaml"
    h.write_private(yaml_file, yaml_out)
    roundtrip = json.loads(add("launcher-add-offline-yaml-file", "launcher", *JAZ, "--file", yaml_file, "--dry-run",
                               "--output", "json").stdout)
    require(roundtrip["spec"]["jdks"] == served["spec"]["jdks"] and roundtrip["spec"].get("launchers") == [jaz],
            f"YAML file input does not round-trip the server dry-run declaration: {roundtrip['spec']}")
    refusals = {}
    for flags in ([], ["--dry-run=server"]):
        name = "jdk-add-offline-file-requires-client" + "".join(flags).replace("-", "_").replace("=", "_")
        result = add(name, "jdk", *JDK17, "--file", source, *flags, expect="fail")
        quiet_failure(result, "--file and --values require --dry-run=client", f"--file with {flags}")
        refusals[" ".join(flags) or "live"] = result.stderr.strip()
    require(sha256(source) == digest and yaml_file.read_text() == yaml_out, "offline input files were modified")
    f.record("k8s-profile-add-offline-file-input", {"jsonFile": source.name, "yamlFile": yaml_file.name,
             "sha256Unchanged": digest, "refusals": refusals})

    f.kube("label", "nodeprofile", "live", "app.kubernetes.io/managed-by=Helm")
    try:
        before = snapshot(f, "live")
        refused = {}
        for name, kind, args, flags in (("jdk-add-helm-live", "jdk", JDK17, []),
                                        ("jdk-add-helm-server-dry-run", "jdk", JDK17, ["--dry-run=server"]),
                                        ("launcher-add-helm-live", "launcher", JAZ, [])):
            result = add(name, kind, *args, *flags, expect="fail")
            quiet_failure(result, 'profile "live" is managed by Helm; edit its source of truth', name)
            refused[name] = result.stderr.strip()
        preview = add("jdk-add-helm-client-dry-run", "jdk", *JDK17, "--dry-run", "--output", "json")
        require(json.loads(preview.stdout)["spec"]["jdks"][-1] == jdk17 and
                "Profile is managed by Helm. Update its source of truth" in preview.stderr,
                "client preview of a Helm-managed profile lacks the ownership notice")
    finally:
        f.kube("label", "nodeprofile", "live", "app.kubernetes.io/managed-by-")
    require(f.get("nodeprofile", "live")["metadata"]["uid"] == uid, "live profile was replaced")
    f.record("k8s-profile-add-refuses-helm-managed", {"label": "app.kubernetes.io/managed-by=Helm", **refused,
             "clientPreview": "allowed with ownership notice"})


def disposable_add(f, kc):
    """Real (non-dry-run) additions on a profile whose pool matches no node."""
    # rollout is explicit like `live`: the operator's finalizer Update otherwise
    # serializes `rollout: {}` under its own field manager ("entry"), which
    # `jdk|launcher add` then refuses as foreign ownership (microsoft/brewlet#210).
    f.apply({"apiVersion": "node.brewlet.sh/v1alpha1", "kind": "NodeProfile", "metadata": {"name": INVENTORY_PROFILE},
             "spec": {"nodePool": {"key": "brewlet.sh/e2e-pool", "names": ["wf-inventory-unclaimed"]},
                      "jdks": [{"distribution": "temurin", "feature": 21,
                                "source": {"image": JDK_IMAGE, "javaHome": "/opt/java/openjdk"}}],
                      "rollout": {"validate": True, "containerdRestart": "validated"}}})
    wait(f"{INVENTORY_PROFILE} carries the operator finalizer", lambda: f.get("nodeprofile", INVENTORY_PROFILE)
         ["metadata"].get("finalizers"), timeout=60)
    try:
        first = f.get("nodeprofile", INVENTORY_PROFILE)["metadata"]
        jaz = {"name": "jaz", "source": {"image": JDK_IMAGE, "path": "/opt/java/openjdk/bin/java"}}
        result = f.k8s("launcher-add-live", kc, "launcher", "add", "--profile", INVENTORY_PROFILE, *JAZ,
                       "--output", "json")
        printed = json.loads(result.stdout)
        after = f.get("nodeprofile", INVENTORY_PROFILE)
        require("Profile update accepted; provisioning is asynchronous." in result.stderr and
                printed["spec"]["launchers"] == after["spec"]["launchers"] == [jaz] and
                after["metadata"]["uid"] == first["uid"] and after["metadata"]["generation"] == first["generation"] + 1,
                f"live launcher add did not persist exactly once: {after['metadata']} {after['spec']}")
        result = f.k8s("jdk-add-live", kc, "jdk", "add", "--profile", INVENTORY_PROFILE, *JDK17)
        after = f.get("nodeprofile", INVENTORY_PROFILE)
        managers = sorted({m["manager"] for m in f.get("nodeprofile", INVENTORY_PROFILE, "--show-managed-fields")
                           ["metadata"].get("managedFields", [])})
        require(re.search(r"^kind: NodeProfile$", result.stdout, re.M) and
                [(j["distribution"], j["feature"]) for j in after["spec"]["jdks"]] == [("temurin", 21), ("temurin", 17)]
                and after["spec"]["launchers"] == [jaz] and after["metadata"]["generation"] == first["generation"] + 2
                and "brewlet" in managers, f"live jdk add: {after['spec']} managers={managers}")
        live_rows = {r["name"]: r for r in json.loads(f.k8s("profile-list-after-add", kc, "profile", "list",
                                                             "--output", "json").stdout)}
        require(live_rows[INVENTORY_PROFILE]["spec"] == after["spec"] and live_rows["live"]["ready"],
                "profile list does not reflect the live addition")
        updated = {"generation": after["metadata"]["generation"], "jdks": after["spec"]["jdks"],
                   "launchers": after["spec"]["launchers"], "fieldManagers": managers}
    finally:
        f.kube("delete", "nodeprofile", INVENTORY_PROFILE, "--ignore-not-found", "--wait=true", "--timeout=180s")
    require(f.kube("get", "nodeprofile", INVENTORY_PROFILE, check=False).returncode != 0,
            "disposable profile still exists")
    require(f.get("node", f.node)["metadata"]["labels"].get("brewlet.sh/owner-uid") ==
            f.get("nodeprofile", "live")["metadata"]["uid"], "disposable profile disturbed the live claim")
    f.record("k8s-profile-add-live-update", {"profile": INVENTORY_PROFILE, "pool": "matches no node", **updated})


def install_guards(f, kc):
    def releases():
        return sorted((r["name"], r["namespace"]) for r in json.loads(
            f.run(["helm", "--kubeconfig", f.kubeconfig, "--kube-context", f.context, "list", "-A",
                   "-o", "json"]).stdout))

    before = releases()
    values = f.private / "install-values.json"
    h.write_private(values, json.dumps({"defaultProfile": {"enabled": False}}))
    guard = f.k8s("install-existing-crds", kc, "install", "--version", "0.1.0", "-f", values, expect="fail")
    quiet_failure(guard, "Brewlet CRDs already exist; install is fresh-install-only", "install on an installed cluster")
    require('Installing Brewlet chart 0.1.0 as release "brewlet" in namespace "brewlet" (current context)' in guard.stderr
            and "Pulling" not in guard.stderr, "install did not report its default target before refusing")
    other = f.k8s("install-existing-crds-namespace", kc, "install", "--namespace", "wf-install", "--release",
                  "wf-other", "--version", "0.1.0", "--values", values, expect="fail", context="wf-target")
    quiet_failure(other, "Brewlet CRDs already exist", "install into another namespace")
    require('as release "wf-other" in namespace "wf-install" (context "wf-target")' in other.stderr,
            "install --namespace/--release/--context not reflected in its target")
    require(f.kube("get", "namespace", "wf-install", check=False).returncode != 0,
            "refused install created its namespace")
    invalid = {}
    for name, args, needle in (
        ("install-missing-values", ["--version", "0.1.0"], "at least one --values/-f file is required"),
        ("install-missing-values-dry-run", ["--version", "0.1.0", "--dry-run"], "at least one --values/-f file"),
        ("install-version-range", ["--version", "latest", "-f", values], "--version must be an exact chart version"),
        ("install-unreadable-values", ["--version", "0.1.0", "-f", f.private / "missing-values.yaml", "--dry-run"],
         "missing-values.yaml"),
        ("install-invalid-namespace", ["--namespace", "Not_A_Namespace", "--version", "0.1.0", "-f", values,
                                       "--dry-run"], "--namespace"),
    ):
        result = f.k8s(name, kc, "install", *args, expect="fail")
        quiet_failure(result, needle, name)
        invalid[name] = result.stderr.strip()
    require(releases() == before, f"install guards changed Helm releases: {before} -> {releases()}")
    f.record("k8s-install-validation-and-fresh-install-guard", {"helmReleases": before, "existingCRDs": guard.stderr.strip(),
             "namespaceTarget": "wf-install (not created)", **invalid,
             "dryRunRender": "not exercised: renders the released OCI chart from ghcr.io, not the checkout"})


# ---- 4. Maven publication and deployment handoff ----------------------------

def maven_scenario(f):
    f.ensure_namespace(MAVEN_NS)
    base = f.base_kubeconfig()
    decoy = f"https://127.0.0.1:{h.unused_port()}"
    kc = f.write_kubeconfig("maven", h.derive_kubeconfig(base, context="wf-maven-target", decoy_server=decoy))
    settings = f.private / "settings.xml"
    target = {"kubeconfig": kc.name, "context": "wf-maven-target", "namespace": MAVEN_NS,
              "currentContext": "decoy (unreachable)"}

    def mvn(name, project, goals, props, expect=0, settings_file=settings, extra=()):
        argv = [*f.maven_base, "--settings", settings_file, *extra, "-f", project / "pom.xml", *goals,
                *(f"-D{k}={v}" for k, v in props.items())]
        return f.cmd(name, argv, expect=expect, timeout=900, target={"registry": f.registry})

    def props(app, **extra):
        values = {"brewlet.image": f"{f.registry}/wf/maven:{app}"}
        values.update(extra)
        return values

    project = maven_app(f, "maven-app")
    mvn("maven-push", project, ["package", f"{f.plugin}:push"], props("wf-maven"))
    require(not (project / "target/brewlet/javaapplication.yaml").exists(),
            "publication must not generate deployment configuration")
    require(f.kube("get", "javaapplication", "wf-maven", "-n", MAVEN_NS, check=False).returncode != 0,
            "publication must not deploy the application")
    push_file = project / "target/brewlet/push.json"
    handoff = json.loads(push_file.read_text())
    published = h.verify_published(f.registry, "wf/maven", "wf-maven")
    h.validate_handoff(handoff, f.registry, "wf/maven", "wf-maven", published["digest"])
    f.application("wf-maven", MAVEN_NS, handoff["deployImage"])
    f.k8s("maven-image-wait", kc, "app", "wait", "wf-maven", "--namespace", MAVEN_NS,
           "--wait-timeout", "5m", context="wf-maven-target")
    applied = f.get("javaapplication", "wf-maven", "-n", MAVEN_NS)
    require(applied["spec"]["artifact"]["image"] == handoff["deployImage"],
            "applied image differs from push.json")
    ready = h.condition(applied, "Ready") or {}
    require(ready.get("status") == "True" and ready.get("observedGeneration") == applied["metadata"]["generation"],
            "applied JavaApplication is not Ready for its current generation")
    require("Hello from a JAR" in f.hello(MAVEN_NS, "wf-maven"), "deployed workload did not respond")
    require(f.kube("get", "javaapplication", "wf-maven", "-n", "default", check=False).returncode != 0,
            "deploy also targeted the default namespace")
    uid = json.loads(run(["kubectl", "--kubeconfig", kc, "--context", "wf-maven-target", "get", "namespace",
                          "kube-system", "-o", "json"], env=f.env).stdout)["metadata"]["uid"]
    require(uid == f.cluster_uid, "Deployment kubeconfig context is not the disposable cluster")
    f.record("maven-push-kubectl-apply-cli-ready-response", {"handoff": handoff, "index": published["digest"],
             "appliedGeneration": applied["metadata"]["generation"], "clusterUID": uid, **target})

    encrypted_push(f, mvn, props)

    before = sha256(push_file)
    live_version = f.get("javaapplication", "wf-maven", "-n", MAVEN_NS)["metadata"]["resourceVersion"]
    apps = sorted(a["metadata"]["name"] for a in f.get("javaapplication", "-n", MAVEN_NS)["items"])
    mvn("maven-push-dry-run", project, ["package", f"{f.plugin}:push"],
        props("wf-maven", **{"brewlet.dryRun": "true"}))
    mvn("maven-push-dry-run-new-tag", project, ["package", f"{f.plugin}:push"],
        props("wf-maven", **{"brewlet.dryRun": "true", "brewlet.image": f"{f.registry}/wf/maven-dryrun:v1"}))
    require(sha256(push_file) == before, "dry run rewrote push.json")
    require(h.manifest(f.registry, "wf/maven-dryrun", "v1") is None, "dry run published to the registry")
    require(f.get("javaapplication", "wf-maven", "-n", MAVEN_NS)["metadata"]["resourceVersion"] == live_version and
            sorted(a["metadata"]["name"] for a in f.get("javaapplication", "-n", MAVEN_NS)["items"]) == apps,
            "dry run mutated Kubernetes")
    f.record("maven-push-dry-run-nonmutating", {"pushJsonSHA256": before, "resourceVersion": live_version,
             "deployImage": handoff["deployImage"]})


def encrypted_push(f, mvn, props):
    user, password = f.auth
    security = f.private / "maven-security"
    security.mkdir(mode=0o700)
    master = f.secret(secrets.token_urlsafe(24))
    result = f.cmd("maven-encrypt-master", [*f.maven_base, "--encrypt-master-password", master],
                   sensitive_stdout=True)
    master_cipher = f.secret(h.encrypted(result.stdout))
    h.write_private(security / "settings-security.xml", h.settings_security(master_cipher))
    sec = f"-Dsettings.security={security / 'settings-security.xml'}"
    result = f.cmd("maven-encrypt-password", [*f.maven_base, sec, "--encrypt-password", password],
                   sensitive_stdout=True)
    cipher = f.secret(h.encrypted(result.stdout))
    wrong = f.secret(secrets.token_urlsafe(24))
    wrong_cipher = f.secret(h.encrypted(f.cmd("maven-encrypt-wrong", [*f.maven_base, sec, "--encrypt-password", wrong],
                                              sensitive_stdout=True).stdout))
    good = h.write_private(security / "settings.xml", h.maven_settings(f.auth_registry, user, cipher))
    bad = h.write_private(security / "settings-bad.xml", h.maven_settings(f.auth_registry, user, wrong_cipher))
    other_master = f.secret(h.encrypted(f.cmd("maven-encrypt-other-master", [
        *f.maven_base, "--encrypt-master-password", f.secret(secrets.token_urlsafe(24))], sensitive_stdout=True).stdout))
    h.write_private(security / "other-security.xml", h.settings_security(other_master))
    other = f"-Dsettings.security={security / 'other-security.xml'}"

    project = maven_app(f, "maven-auth")
    image = f"{f.auth_registry}/wf/maven-auth"
    mvn("maven-push-encrypted-settings", project, ["package", f"{f.plugin}:push"],
        props("wf-auth", **{"brewlet.image": f"{image}:v1"}), settings_file=good, extra=[sec])
    handoff = json.loads((project / "target/brewlet/push.json").read_text())
    published = h.verify_published(f.auth_registry, "wf/maven-auth", "v1", f.auth)
    h.validate_handoff(handoff, f.auth_registry, "wf/maven-auth", "v1", published["digest"])
    failures = {}
    for name, settings_file, flag, tag, message in (
        ("maven-push-wrong-password", bad, sec, "wrong", None),
        ("maven-push-undecryptable", good, other, "undecryptable", "Cannot decrypt settings.xml credentials for registry"),
    ):
        failing = maven_app(f, name)
        result = mvn(name, failing, ["package", f"{f.plugin}:push"], props("wf-auth", **{"brewlet.image": f"{image}:{tag}"}),
                     expect="fail", settings_file=settings_file, extra=[flag])
        require(message is None or message in result.stdout + result.stderr, f"{name} failure not explicit")
        require(not (failing / "target/brewlet/push.json").exists() and
                h.manifest(f.auth_registry, "wf/maven-auth", tag, f.auth) is None, f"{name} published content")
        failures[name] = "failed without secret disclosure"
    f.record("maven-encrypted-settings-credentials", {"handoff": handoff, "settingsSecurity": "private, generated",
                                                      **failures})


# ---- 5. profile deletion ----------------------------------------------------

def bare_pod(name, image, terminating=False):
    spec = {"runtimeClassName": "brewlet", "nodeSelector": {"brewlet.sh/jdk.temurin-21": "true"},
            "containers": [{"name": "app", "image": image, "imagePullPolicy": "IfNotPresent",
                            "resources": {"requests": {"cpu": "50m", "memory": "128Mi"},
                                          "limits": {"cpu": "500m", "memory": "256Mi"}}}]}
    if terminating:
        # Real graceful termination: the kubelet keeps the Pod in its grace period.
        spec["terminationGracePeriodSeconds"] = 600
        spec["containers"][0]["lifecycle"] = {"preStop": {"sleep": {"seconds": 590}}}
    return {"apiVersion": "v1", "kind": "Pod", "metadata": {"name": name, "namespace": PROFILE_NS,
            "annotations": {"brewlet.sh/jdk": "temurin-21"}}, "spec": spec}


class LogCapture:
    """Save Brewlet control-plane/provisioner logs while cleanup workers still exist."""

    def __init__(self, f, prefix):
        self.f, self.prefix, self.stop = f, prefix, threading.Event()
        self.thread = threading.Thread(target=self.loop, daemon=True)

    def loop(self):
        while not self.stop.is_set():
            self.capture()
            self.stop.wait(3)

    def capture(self):
        pods = self.f.kube("get", "pods", "-n", "brewlet", "-o", "json", check=False, timeout=60)
        if pods.returncode:
            return
        for pod in json.loads(pods.stdout)["items"]:
            name = pod["metadata"]["name"]
            logs = self.f.kube("logs", "-n", "brewlet", name, "--all-containers", "--tail=400",
                               check=False, timeout=60)
            if logs.returncode == 0:
                self.f.save(f"{self.prefix}-{name}.log", logs.stdout)

    def __enter__(self):
        self.thread.start()
        return self

    def __exit__(self, *_exc):
        self.stop.set()
        self.thread.join(timeout=120)
        return False


def remove_workloads(f):
    for namespace in (APP_NS, OTHER_NS, MAVEN_NS):
        f.kube("delete", "javaapplication", "--all", "-n", namespace, "--wait=true", "--timeout=180s")
    wait("all Brewlet workload Pods are gone", lambda: not f.brewlet_pods(), timeout=300)


def profile_scenario(f, image):
    remove_workloads(f)
    f.ensure_namespace(PROFILE_NS)
    kc = f.write_kubeconfig("profile", h.derive_kubeconfig(f.base_kubeconfig(), context="wf-target"))
    f.apply(bare_pod("wf-running", image))
    f.apply(bare_pod("wf-terminating", image, terminating=True))
    f.kube("wait", "--for=jsonpath={.status.phase}=Running", "pod/wf-running", "pod/wf-terminating",
           "-n", PROFILE_NS, "--timeout=300s")
    f.kube("delete", "pod", "wf-terminating", "-n", PROFILE_NS, "--wait=false")
    pod = f.get("pod", "wf-terminating", "-n", PROFILE_NS)
    require(pod["metadata"].get("deletionTimestamp") and pod["status"]["phase"] == "Running",
            "fixture Pod is not terminating inside its grace period")
    uid = f.get("nodeprofile", "live")["metadata"]["uid"]
    host_before = f.host_state()
    require(host_before["shim"] and host_before["jdkRoots"] and host_before["ownerUID"] == uid and
            host_before["jdkLabels"] and host_before["containerdRuntimeConfig"], f"profile not provisioned: {host_before}")

    f.pause_operator()
    resumed = False
    try:
        before = snapshot(f, "live")
        for flags in ([], ["--dry-run"], ["--dry-run=server"], ["--wait"]):
            result = f.k8s("profile-delete-refused-workloads" + "".join(flags).replace("-", "_"), kc,
                           "profile", "delete", "live", *flags, expect="fail")
            require('2 Java workload pod(s) still run or are terminating on nodes claimed by profile "live"' in result.stderr
                    and f"{PROFILE_NS}/wf-terminating (node {f.node}, terminating)" in result.stderr and
                    f"{PROFILE_NS}/wf-running (node {f.node})" in result.stderr and not result.stdout.strip(),
                    f"workload guard incomplete for {flags}")
            unchanged(f, "live", before, f"refused delete {flags}")
        report = json.loads(f.k8s("profile-delete-yes-server-dry-run", kc, "profile", "delete", "live", "--yes",
                                  "--dry-run=server", "--output", "json").stdout)
        require(report["dryRun"] == "server" and not report["deletionRequested"] and
                sorted(w["name"] for w in report["javaWorkloads"]) == ["wf-running", "wf-terminating"],
                f"--yes server dry run report: {report}")
        unchanged(f, "live", before, "--yes --dry-run=server")
        f.record("profile-delete-refuses-running-and-terminating-workloads", {"profile": before,
                 "terminatingPod": pod["metadata"]["deletionTimestamp"], "flags": ["", "--dry-run", "--dry-run=server", "--wait"]})

        f.kube("label", "nodeprofile", "live", "app.kubernetes.io/managed-by=Helm")
        try:
            managed = snapshot(f, "live")
            for flags in ([], ["--yes"]):
                f.k8s("profile-delete-refused-helm" + "".join(flags).replace("-", "_"), kc, "profile", "delete", "live",
                      *flags, expect="fail").stderr.index('profile "live" is managed by Helm')
                unchanged(f, "live", managed, "Helm-managed refusal")
        finally:
            f.kube("label", "nodeprofile", "live", "app.kubernetes.io/managed-by-")
        f.record("profile-delete-refuses-helm-managed", {"label": "app.kubernetes.io/managed-by=Helm"})

        restricted_identity(f, kc)
        f.kube("delete", "pod", "wf-running", "wf-terminating", "-n", PROFILE_NS, "--grace-period=0", "--force")
        wait("profile workloads are gone", lambda: not f.brewlet_pods(), timeout=180)
        before = snapshot(f, "live")
        result = f.k8s("profile-delete-client-dry-run", kc, "profile", "delete", "live", "--dry-run", "--output", "json")
        client = json.loads(result.stdout)
        require(client["dryRun"] == "client" and client["uid"] == uid and client["claimedNodes"] == [f.node] and
                not client["javaWorkloads"] and not client["deletionRequested"] and
                "Server validation was not performed" in result.stderr, f"client dry-run plan: {client}")
        server = h.yaml_top_level(f.k8s("profile-delete-server-dry-run", kc, "profile", "delete", "live",
                                        "--dry-run=server", "--output", "yaml").stdout)
        require(server.get("dryRun") == "server" and server.get("uid") == uid, f"server dry-run plan: {server}")
        unchanged(f, "live", before, "dry runs")
        f.record("profile-delete-dry-runs-nonmutating", {"profile": before, "client": client, "server": server})

        result = f.k8s("profile-delete-no-wait", kc, "profile", "delete", "live")
        require("Deletion requested; host cleanup is asynchronous" in result.stderr, "no-wait notice missing")
        timeout = f.k8s("profile-delete-attach-timeout", kc, "profile", "delete", "live", "--wait",
                        "--wait-timeout", "10s", expect="fail")
        bound = h.assert_bounded("profile cleanup wait", timeout.elapsed, 10, CLI_TOLERANCE)
        require('is already deleting; following its cleanup' in timeout.stderr and "timed out after 10s" in timeout.stderr
                and "cleanup continues in the background" in timeout.stderr, "timeout diagnostics missing")
        stuck = f.get("nodeprofile", "live")
        host_blocked = f.host_state()
        require(stuck["metadata"].get("deletionTimestamp") and FINALIZER in stuck["metadata"]["finalizers"] and
                stuck["status"].get("targets") and stuck["status"].get("conditions") and
                host_blocked["ownerUID"] == uid and host_blocked["jdkRoots"],
                "CLI timeout cancelled cleanup or stripped finalizers, status or ownership")
        f.record("profile-delete-timeout-preserves-cleanup", {**bound, "operator": "paused (controlled block)",
                 "finalizers": stuck["metadata"]["finalizers"], "host": host_blocked})

        # Attach the watch and the CLI while the operator is paused: once resumed,
        # cleanup can finish before a later watch or CLI call would observe it.
        events = f.work / "nodeprofile-live-watch.json"
        with open(events, "w") as out, open(f.work / "nodeprofile-live-watch.stderr", "w") as err:
            watcher = subprocess.Popen(["kubectl", "--kubeconfig", str(f.kubeconfig), "--context", f.context, "get",
                                        "nodeprofile", "live", "--watch", "-o", "json", "--output-watch-events"],
                                       stdout=out, stderr=err, env=f.env, start_new_session=True)
        f.children.append(watcher)
        wait("watch started", lambda: events.stat().st_size > 0, timeout=60, interval=1)
        stdout, stderr = f.work / "profile-delete-attach-wait.stdout", f.work / "profile-delete-attach-wait.stderr"
        argv = [str(a) for a in (f.cli, "k8s", "--kubeconfig", kc, "profile", "delete", "live", "--wait",
                                 "--wait-timeout", "360s")]
        started = time.monotonic()
        with open(stdout, "w") as out, open(stderr, "w") as err:
            attach = subprocess.Popen(argv, stdout=out, stderr=err, env=f.env, start_new_session=True)
        f.children.append(attach)
        f.commands.append({"name": "profile-delete-attach-wait", **h.sanitized_command(argv, {}, f.secrets),
                           "target": {"kubeconfig": Path(kc).name, "context": "current-context"},
                           "stdout": stdout.name, "stderr": stderr.name, "background": True})
        wait("profile delete --wait attaches to the deleting profile",
             lambda: "following its cleanup" in stderr.read_text(), timeout=60, interval=1)
        require(attach.poll() is None, "attached wait exited before cleanup resumed")
        with LogCapture(f, "cleanup-live"):
            f.resume_operator()
            resumed = True
            code = attach.wait(timeout=420)
        f.commands[-1].update({"exitCode": code, "elapsedSeconds": round(time.monotonic() - started, 3)})
        f.save("commands.json", f.commands)
    finally:
        if not resumed:
            f.resume_operator()

    result = stderr.read_text()
    require(code == 0, f"attached wait failed: {result[-2000:]}")
    require('is already deleting; following its cleanup' in result and 'NodeProfile "live" deleted after' in
            result, "attached wait did not follow cleanup to completion")
    time.sleep(2)  # let the watch flush the DELETED event already observed by the CLI
    watcher.terminate()
    watcher.wait(timeout=10)
    timeline = h.cleanup_sequence(h.parse_json_stream(events.read_text()), uid)
    f.save("nodeprofile-live-cleanup-timeline.json", timeline)
    cleaned = assert_host_clean(f, "live")
    f.record("profile-delete-production-cleanup", {"uid": uid, "timelineEvents": len(timeline), "host": cleaned})

    f.provision_profile("wf-second")
    second = f.get("nodeprofile", "wf-second")["metadata"]["uid"]
    require(f.host_state()["ownerUID"] == second, "second profile did not claim the node")
    with LogCapture(f, "cleanup-wf-second"):
        result = f.k8s("profile-delete-fresh-wait", kc, "profile", "delete", "wf-second", "--wait",
                       "--wait-timeout", "360s", timeout=420)
    require("Waiting for NodeProfile" in result.stderr and 'NodeProfile "wf-second" deleted after' in result.stderr,
            "fresh --wait did not report cleanup")
    f.record("profile-delete-fresh-wait-cleanup", {"uid": second, "host": assert_host_clean(f, "wf-second")})


def restricted_identity(f, kc_admin):
    name = "wf-restricted"
    f.apply({"apiVersion": "v1", "kind": "ServiceAccount", "metadata": {"name": name, "namespace": PROFILE_NS}})
    f.apply({"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole", "metadata": {"name": name},
             "rules": [{"apiGroups": ["node.brewlet.sh"], "resources": ["nodeprofiles"],
                        "verbs": ["get", "list", "delete"]},
                       {"apiGroups": [""], "resources": ["nodes"], "verbs": ["get", "list"]}]})
    f.apply({"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding", "metadata": {"name": name},
             "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": name},
             "subjects": [{"kind": "ServiceAccount", "name": name, "namespace": PROFILE_NS}]})
    try:
        token = f.secret(f.kube("create", "token", name, "-n", PROFILE_NS, "--duration=10m").stdout.strip())
        kc = f.write_kubeconfig("restricted", h.derive_kubeconfig(f.base_kubeconfig(), context="wf-restricted", token=token))
        before = snapshot(f, "live")
        result = f.k8s("profile-delete-restricted-pod-list", kc, "profile", "delete", "live", expect="fail")
        require("cannot verify that no Java workloads run on the profile's nodes" in result.stderr,
                "restricted identity did not fail closed")
        unchanged(f, "live", before, "restricted refusal")
        result = f.k8s("profile-delete-restricted-yes-server-dry-run", kc, "profile", "delete", "live", "--yes",
                       "--dry-run=server")
        unchanged(f, "live", before, "restricted --yes server dry run")
        f.record("profile-delete-restricted-identity-fails-closed", {"identity": f"serviceaccount {PROFILE_NS}/{name}",
                 "missing": "pods list", "yesServerDryRun": "succeeded without mutation"})
    finally:
        f.kube("delete", "clusterrolebinding,clusterrole", name, "--ignore-not-found")
        f.kube("delete", "serviceaccount", name, "-n", PROFILE_NS, "--ignore-not-found")


def assert_host_clean(f, profile):
    state = f.host_state()
    require(not state["shim"] and not state["jdkRoots"] and not state["containerdRuntimeConfig"] and
            not state["ownerUID"] and not state["jdkLabels"], f"host cleanup incomplete for {profile}: {state}")
    require(f.kube("get", "nodeprofile", profile, check=False).returncode != 0, f"{profile} still exists")
    leftovers = [d["metadata"]["name"] for d in f.get("daemonsets", "-n", "brewlet")["items"]
                 if profile in d["metadata"]["name"]]
    require(not leftovers, f"profile workers remain: {leftovers}")
    return state


def assert_no_leaks(f):
    leaks = {"javaApplications": [a["metadata"]["name"] for a in f.get("javaapplication", "-A")["items"]],
             "brewletPods": [p["metadata"]["name"] for p in f.brewlet_pods()],
             "nodeProfiles": [p["metadata"]["name"] for p in f.get("nodeprofile")["items"]]}
    require(not any(leaks.values()), f"test resources leaked: {leaks}")
    f.record("no-leaked-test-resources", leaks)


def main():
    with CheckoutFixture() as fixture:
        image, published = push_scenario(fixture)
        app_scenario(fixture, image, published)
        inventory_scenario(fixture, image)
        maven_scenario(fixture)
        profile_scenario(fixture, image)
        assert_no_leaks(fixture)


if __name__ == "__main__":
    main()
