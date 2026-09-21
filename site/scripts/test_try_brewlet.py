# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Execute the disposable demo with offline tool doubles; never contact Docker."""

import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import tarfile
import tempfile
import time
import unittest


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "site/try-brewlet.sh"
DIGEST = "sha256:" + "a" * 64


def tool_bytes(tool):
    return (f"#!{sys.executable}\nimport runpy, sys\n"
            f"sys.argv = [{__file__!r}, '--fake-tool', {tool!r}, *sys.argv[1:]]\n"
            f"runpy.run_path({str(Path(__file__).resolve())!r}, run_name='__main__')\n").encode()


def archive_bytes(name, content):
    output = io.BytesIO()
    with tarfile.open(fileobj=output, mode="w") as archive:
        entry = tarfile.TarInfo(name)
        entry.size, entry.mode = len(content), 0o755
        archive.addfile(entry, io.BytesIO(content))
    return gzip.compress(output.getvalue(), mtime=0)


def fake_tool(tool, args):
    state = Path(os.environ["MOCK_STATE"])
    failure = os.environ.get("MOCK_FAILURE", "")
    with (state / "calls.jsonl").open("a") as log:
        log.write(json.dumps({"tool": tool, "args": args,
                              "host": os.environ.get("DOCKER_HOST"),
                              "platform": os.environ.get("DOCKER_DEFAULT_PLATFORM"),
                              "kubeconfig": os.environ.get("KUBECONFIG"),
                              "provider": os.environ.get("KIND_EXPERIMENTAL_PROVIDER"),
                              "api_override": os.environ.get("HELM_KUBEAPISERVER"),
                              "driver": os.environ.get("HELM_DRIVER"),
                              "master": os.environ.get("KUBERNETES_MASTER")}) + "\n")

    def value(flag):
        return args[args.index(flag) + 1]

    def fail(phase):
        if failure == phase:
            print("Injected failure: " + phase, file=sys.stderr)
            raise SystemExit(1)

    if tool == "uname":
        print(os.environ.get("MOCK_OS", "Linux") if args == ["-s"]
              else os.environ.get("MOCK_ARCH", "x86_64"))
    elif tool == "curl":
        url = next(arg for arg in args if arg.startswith("http"))
        if "/actuator/health" in url:
            print('{"status":"UP"}')
            return
        fail("download")
        output = Path(value("-o"))
        if "install.sh" in url:
            data = (f'#!/bin/sh\n[ "$1 $2 $3" = "--version latest --install-dir" ] || exit 99\n'
                    f'cp "$MOCK_BIN/brewlet" "$4/brewlet"\n').encode()
        elif "spring-petclinic/archive" in url:
            data = archive_bytes("spring-petclinic-revision/pom.xml", b"<project/>")
        elif "sha256sum.txt" in url:
            digest = hashlib.sha256(tool_bytes("jq")).hexdigest()
            data = "".join(f"{digest}  jq-{platform}-{arch}\n"
                           for platform in ("linux", "macos")
                           for arch in ("amd64", "arm64")).encode()
        else:
            if "get.helm.sh" in url:
                platform = re.search(r"helm-v[\d.]+-(.*?).tar.gz", url).group(1)
                data = archive_bytes(platform + "/helm", tool_bytes("helm"))
            elif "kind.sigs" in url:
                data = tool_bytes("kind")
            elif "dl.k8s.io" in url:
                data = tool_bytes("kubectl")
            elif "jqlang" in url:
                data = tool_bytes("jq")
            else:
                raise AssertionError(url)
            if url.endswith((".sha256", ".sha256sum")):
                data = (("0" * 64 if failure == "checksum" else hashlib.sha256(data).hexdigest()) + "\n").encode()
        output.write_bytes(data)
    elif tool == "docker":
        if args[:2] == ["context", "inspect"]:
            print("tcp://remote:2375" if failure == "remote" else "unix:///local/docker.sock")
        elif args[0] == "info":
            fail("docker")
            if "--format" in args:
                print((2 if failure == "resources" else 4) if "NCPU" in value("--format")
                      else 8 * 1024 ** 3)
        elif args[:2] == ["buildx", "version"]:
            print("buildx")
        elif args[:2] == ["buildx", "imagetools"]:
            print(json.dumps({"manifests": [{"platform": {"os": "linux", "architecture": "arm64"},
                                            "digest": DIGEST}]}) if "--raw" in args else DIGEST)
        elif args[0] == "version":
            print("arm64")
        elif args[0] == "run":
            Path(value("--cidfile")).write_text("owned-builder")
            if failure == "building":
                signal.signal(signal.SIGINT, signal.SIG_IGN)
            (state / "builder").write_text(value("--label").split("=", 1)[1])
            if failure == "building":
                while True:
                    time.sleep(1)
            fail("build")
            target = Path(value("--volume").removesuffix(":/work")) / "target"
            target.mkdir()
            (target / "spring-petclinic.jar").write_bytes(b"jar")
            (state / "builder").unlink()
        elif args[:2] == ["container", "inspect"]:
            raise SystemExit(0 if (state / "builder").exists() else 1)
        elif args[0] == "inspect":
            print((state / "builder").read_text())
        elif args[:2] == ["rm", "-f"]:
            assert args[2:] == ["owned-builder"]
            (state / "builder").unlink()
        elif args[0] == "exec":
            assert (state / "cluster").read_text() + "-worker" in args
            if "crictl" in args:
                fail("pull")
            if "import" in args:
                sys.stdin.buffer.read()
        else:
            raise AssertionError(args)
    elif tool == "kind":
        if args == ["get", "clusters"]:
            print("unrelated-cluster")
            if failure == "collision":
                print(Path(os.environ["KUBECONFIG"]).parent.name.lower().replace(".", "-"))
        elif args[:2] == ["create", "cluster"]:
            (state / "cluster").write_text(value("--name"))
            Path(value("--kubeconfig")).write_text("private kubeconfig")
            assert "--retain" in args
            assert "--image" in args and DIGEST in value("--image")
            fail("create")
        elif args[:2] == ["delete", "cluster"]:
            assert value("--name") == (state / "cluster").read_text()
            fail("cleanup")
            (state / "cluster").unlink()
        elif args[:2] == ["export", "logs"]:
            fail("diagnostics")
            Path(args[2]).mkdir()
            (Path(args[2]) / "evidence.log").write_text("diagnostics")
        else:
            raise AssertionError(args)
    elif tool in ("kubectl", "helm"):
        cluster = (state / "cluster").read_text()
        assert value("--kubeconfig") != str(state / "home/.kube/config")
        assert value("--context" if tool == "kubectl" else "--kube-context") == "kind-" + cluster
        if "template" in args:
            for component in ("admission", "node-provisioner", "operator"):
                print(f"image: ghcr.io/microsoft/brewlet-{component}@{DIGEST}")
        elif "install" in args:
            fail("install")
        elif "javaapplication/petclinic" in args:
            fail("readiness")
        elif "port-forward" in args:
            fail("forward")
            assert value("--address") == "127.0.0.1"
            assert ":8080" in args
            print("Forwarding from 127.0.0.1:38111 -> 8080", flush=True)
            while True:
                time.sleep(1)
    elif tool == "brewlet":
        if args == ["version"]:
            print("9.8.7")
        elif args[0] == "push":
            assert value("--format") == "image"
            store = Path(value("--store"))
            (store / "blobs").mkdir(parents=True)
            (store / "oci-layout").write_text('{"imageLayoutVersion":"1.0.0"}')
            (store / "index.json").write_text(json.dumps({"manifests": [{
                "annotations": {"org.opencontainers.image.ref.name": args[2]}, "digest": DIGEST}]}))
        else:
            raise AssertionError(args)
    elif tool == "jq":
        if "--arg" in args:
            key, expected = args[args.index("--arg") + 1:args.index("--arg") + 3]
            data = json.loads(sys.stdin.read() if key == "arch" else Path(args[-1]).read_text())
            for manifest in data["manifests"]:
                if (key == "arch" and manifest["platform"]["architecture"] == expected or
                        key == "ref" and manifest["annotations"]["org.opencontainers.image.ref.name"] == expected):
                    print(manifest["digest"])
        else:
            assert '.status == "UP"' in args
            assert json.loads(Path(args[-1]).read_text())["status"] == "UP"
            print("true")
    else:
        raise AssertionError(tool)


class DisposableDemoTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="brewlet demo tests ")
        self.addCleanup(self.temp.cleanup)
        self.state = Path(self.temp.name)
        self.bin = self.state / "bin"
        self.bin.mkdir()
        for tool in ("curl", "docker", "uname", "brewlet"):
            path = self.bin / tool
            path.write_bytes(tool_bytes(tool))
            path.chmod(0o755)
        self.home = self.state / "home"
        (self.home / ".kube").mkdir(parents=True)
        (self.home / ".kube/config").write_text("do not change my contexts")
        (self.state / "tmp").mkdir()
        self.env = {
            **os.environ, "PATH": str(self.bin) + os.pathsep + os.environ["PATH"],
            "HOME": str(self.home), "TMPDIR": str(self.state / "tmp"),
            "MOCK_STATE": str(self.state), "MOCK_BIN": str(self.bin),
            "KUBECONFIG": str(self.home / ".kube/config"), "DOCKER_CONTEXT": "desktop-linux",
            "DOCKER_DEFAULT_PLATFORM": "linux/amd64", "PETCLINIC_REF": "incorrect-image-name",
            "HELM_KUBEAPISERVER": "https://production.invalid", "HELM_DRIVER": "sql",
            "KUBERNETES_MASTER": "https://production.invalid", "KIND_EXPERIMENTAL_PROVIDER": "podman",
        }

    def calls(self, tool=None):
        path = self.state / "calls.jsonl"
        calls = [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []
        return [call for call in calls if tool is None or call["tool"] == tool]

    def run_demo(self, failure="", stop_signal=None, **env):
        with (self.state / "output.log").open("w") as output:
            process = subprocess.Popen(
                ["bash", str(SCRIPT)], env={**self.env, "MOCK_FAILURE": failure, **env},
                stdout=output, stderr=subprocess.STDOUT, start_new_session=True,
            )
            try:
                if stop_signal is not None:
                    deadline = time.monotonic() + 30
                    while process.poll() is None and time.monotonic() < deadline:
                        if (failure == "building" and (self.state / "builder").exists() or
                                "PetClinic is ready:" in (self.state / "output.log").read_text()):
                            os.killpg(process.pid, stop_signal)
                            break
                        time.sleep(0.05)
                code = process.wait(timeout=30)
            except BaseException:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait()
                raise
        text = (self.state / "output.log").read_text()
        self.assertEqual((self.home / ".kube/config").read_text(), "do not change my contexts")
        return code, text

    def test_success_and_ctrl_c_preserve_existing_configuration(self):
        code, text = self.run_demo(stop_signal=signal.SIGINT)
        self.assertEqual(code, 130, text)
        self.assertIn("PetClinic is ready: http://127.0.0.1:38111", text)
        self.assertIn("Removed only the disposable cluster", text)
        self.assertFalse((self.state / "cluster").exists())
        self.assertFalse((self.state / "builder").exists())
        for call in self.calls():
            if call["tool"] in ("kind", "kubectl", "helm"):
                self.assertEqual(call["provider"], "docker")
                self.assertIsNone(call["api_override"])
                self.assertIsNone(call["driver"])
                self.assertIsNone(call["master"])
        for call in self.calls("docker"):
            if call["args"][:2] != ["context", "inspect"]:
                self.assertEqual(call["host"], "unix:///local/docker.sock")
                self.assertIsNone(call["platform"])
        workspace = next((self.state / "tmp").iterdir())
        self.assertFalse((workspace / "source").exists())
        self.assertTrue((workspace / "cluster-logs/evidence.log").is_file())
        self.assertTrue((workspace / "bin/kind").is_file())
        self.assertIn("delete cluster --name ", (workspace / "cleanup-command.txt").read_text())

    def test_failures_cleanup_only_resources_created_by_the_run(self):
        for failure in ("download", "checksum", "build", "create", "pull", "install", "readiness", "forward", "collision"):
            with self.subTest(failure=failure):
                self.setUp()
                code, text = self.run_demo(failure)
                self.assertNotEqual(code, 0, text)
                self.assertFalse((self.state / "cluster").exists(), text)
                self.assertFalse((self.state / "builder").exists(), text)
                deletes = [call for call in self.calls("kind") if call["args"][:2] == ["delete", "cluster"]]
                self.assertEqual(len(deletes), int(failure in ("create", "pull", "install", "readiness", "forward")))
                if failure in ("checksum", "build", "create", "pull", "collision"):
                    self.assertFalse(any("install" in call["args"] for call in self.calls("helm")))

    def test_cleanup_failure_retains_tools_and_prints_exact_recovery_command(self):
        code, text = self.run_demo("cleanup", stop_signal=signal.SIGTERM)
        self.assertEqual(code, 1, text)
        cluster = (self.state / "cluster").read_text()
        self.assertIn("Cluster cleanup failed", text)
        self.assertIn("--name " + cluster, text)
        self.assertIn("DOCKER_HOST=unix:///local/docker.sock", text)
        self.assertTrue((next((self.state / "tmp").iterdir()) / "bin/kind").is_file())

    def test_ctrl_c_during_build_removes_only_the_owned_build_container(self):
        code, text = self.run_demo("building", stop_signal=signal.SIGINT)
        self.assertEqual(code, 130, text)
        self.assertFalse((self.state / "builder").exists())
        self.assertFalse(any(call["args"][:2] == ["create", "cluster"] for call in self.calls("kind")))
        removed = [call["args"] for call in self.calls("docker") if call["args"][:2] == ["rm", "-f"]]
        self.assertEqual(removed, [["rm", "-f", "owned-builder"]])

    def test_failed_diagnostics_do_not_prevent_cluster_cleanup(self):
        code, text = self.run_demo("diagnostics", stop_signal=signal.SIGTERM)
        self.assertEqual(code, 143, text)
        self.assertIn("Some cluster diagnostics could not be collected", text)
        self.assertFalse((self.state / "cluster").exists())

    def test_remote_or_insufficient_engine_is_rejected_before_downloads(self):
        for failure in ("remote", "resources", "docker"):
            with self.subTest(failure=failure):
                self.setUp()
                code, text = self.run_demo(failure)
                self.assertNotEqual(code, 0, text)
                self.assertEqual(self.calls("curl"), [])
                self.assertEqual(list((self.state / "tmp").iterdir()), [])

    def test_macos_and_linux_tool_platforms(self):
        for os_name, arch, platform in (("Darwin", "arm64", "darwin-arm64"),
                                       ("Linux", "x86_64", "linux-amd64")):
            with self.subTest(platform=platform):
                self.setUp()
                code, text = self.run_demo(stop_signal=signal.SIGTERM, MOCK_OS=os_name, MOCK_ARCH=arch)
                self.assertEqual(code, 143, text)
                urls = " ".join(arg for call in self.calls("curl") for arg in call["args"])
                self.assertIn("kind-" + platform, urls)
                self.assertIn("helm-v4.3.0-" + platform + ".tar.gz", urls)

    def test_failed_child_does_not_exit_or_reconfigure_the_callers_shell(self):
        for shell in ("bash", "sh"):
            with self.subTest(shell=shell):
                result = subprocess.run(
                    [shell, "-c", 'before="$-"; bash "$1"; test "$before" = "$-"; '
                     'printf "TERMINAL_STILL_OPEN:%s\\n" "$KUBECONFIG"', "test", str(SCRIPT)],
                    env={**self.env, "MOCK_FAILURE": "checksum"}, text=True, capture_output=True, timeout=30,
                )
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn("TERMINAL_STILL_OPEN:" + self.env["KUBECONFIG"], result.stdout)
        result = subprocess.run(
            ["bash", "-c", 'before="$-"; source "$1"; test "$before" = "$-"; echo SOURCED_SHELL_ALIVE',
             "test", str(SCRIPT)], env=self.env, text=True, capture_output=True, timeout=10,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("do not source", result.stderr)
        self.assertIn("SOURCED_SHELL_ALIVE", result.stdout)

    def test_pinned_petclinic_revision_matches_the_existing_fixture(self):
        script = SCRIPT.read_text()
        revision = re.search(r"^petclinic_revision=([0-9a-f]{40})$", script, re.MULTILINE).group(1)
        fixture = (ROOT / "integration-tests/fixtures/spring-petclinic/build.sh").read_text()
        self.assertIn("PETCLINIC_REF:-" + revision, fixture)


if __name__ == "__main__":
    if len(sys.argv) > 1 and sys.argv[1] == "--fake-tool":
        fake_tool(sys.argv[2], sys.argv[3:])
    else:
        unittest.main()
