"""Check published reader coverage and executable example structure offline."""

import importlib.util
import json
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch
from datetime import datetime, timedelta, timezone

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("tutorial_check", Path(__file__).with_name("tutorial_check.py"))
TUTORIAL = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(TUTORIAL)


class DocumentationTests(unittest.TestCase):
    def test_reader_categories_are_published_not_just_present_in_repository(self):
        config = (ROOT / "site/mkdocs.yml").read_text().split("nav:", 1)[1]
        for page in ("concepts", "choosing-brewlet", "local-kubernetes", "installation",
                     "configuration", "providers", "multi-arch", "language-recipes",
                     "dependency-layer-sets", "resources", "observability", "instrumentation",
                     "security", "registry-trust", "troubleshooting", "uninstallation",
                     "reference", "compatibility-policy", "demo", "workshops/platform",
                     "workshops/developer"):
            with self.subTest(page=page):
                self.assertIn(page + ".md", config)
                self.assertTrue((ROOT / "docs" / (page + ".md")).is_file())
        self.assertNotIn("live-validation.md", config)
        self.assertNotIn(": compatibility.md", config)

    def test_exact_reader_tutorial_has_bounded_isolated_execution_and_normal_cleanup(self):
        script = "\n".join(TUTORIAL.steps())
        subprocess.run(["bash", "-n"], input=script, text=True, check=True)
        for required in ('--kubeconfig "$KUBECONFIG"', '--context "$CONTEXT"',
                         "io.x-k8s.kind.cluster", "NODE_UID", "RUNTIME_UID",
                         "imagePullPolicy: Never", "runtimeimage/$RID", "nodeprofile/$PROFILE",
                         "binding-retained.json", "binding-after.json", "--timeout=10m"):
            self.assertIn(required, script)
        self.assertLess(script.index('k wait --for=delete "nodeprofile/$PROFILE"'),
                        script.index('kind delete cluster'))
        for forbidden in ("--no-hooks", "skip_verify", "curl -k", "rm -rf", "JavaApplication"):
            self.assertNotIn(forbidden, script)

    def test_tutorial_is_sdk_independent_and_uses_explicit_distroless_invocation(self):
        launch = json.loads((ROOT / "examples/local/launch.json").read_text())
        self.assertEqual(launch["process"]["entrypoint"], ["/usr/bin/python3.13"])
        self.assertNotIn("PATH", [entry["name"] for entry in launch["process"]["env"]])
        script = "\n".join(TUTORIAL.steps())
        self.assertNotIn("mvn ", script)
        self.assertNotIn("dotnet ", script)
        self.assertNotIn("javac ", script)
        self.assertIn("git archive HEAD", script)
        self.assertIn("--target artifact", script)
        self.assertIn('export DOCKER_DEFAULT_PLATFORM="linux/$ARCH"', script)
        self.assertIn('docker image inspect --format \'{{.Architecture}}\' "$NODE_IMAGE_ID"', script)
        producer = (ROOT / "examples/local/producer.Dockerfile").read_text()
        self.assertIn("chmod 0755 /example /example/app", producer)
        self.assertIn("--mount=type=tmpfs,target=/package", producer)
        self.assertIn("--layer app=/package:/app", producer)

    def test_helm_documentation_tracks_every_current_leaf_value(self):
        import yaml
        values = yaml.safe_load((ROOT / "kubernetes/charts/brewlet/values.yaml").read_text())
        guide = (ROOT / "docs/configuration.md").read_text()
        def visit(value, prefix=""):
            for key, child in value.items():
                name = prefix + key
                if isinstance(child, dict):
                    visit(child, name + ".")
                else:
                    self.assertIn("`" + name + "`", guide)
        visit(values)


class TutorialObservationTests(unittest.TestCase):
    def setUp(self):
        spec = importlib.util.spec_from_file_location("reader_observe", ROOT / "examples/local/observe.py")
        self.module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.module)
        self.environment = patch.dict("os.environ", {
            "NODE": "private-node", "NODE_UID": "node-uid", "RID": "python-test",
            "RUNTIME_UID": "inventory-uid", "POD": "actual-pod", "APP_NAMESPACE": "private-app"})
        self.environment.start()
        self.addCleanup(self.environment.stop)
        self.binding = {"podUID": "pod-uid", "id": "python-test", "inventoryUID": "inventory-uid",
                        "digest": "sha256:" + "a" * 64, "state": "running", "generation": 2}
        self.observation = {"complete": True, "nodeUID": "node-uid",
                            "observedAt": datetime.now(timezone.utc).isoformat(),
                            "bindings": [self.binding]}
        self.image = {"metadata": {"uid": "inventory-uid", "generation": 3},
                      "status": {"digest": self.binding["digest"], "generation": 2,
                                 "rotation": {"participants": [{"uid": "node-uid"}]},
                                 "conditions": [{"type": "RotationComplete", "status": "True",
                                                 "observedGeneration": 3}]}}

    def observed(self, mode="binding"):
        node = {"metadata": {"uid": "node-uid", "annotations": {
            "brewlet.sh/runtime-bindings": json.dumps(self.observation)}}}
        with patch.object(self.module, "get", side_effect=[
                node, self.image, *([{"metadata": {"uid": "pod-uid"}}] if mode == "binding" else [])]):
            return self.module.observe(mode, "example.invalid/runtime@" + self.binding["digest"])

    def test_actual_identity_and_fresh_complete_binding_required(self):
        self.assertEqual(self.observed(), self.binding)
        self.observation["complete"] = False
        self.assertIsNone(self.observed())

    def test_stale_observation_is_not_a_positive_binding(self):
        self.observation["observedAt"] = (datetime.now(timezone.utc) - timedelta(minutes=5)).isoformat()
        self.assertIsNone(self.observed())

    def test_inventory_replacement_fails_instead_of_adopting_new_epoch(self):
        self.image["metadata"]["uid"] = "replacement"
        with self.assertRaisesRegex(RuntimeError, "inventory UID changed"):
            self.observed()

    def test_rotation_requires_current_condition_generation_and_actual_node_participation(self):
        self.assertEqual(self.observed("rotation")["generation"], 2)
        self.image["status"]["conditions"][0]["observedGeneration"] = 2
        self.assertIsNone(self.observed("rotation"))
        self.image["status"]["conditions"][0]["observedGeneration"] = 3
        self.image["status"]["rotation"]["participants"] = []
        self.assertIsNone(self.observed("rotation"))

    def test_webhook_retry_is_only_for_this_service_transport_not_denials_or_tls(self):
        error = 'failed calling webhook "runtimeimages.brewlet.sh": https://brewlet-admission.brewlet.svc: connect: connection refused'
        self.assertTrue(self.module.webhook_pending(error))
        self.assertFalse(self.module.webhook_pending(error.replace("brewlet-admission.brewlet.svc", "foreign.svc")))
        self.assertFalse(self.module.webhook_pending(error + " x509 certificate error"))
        self.assertFalse(self.module.webhook_pending('runtimeimages.brewlet.sh denied the request'))


if __name__ == "__main__":
    unittest.main()
