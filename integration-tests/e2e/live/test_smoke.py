# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

from copy import deepcopy
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, call, patch
import xml.etree.ElementTree as ET

from smoke import APP, deploy, exercise, main
from workflows import maven_app


class SmokeTests(unittest.TestCase):
    def fixture(self):
        fixture = Mock(namespace="private", node="owned-node")
        image = "registry/app@sha256:" + "a" * 64
        deployment = patch("smoke.deploy", return_value=image)
        deployment.start()
        self.addCleanup(deployment.stop)
        fixture.kube.return_value.returncode = 0
        fixture.run.return_value = Mock(returncode=0, stdout="Ready", stderr="")
        fixture.get.return_value = {"items": [{
            "metadata": {"name": "smoke-pod"},
            "spec": {"runtimeClassName": "brewlet", "nodeName": "owned-node",
                     "containers": [{"image": image}]},
        }]}
        fixture.service_get.return_value = "Hello from a JAR"
        return fixture

    def test_serves_published_image_and_records_evidence(self):
        fixture = self.fixture()
        exercise(fixture)
        fixture.java_application.assert_not_called()
        fixture.wait_ready.assert_called_once_with(APP)
        fixture.run.assert_called_once_with(
            [fixture.cli, "k8s", "--kubeconfig", fixture.kubeconfig,
             "--context", fixture.context, "app", "wait", APP,
             "--namespace", fixture.namespace, "--wait-timeout", "5m"],
            check=False, timeout=330)
        fixture.record.assert_called_once()

    def test_wrong_runtime_node_image_or_response_fails(self):
        baseline = self.fixture().get.return_value
        for field, value in (("runtimeClassName", "runc"), ("nodeName", "foreign"),
                             ("containers", [{"image": "different"}])):
            with self.subTest(field=field):
                fixture = self.fixture()
                fixture.get.return_value = deepcopy(baseline)
                fixture.get.return_value["items"][0]["spec"][field] = value
                with self.assertRaises(AssertionError):
                    exercise(fixture)
                fixture.record.assert_not_called()
                fixture = self.fixture()
                fixture.run.return_value.returncode = 1
                with self.assertRaisesRegex(RuntimeError, "CLI readiness failed"):
                    exercise(fixture)
                fixture.wait_ready.assert_not_called()
                fixture.record.assert_not_called()
        fixture = self.fixture()
        fixture.service_get.return_value = "wrong server"
        with self.assertRaises(AssertionError):
            exercise(fixture)

    def test_missing_pod_and_readiness_failure_fail(self):
        fixture = self.fixture()
        fixture.get.return_value = {"items": []}
        with self.assertRaises(AssertionError):
            exercise(fixture)
        fixture = self.fixture()
        fixture.wait_ready.side_effect = RuntimeError("runtime failed")
        with self.assertRaisesRegex(RuntimeError, "runtime failed"):
            exercise(fixture)
        fixture.record.assert_not_called()

    def test_main_always_uses_owned_fixture_context(self):
        with patch("smoke.CheckoutFixture") as fixture, patch("smoke.exercise") as run:
            main()
            fixture.assert_called_once_with("smoke")
            run.assert_called_once_with(fixture.return_value.__enter__.return_value)
            fixture.return_value.__exit__.assert_called_once()

    def deployment_fixture(self, root):
        fixture = Mock(namespace="private", registry="localhost:1234",
                       kubeconfig=root / "private-kubeconfig", context="kind-owned",
                       plugin="sh.brewlet:brewlet-maven-plugin:0.1.0-SNAPSHOT",
                       maven_args=["mvn", "--settings", str(root / "settings.xml")])
        fixture.run.return_value = Mock(returncode=0, stdout="Ready", stderr="")
        output = root / "target/brewlet"
        output.mkdir(parents=True)
        image = fixture.registry + "/apps/smoke@sha256:" + "a" * 64
        (output / "push.json").write_text(json.dumps({"deployImage": image}))
        rendered = {
            "apiVersion": "apps.brewlet.sh/v1alpha1", "kind": "JavaApplication",
            "metadata": {"name": APP, "namespace": fixture.namespace},
            "spec": {"artifact": {"image": image}, "jvm": {"version": 21, "distribution": "temurin"},
                     "ports": [{"name": "http", "containerPort": 8080}],
                     "probes": {"readiness": {"httpGet": {"path": "/healthz", "port": 8080}}}},
        }
        (output / "javaapplication.yaml").write_text(json.dumps(rendered))
        fixture.kube.return_value = Mock(returncode=0, stdout=json.dumps(rendered), stderr="")
        fixture.get.return_value = {"spec": {"artifact": {"image": image}}}
        return fixture, image

    def test_push_and_manifest_hand_off_to_separate_deployment(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fixture, image = self.deployment_fixture(root)
            with patch("smoke.maven_app", return_value=root) as app:
                self.assertEqual(deploy(fixture), image)
            configuration = ET.fromstring("<configuration>" + app.call_args.kwargs["configuration"]
                                          + "</configuration>")
            self.assertEqual(configuration.findtext("ports/port/containerPort"), "8080")
            self.assertEqual(configuration.findtext("probes/readiness/path"), "/healthz")
            argv = fixture.run.call_args.args[0]
            for arg in (fixture.plugin + ":push", fixture.plugin + ":manifest",
                        "-Dbrewlet.image=localhost:1234/apps/smoke:pr",
                        "-Dbrewlet.appName=" + APP, "-Dbrewlet.namespace=private",
                        "-Dbrewlet.jdkFeature=21", "-Dbrewlet.jdkDistribution=temurin",
                        "-Dbrewlet.resources.cpuRequest=100m", "-Dbrewlet.resources.cpuLimit=500m",
                        "-Dbrewlet.resources.memoryRequest=128Mi", "-Dbrewlet.resources.memoryLimit=256Mi"):
                self.assertIn(arg, argv)
            self.assertLess(argv.index(fixture.plugin + ":push"), argv.index(fixture.plugin + ":manifest"))
            self.assertFalse(any(arg.startswith("-Dbrewlet.kube") for arg in argv if isinstance(arg, str)))
            fixture.java_application.assert_not_called()
            manifest = root / "target/brewlet/javaapplication.yaml"
            self.assertEqual(fixture.kube.call_args_list, [
                call("apply", "--dry-run=client", "-f", manifest, "-o", "json"),
                call("apply", "-f", manifest, "-n", "private"),
            ])
            fixture.get.assert_called_once_with("javaapplication", APP, "-n", "private")
            fixture.save.assert_has_calls([call("maven-push-manifest.log", "Ready"),
                                           call("javaapplication.yaml", manifest.read_text())])

    def test_deployment_failure_or_identity_mismatch_cannot_pass(self):
        for failure in ("command", "handoff", "missing", "apiVersion", "kind", "name", "namespace",
                        "image", "jdk", "distribution", "ports", "readiness", "parse", "apply", "applied"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                fixture, _ = self.deployment_fixture(root)
                if failure == "command":
                    fixture.run.return_value.returncode = 1
                elif failure == "handoff":
                    (root / "target/brewlet/push.json").write_text('{"deployImage":"mutable:tag"}')
                elif failure == "missing":
                    (root / "target/brewlet/javaapplication.yaml").unlink()
                elif failure in ("parse", "apply"):
                    fixture.kube.side_effect = ([fixture.kube.return_value, RuntimeError("apply failed")]
                                                if failure == "apply" else RuntimeError("parse failed"))
                elif failure == "applied":
                    fixture.get.return_value["spec"]["artifact"]["image"] = "wrong image"
                else:
                    rendered = json.loads(fixture.kube.return_value.stdout)
                    if failure in ("apiVersion", "kind"):
                        rendered[failure] = "wrong"
                    elif failure in ("name", "namespace"):
                        rendered["metadata"][failure] = "foreign"
                    elif failure == "image":
                        rendered["spec"]["artifact"]["image"] = "mutable:tag"
                    elif failure in ("jdk", "distribution"):
                        rendered["spec"]["jvm"]["version" if failure == "jdk" else "distribution"] = "wrong"
                    elif failure == "ports":
                        rendered["spec"]["ports"] = []
                    elif failure == "readiness":
                        rendered["spec"]["probes"]["readiness"]["httpGet"]["path"] = "/"
                    fixture.kube.return_value.stdout = json.dumps(rendered)
                with patch("smoke.maven_app", return_value=root), \
                        self.assertRaises((RuntimeError, AssertionError)):
                    deploy(fixture)
                fixture.java_application.assert_not_called()
                if failure not in ("apply", "applied"):
                    self.assertFalse(any("--dry-run=client" not in c.args for c in fixture.kube.call_args_list))
                if failure != "applied":
                    fixture.get.assert_not_called()

    def test_fixture_pom_declares_selected_plugin_and_optional_configuration(self):
        for configuration in ("", "<ports><port><containerPort>8080</containerPort></port></ports>"):
            with self.subTest(configuration=configuration), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                pom = root / "pom.xml"
                pom.write_text("<project><build><plugins>\n    </plugins></build></project>")
                fixture = Mock(plugin_version="0.1.0-SNAPSHOT")
                with patch("workflows.build_app", return_value=root):
                    self.assertEqual(maven_app(fixture, "app", configuration), root)
                plugin = ET.parse(pom).find("build/plugins/plugin")
                self.assertEqual(plugin.findtext("groupId"), "sh.brewlet")
                self.assertEqual(plugin.findtext("artifactId"), "brewlet-maven-plugin")
                self.assertEqual(plugin.findtext("version"), fixture.plugin_version)
                self.assertEqual(plugin.findtext("configuration/ports/port/containerPort"),
                                 "8080" if configuration else None)


if __name__ == "__main__":
    unittest.main()
