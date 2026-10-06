# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

from copy import deepcopy
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

from smoke import APP, deploy, exercise, main


class SmokeTests(unittest.TestCase):
    def fixture(self):
        fixture = Mock(namespace="private", node="owned-node")
        image = "registry/app@sha256:" + "a" * 64
        deployment = patch("smoke.deploy", return_value=image)
        deployment.start()
        self.addCleanup(deployment.stop)
        fixture.kube.return_value.returncode = 0
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
        (output / "javaapplication.yaml").write_text("image: " + image)
        fixture.get.return_value = {"spec": {"artifact": {"image": image}}}
        return fixture, image

    def test_real_deploy_goal_uses_private_target_and_generated_manifest(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fixture, image = self.deployment_fixture(root)
            with patch("smoke.maven_app", return_value=root):
                self.assertEqual(deploy(fixture), image)
            argv = fixture.run.call_args.args[0]
            for arg in (fixture.plugin + ":deploy", f"-Dbrewlet.kubeconfig={fixture.kubeconfig}",
                        "-Dbrewlet.kubeContext=kind-owned", "-Dbrewlet.namespace=private",
                        "-Dbrewlet.readinessPath=/healthz"):
                self.assertIn(arg, argv)
            fixture.get.assert_called_once_with("javaapplication", APP, "-n", "private")
            fixture.save.assert_called_once_with("maven-deploy.log", "Ready")

    def test_deployment_failure_or_identity_mismatch_cannot_pass(self):
        for failure in ("command", "handoff", "manifest", "applied"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                fixture, _ = self.deployment_fixture(root)
                if failure == "command":
                    fixture.run.return_value.returncode = 1
                elif failure == "handoff":
                    (root / "target/brewlet/push.json").write_text('{"deployImage":"mutable:tag"}')
                elif failure == "manifest":
                    (root / "target/brewlet/javaapplication.yaml").write_text("wrong image")
                else:
                    fixture.get.return_value["spec"]["artifact"]["image"] = "wrong image"
                with patch("smoke.maven_app", return_value=root), \
                        self.assertRaises((RuntimeError, AssertionError)):
                    deploy(fixture)


if __name__ == "__main__":
    unittest.main()
