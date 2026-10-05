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
  maven     `mvn package brewlet:deploy` (push -> manifest -> apply -> Ready ->
            response, wait opt-out, timeouts, encrypted settings, dry run)
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

from common import JDK_IMAGE, OWNER_LABEL, REGISTRY_IMAGE, ROOT, owned_container, run, sha256, wait
from checkout import CheckoutFixture as CheckoutRuntimeFixture
import workflows_helpers as h

APP_NS, OTHER_NS, MAVEN_NS, PROFILE_NS = "wf-alpha", "wf-beta", "wf-maven", "wf-profile"
OPERATOR = ["deployment/brewlet-operator", "-n", "brewlet"]
SHIM_PATHS = ("/opt/brewlet/bin/containerd-shim-brewlet-v2", "/usr/local/bin/containerd-shim-brewlet-v2")
JDK_ROOT = "/opt/brewlet/jdks"
FINALIZER = "node.brewlet.sh/cleanup"
# Documented tolerance above a requested deadline: process start, one in-flight
# kubectl call and output flushing. Maven adds JVM shutdown and the 5s
# process-tree termination grace.
CLI_TOLERANCE, MAVEN_TOLERANCE = 15, 20


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

    def build_command(self, name, argv, **kwargs):
        return self.cmd(name, argv, **kwargs)

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


def push_scenario(f):
    app = build_app(f, "push-app")
    f.cmd("package-demo", [*f.maven_args, "-f", app / "pom.xml", "package"], timeout=600)
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


