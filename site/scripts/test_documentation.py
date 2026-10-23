"""Check published reader coverage and executable example structure offline."""

import importlib.util
import json
from pathlib import Path
import re
import subprocess
import unittest
from unittest.mock import patch
from datetime import datetime, timedelta, timezone

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("tutorial_check", Path(__file__).with_name("tutorial_check.py"))
TUTORIAL = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(TUTORIAL)


class DocumentationTests(unittest.TestCase):
    def test_language_guides_explain_the_model_before_the_recipe(self):
        markers = {"java": "JDK", "python": "Python interpreter", "nodejs": "Node.js",
                   "dotnet": "framework-dependent", "webassembly": ".wasm"}
        for name, marker in markers.items():
            text = (ROOT / "docs" / (name + ".md")).read_text()
            with self.subTest(guide=name):
                intro = text.split("## How it works\n", 1)[1].split("\n## ", 1)[0]
                self.assertLess(text.index("## How it works"), text.index("## Tested runtime inputs"))
                self.assertLessEqual(len(intro.split()), 200, "keep the landing overview brief")
                for required in (marker, "Deployment", "/app", "composition", "new task instances"):
                    self.assertIn(required, intro)
                for target in ("concepts.md", "runtime-images.md", "managed-composition.md",
                               "reference.md#launch-descriptor"):
                    self.assertIn("](" + target + ")", intro)

    def test_runtime_comparison_includes_native_image_volumes_and_qualification_limits(self):
        text = (ROOT / "docs/choosing-brewlet.md").read_text()
        for required in ("Conventional application image",
                         "Runtime container + application image volume",
                         "## Authorization, restarts and recovery",
                         "Kubernetes 1.36", "Neither design hot-patches",
                         "No equal-condition benchmark",
                         "not a native image-volume"):
            self.assertIn(required, text)
        self.assertIn("https://kubernetes.io/docs/concepts/storage/volumes/#image", text)

    def test_workload_guides_have_consistent_complete_structure_and_pretty_launch_json(self):
        headings = ("Tested runtime inputs", "Build the payload", "Launch descriptor",
                    "Package", "Native deployment", "Verify", "Pitfalls and qualification limits")
        for name in ("java", "python", "nodejs", "dotnet", "webassembly"):
            text = (ROOT / "docs" / (name + ".md")).read_text()
            with self.subTest(guide=name):
                positions = [text.index("## " + heading + "\n") for heading in headings]
                self.assertEqual(positions, sorted(positions))
                launches = [json.loads(block) for block in re.findall(r"```json\n(.*?)\n```", text, re.S)]
                self.assertTrue(any(value.get("schemaVersion") == 1 and "process" in value for value in launches))
                self.assertIn("image:", text)
                self.assertIn("brewlet.sh/runtime-image:", text)
        for name in ("java", "python", "nodejs", "dotnet", "webassembly", "language-recipes"):
            text = (ROOT / "docs" / (name + ".md")).read_text()
            for block in re.findall(r"```json\n(.*?)\n```", text, re.S):
                with self.subTest(guide=name, snippet=block[:50]):
                    parsed = json.loads(block)
                    self.assertEqual(block, json.dumps(parsed, indent=2, ensure_ascii=False))
                    self.assertNotIn("requires", parsed)

    def test_landing_workload_entries_are_all_links_with_the_same_markup(self):
        landing = (ROOT / "site/index.html").read_text()
        strip = re.search(r'<nav class="container language-strip".*?</nav>', landing, re.S).group()
        links = re.findall(r'<a href="([^"]+)">([^<]+)</a>', strip)
        self.assertEqual(links, [
            ("docs/java/", "Java"), ("docs/python/", "Python"), ("docs/nodejs/", "Node.js"),
            ("docs/dotnet/", ".NET"), ("docs/webassembly/", "WebAssembly")])
        self.assertNotIn("<strong>", strip)
        self.assertIn('aria-label="Tested workload guides"', strip)
        for anchor in ("Python", "Java", "Node.js", ".NET", "Static/native binaries"):
            self.assertIn("## " + anchor, (ROOT / "docs/language-recipes.md").read_text())

    def test_published_json_examples_are_valid_and_not_collapsed_to_one_line(self):
        import yaml
        config = yaml.safe_load((ROOT / "site/mkdocs.yml").read_text())
        def pages(entries):
            for entry in entries:
                for value in entry.values():
                    if isinstance(value, list):
                        yield from pages(value)
                    elif isinstance(value, str) and value.endswith(".md") and "://" not in value:
                        yield ROOT / "docs" / value
        for page in pages(config["nav"]):
            for block in re.findall(r"```json\n(.*?)\n```", page.read_text(), re.S):
                with self.subTest(page=page.name, snippet=block[:40]):
                    json.loads(block)
                    self.assertGreater(len(block.splitlines()), 1)

    def test_fixture_workload_deployments_preserve_explicit_native_contract(self):
        import yaml
        for name in ("java", "python", "nodejs", "dotnet"):
            text = (ROOT / "docs" / (name + ".md")).read_text()
            manifests = [yaml.safe_load(block) for block in re.findall(r"```yaml\n(.*?)\n```", text, re.S)]
            self.assertEqual(len(manifests), 1)
            deployment = manifests[0]
            template = deployment["spec"]["template"]
            self.assertEqual(deployment["kind"], "Deployment")
            self.assertIn("brewlet.sh/runtime-image", template["metadata"]["annotations"])
            self.assertEqual(template["spec"]["runtimeClassName"], "brewlet")
            self.assertEqual(template["spec"]["nodeSelector"]["kubernetes.io/arch"], "arm64")
            app = template["spec"]["containers"][0]
            self.assertTrue(app["securityContext"]["readOnlyRootFilesystem"])
            self.assertEqual(app["readinessProbe"]["httpGet"]["port"], 8000)

    def test_reader_categories_are_published_not_just_present_in_repository(self):
        config = (ROOT / "site/mkdocs.yml").read_text().split("nav:", 1)[1]
        for page in ("concepts", "choosing-brewlet", "local-kubernetes", "installation",
                     "configuration", "providers", "multi-arch", "language-recipes",
                     "dependency-layer-sets", "dependency-remediation", "resources", "observability", "instrumentation",
                     "security", "registry-trust", "troubleshooting", "uninstallation",
                     "reference", "compatibility-policy", "demo", "workshops/platform",
                     "workshops/developer"):
            with self.subTest(page=page):
                self.assertIn(page + ".md", config)
                self.assertTrue((ROOT / "docs" / (page + ".md")).is_file())

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