# ---- 3. Maven deploy --------------------------------------------------------

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
        return f.cmd(name, argv, expect=expect, timeout=900, target=target,
                     env={"MAVEN_OPTS": "-Dorg.slf4j.simpleLogger.showDateTime=true "
                                        "-Dorg.slf4j.simpleLogger.dateTimeFormat=HH:mm:ss.SSS"})

    def props(app, **extra):
        values = {"brewlet.kubeconfig": kc, "brewlet.kubeContext": "wf-maven-target",
                  "brewlet.namespace": MAVEN_NS, "brewlet.appName": app, "brewlet.jdkFeature": "21",
                  "brewlet.readinessPath": "/healthz", "brewlet.resources.cpuRequest": "100m",
                  "brewlet.resources.memoryRequest": "128Mi", "brewlet.resources.cpuLimit": "500m",
                  "brewlet.resources.memoryLimit": "256Mi", "brewlet.image": f"{f.registry}/wf/maven:{app}"}
        values.update(extra)
        return values

    project = build_app(f, "maven-app")
    deployed = mvn("maven-deploy", project, ["package", f"{f.plugin}:deploy"], props("wf-maven"))
    require(f"wf-maven is Ready in namespace {MAVEN_NS}" in deployed.stdout, "deploy did not report readiness")
    push_file = project / "target/brewlet/push.json"
    handoff = json.loads(push_file.read_text())
    published = h.verify_published(f.registry, "wf/maven", "wf-maven")
    h.validate_handoff(handoff, f.registry, "wf/maven", "wf-maven", published["digest"])
    manifest = (project / "target/brewlet/javaapplication.yaml").read_text()
    applied = f.get("javaapplication", "wf-maven", "-n", MAVEN_NS)
    require(handoff["deployImage"] in manifest and applied["spec"]["artifact"]["image"] == handoff["deployImage"],
            "manifest/applied image differs from push.json")
    ready = h.condition(applied, "Ready") or {}
    require(ready.get("status") == "True" and ready.get("observedGeneration") == applied["metadata"]["generation"],
            "applied JavaApplication is not Ready for its current generation")
    require("Hello from a JAR" in f.hello(MAVEN_NS, "wf-maven"), "deployed workload did not respond")
    require(f.kube("get", "javaapplication", "wf-maven", "-n", "default", check=False).returncode != 0,
            "deploy also targeted the default namespace")
    uid = json.loads(run(["kubectl", "--kubeconfig", kc, "--context", "wf-maven-target", "get", "namespace",
                          "kube-system", "-o", "json"], env=f.env).stdout)["metadata"]["uid"]
    require(uid == f.cluster_uid, "Maven kubeconfig context is not the disposable cluster")
    f.record("maven-deploy-push-manifest-apply-ready-response", {"handoff": handoff, "index": published["digest"],
             "appliedGeneration": applied["metadata"]["generation"], "clusterUID": uid, **target})

    nowait = build_app(f, "maven-nowait")
    result = mvn("maven-deploy-no-wait", nowait, ["package", f"{f.plugin}:deploy"],
                 props("wf-nowait", **{"brewlet.wait": "false", "brewlet.readinessPath": "/not-ready",
                                       "brewlet.waitTimeout": "60"}))
    require("not waiting for readiness (brewlet.wait=false)" in result.stdout and
            f.get("javaapplication", "wf-nowait", "-n", MAVEN_NS)["metadata"]["name"] == "wf-nowait",
            "wait opt-out did not apply without readiness")
    f.record("maven-deploy-wait-opt-out", {"app": "wf-nowait", "readiness": "never (/not-ready)",
                                           "elapsedSeconds": round(result.elapsed, 1)})

    slow = build_app(f, "maven-timeout")
    result = mvn("maven-deploy-readiness-timeout", slow, ["package", f"{f.plugin}:deploy"],
                 props("wf-mvn-timeout", **{"brewlet.readinessPath": "/not-ready", "brewlet.waitTimeout": "30"}),
                 expect="fail")
    output = result.stdout + result.stderr
    require(f"JavaApplication {MAVEN_NS}/wf-mvn-timeout was not Ready after 30s" in output and
            f"kubectl describe javaapplication wf-mvn-timeout -n {MAVEN_NS}" in output,
            "readiness timeout diagnostics missing")
    began = h.log_seconds(output, "waiting up to 30s for JavaApplication")
    ended = h.log_seconds(output, "was not Ready after 30s")
    f.record("maven-deploy-live-readiness-timeout", h.assert_bounded(
        "Maven readiness wait", (ended - began) % 86400, 30, MAVEN_TOLERANCE))

    stalled = build_app(f, "maven-stalled")
    state = f.private / "stalled-kubectl"
    state.mkdir()
    stub = state / "kubectl"
    stub.write_text(h.stalled_kubectl(state))
    stub.chmod(0o755)
    result = mvn("maven-deploy-stalled-kubectl", stalled, ["package", f"{f.plugin}:deploy"],
                 props("wf-mvn-stalled", **{"brewlet.kubectl": stub, "brewlet.waitTimeout": "8"}), expect="fail")
    finished = time.time()
    require("kubectl apply timed out after 8s" in result.stdout + result.stderr, "stalled kubectl diagnostics missing")
    bound = h.assert_bounded("stalled kubectl apply", finished - float((state / "started").read_text()), 8, MAVEN_TOLERANCE)
    orphans = [pid for pid, name in (((state / "kubectl.pid").read_text().strip(), "kubectl"),
                                     ((state / "child.pid").read_text().strip(), "sleep"))
               if h.process_alive(pid, name)]
    require(not orphans, f"stalled kubectl left orphan processes: {orphans}")
    require(f.kube("get", "javaapplication", "wf-mvn-stalled", "-n", MAVEN_NS, check=False).returncode != 0,
            "stalled kubectl stub mutated the cluster")
    f.record("maven-deploy-injected-stalled-kubectl", {"label": "injected process failure, not a live rollout",
             **bound, "orphans": []})

    encrypted_push(f, mvn, props)

    before = sha256(push_file)
    live_version = f.get("javaapplication", "wf-maven", "-n", MAVEN_NS)["metadata"]["resourceVersion"]
    apps = sorted(a["metadata"]["name"] for a in f.get("javaapplication", "-n", MAVEN_NS)["items"])
    mvn("maven-deploy-dry-run", project, ["package", f"{f.plugin}:deploy"],
        props("wf-maven", **{"brewlet.dryRun": "true"}))
    mvn("maven-deploy-dry-run-new-tag", project, ["package", f"{f.plugin}:deploy"],
        props("wf-maven", **{"brewlet.dryRun": "true", "brewlet.image": f"{f.registry}/wf/maven-dryrun:v1"}))
    require(sha256(push_file) == before, "dry run rewrote push.json")
    require(h.manifest(f.registry, "wf/maven-dryrun", "v1") is None, "dry run published to the registry")
    require(f.get("javaapplication", "wf-maven", "-n", MAVEN_NS)["metadata"]["resourceVersion"] == live_version and
            sorted(a["metadata"]["name"] for a in f.get("javaapplication", "-n", MAVEN_NS)["items"]) == apps,
            "dry run mutated Kubernetes")
    (project / "target/brewlet/javaapplication.yaml").unlink()
    result = mvn("maven-manifest-after-dry-run", project, ["package", f"{f.plugin}:manifest"], props("wf-maven"))
    require(f"using image from the last brewlet:push: {handoff['deployImage']}" in result.stdout and
            handoff["deployImage"] in (project / "target/brewlet/javaapplication.yaml").read_text(),
            "manifest after dry run did not use the saved digest-pinned image")
    f.record("maven-deploy-dry-run-nonmutating", {"pushJsonSHA256": before, "resourceVersion": live_version,
             "manifestImage": handoff["deployImage"]})


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

    project = build_app(f, "maven-auth")
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
        failing = build_app(f, name)
        result = mvn(name, failing, ["package", f"{f.plugin}:push"], props("wf-auth", **{"brewlet.image": f"{image}:{tag}"}),
                     expect="fail", settings_file=settings_file, extra=[flag])
        require(message is None or message in result.stdout + result.stderr, f"{name} failure not explicit")
        require(not (failing / "target/brewlet/push.json").exists() and
                h.manifest(f.auth_registry, "wf/maven-auth", tag, f.auth) is None, f"{name} published content")
        failures[name] = "failed without secret disclosure"
    f.record("maven-encrypted-settings-credentials", {"handoff": handoff, "settingsSecurity": "private, generated",
                                                      **failures})


# ---- 4. profile deletion ----------------------------------------------------

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
    finally:
        f.resume_operator()

    events = f.work / "nodeprofile-live-watch.json"
    with open(events, "w") as out:
        watcher = subprocess.Popen(["kubectl", "--kubeconfig", str(f.kubeconfig), "--context", f.context, "get",
                                    "nodeprofile", "live", "--watch", "-o", "json", "--output-watch-events"],
                                   stdout=out, stderr=subprocess.DEVNULL, env=f.env, start_new_session=True)
    f.children.append(watcher)
    wait("watch started", lambda: events.stat().st_size > 0, timeout=60, interval=1)
    with LogCapture(f, "cleanup-live"):
        result = f.k8s("profile-delete-attach-wait", kc, "profile", "delete", "live", "--wait", "--wait-timeout", "360s",
                       timeout=420)
    require('is already deleting; following its cleanup' in result.stderr and 'NodeProfile "live" deleted after' in
            result.stderr, "attached wait did not follow cleanup to completion")
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
        maven_scenario(fixture)
        profile_scenario(fixture, image)
        assert_no_leaks(fixture)


if __name__ == "__main__":
    main()
