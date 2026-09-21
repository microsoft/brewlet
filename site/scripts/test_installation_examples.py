# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Render the documented Helm commands offline; never install or contact a cluster."""

import json
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import unittest
import uuid


ROOT = Path(__file__).resolve().parents[2]
CHART = ROOT / "kubernetes/charts/brewlet"
DOCUMENTS = (
    "README.md",
    "docs/installation.md",
    "docs/workshops/operations.md",
    "kubernetes/charts/brewlet/README.md",
)
# Synthetic render-only data, not an approved or downloadable JDK catalog.
DIGEST = "0123456789abcdef" * 4


def blocks(document, language):
    text = re.sub(r"^> ?", "", document, flags=re.MULTILINE)
    pattern = rf"^([ \t]*)```{re.escape(language)}\n(.*?)^\1```[ \t]*$"
    return [
        "".join(line.removeprefix(indent) for line in body.splitlines(keepends=True))
        for indent, body in re.findall(pattern, text, re.MULTILINE | re.DOTALL)
    ]


def helm_commands(document):
    for block in blocks(document, "bash"):
        for line in re.sub(r"\\\n\s*", " ", block).splitlines():
            if re.match(r"^helm (?:template|upgrade|install)\b", line):
                yield shlex.split(line)


class MarkdownBlocksTest(unittest.TestCase):
    def test_collapsed_blocks_preserve_shell_and_yaml_indentation(self):
        command = "cat <<'EOF'\nkind: Cluster\nnodes:\n  - role: worker\nEOF\n"
        for indent in ("", "    "):
            with self.subTest(indent=indent):
                fenced = "```bash\n" + command + "```\n"
                document = "".join(indent + line for line in fenced.splitlines(keepends=True))
                if indent:
                    document = '??? note "Optional: preview"\n\n' + document
                self.assertEqual(blocks(document, "bash"), [command])


class InstallationExamplesTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.helm = shutil.which(os.environ.get("HELM", "helm"))
        if not cls.helm:
            raise RuntimeError("Helm is required for offline installation-example regressions")
        cls.work = ROOT / (".test-installation-" + uuid.uuid4().hex)
        cls.work.mkdir()
        cls.addClassCleanup(shutil.rmtree, cls.work)

    def render(self, arguments):
        return subprocess.run(
            [self.helm, "template", "brewlet", str(CHART), *arguments],
            cwd=self.work, text=True, capture_output=True, timeout=60,
        )

    def inventory(self, document):
        candidates = [block for block in blocks(document, "yaml")
                      if block.startswith("provisioner:\n") and "  jdks:\n" in block]
        self.assertTrue(candidates, "No explicit my-jdks.yaml example")
        return candidates[0]

    def render_arguments(self, command):
        # Preserve documented render-affecting options. Never execute the shell
        # snippet, download an OCI chart, or invoke a Helm install/upgrade.
        result, positionals = [], []
        args = iter(command[2:])
        for arg in args:
            if arg in (">", ">>"):
                break
            if arg in ("--install", "--create-namespace", "--wait"):
                continue
            if arg in ("--version", "--kube-context"):
                next(args)
                continue
            if arg in ("--values", "-f", "--set", "--set-string", "--set-json", "--namespace"):
                value = next(args)
                value = value.replace("$BREWLET_POOL", "java-workers")
                value = value.replace("$BREWLET_WORK/", "")
                value = value.replace("<64-lowercase-hex>", DIGEST)
                value = value.replace("<registry>", "registry.example.com")
                result.extend((arg, value))
                continue
            self.assertFalse(arg.startswith("-"), f"Unrecognized Helm option: {arg}")
            positionals.append(arg)
        self.assertEqual(positionals[0], "brewlet")
        self.assertIn(positionals[1], (
            "oci://ghcr.io/microsoft/charts/brewlet", "./kubernetes/charts/brewlet",
        ))
        self.assertEqual(len(positionals), 2)
        return result

    def test_every_documented_install_preview_and_upgrade_renders(self):
        command_count = 0
        for filename in DOCUMENTS:
            document = (ROOT / filename).read_text()
            inventory = self.inventory(document)
            self.assertIn("@sha256:<64-lowercase-hex>", inventory)
            (self.work / "my-jdks.yaml").write_text(
                inventory.replace("<64-lowercase-hex>", DIGEST)
            )
            # Represent the administrator's existing settings during upgrades.
            saved_images = {name: f"registry.example.com/private/{name}@sha256:{DIGEST}"
                            for name in ("operator", "admission", "provisioner")}
            (self.work / "values.yaml").write_text(json.dumps({
                "provisioner": {"pools": ["existing-workers"]},
                "images": saved_images,
            }))
            (self.work / "brewlet-no-profiles.yaml").write_text(json.dumps({
                "defaultProfile": {"enabled": False}, "profiles": [],
            }))
            commands = list(helm_commands(document))
            self.assertTrue(commands, filename)
            for command in commands:
                command_count += 1
                with self.subTest(document=filename, command=command):
                    args = self.render_arguments(command)
                    result = self.render(args)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    profiles = [doc for doc in result.stdout.split("---\n")
                                if "\nkind: NodeProfile\n" in doc]
                    if "brewlet-no-profiles.yaml" in args:
                        self.assertEqual(profiles, [])
                    else:
                        self.assertEqual(len(profiles), 1)
                        profile = profiles[0]
                        self.assertIn("apiVersion: node.brewlet.sh/v1alpha1\n", profile)
                        self.assertIn("\n  name: default\n", profile)
                        pool = "existing-workers" if "values.yaml" in args else "java-workers"
                        self.assertIn(f'\n      - "{pool}"\n', profile)
                        self.assertIn("    - distribution: temurin\n", profile)
                        self.assertIn("      feature: 21\n", profile)
                        self.assertIn(f"        image: docker.io/library/eclipse-temurin@sha256:{DIGEST}\n",
                                      profile)
                        self.assertIn("        javaHome: /opt/java/openjdk\n", profile)
                        self.assertNotIn("includeControlPlane: true", profile)
                        self.assertNotIn("\n  launchers:", profile)
                    if "values.yaml" in args:
                        for image in saved_images.values():
                            self.assertIn(image, result.stdout, "Upgrade lost administrator image choice")
        self.assertGreaterEqual(command_count, 10, "A documented command disappeared from coverage")

    def test_fresh_install_guides_default_to_latest_chart(self):
        for filename in ("README.md", "docs/installation.md",
                         "kubernetes/charts/brewlet/README.md"):
            with self.subTest(document=filename):
                commands = list(helm_commands((ROOT / filename).read_text()))
                installs = [command for command in commands
                            if "--install" in command
                            and "oci://ghcr.io/microsoft/charts/brewlet" in command]
                self.assertEqual(len(installs), 1)
                self.assertNotIn("--version", installs[0])
                self.assertNotIn("--devel", installs[0])

    def test_existing_installation_upgrades_keep_matching_release_pin(self):
        commands = list(helm_commands((ROOT / "docs/installation.md").read_text()))
        upgrades = [command for command in commands
                    if command[1] == "upgrade" and "--install" not in command]
        self.assertTrue(upgrades)
        for command in upgrades:
            with self.subTest(command=command):
                self.assertIn("--version", command)
                self.assertEqual(command[command.index("--version") + 1], "$RELEASE_VERSION")

    def test_local_kubernetes_preview_and_install_render_the_same_worker_inventory(self):
        document = (ROOT / "docs/local-kubernetes.md").read_text()
        match = re.search(
            r'cat > "\$BREWLET_WORK/brewlet-local.yaml" <<EOF\n(.*?)\nEOF',
            document, re.DOTALL,
        )
        self.assertIsNotNone(match, "Missing local cluster values heredoc")
        inventory = match.group(1).replace("${JDK_DIGEST}", "sha256:" + DIGEST)
        (self.work / "brewlet-local.yaml").write_text(inventory)
        commands = list(helm_commands(document))
        self.assertEqual(len(commands), 2)
        profiles = []
        for command in commands:
            with self.subTest(command=command):
                result = self.render(self.render_arguments(command))
                self.assertEqual(result.returncode, 0, result.stderr)
                rendered = [doc for doc in result.stdout.split("---\n")
                            if "\nkind: NodeProfile\n" in doc]
                self.assertEqual(len(rendered), 1)
                profile = rendered[0]
                profiles.append(profile)
                self.assertIn('- "local-java"', profile)
                self.assertIn('key: "brewlet.sh/local-pool"', profile)
                self.assertNotIn("includeControlPlane: true", profile)
                self.assertNotIn("\n  tolerations:", profile)
                self.assertIn("feature: 21", profile)
                self.assertIn(f"docker.io/library/eclipse-temurin@sha256:{DIGEST}", profile)
                self.assertIn("javaHome: /opt/java/openjdk", profile)
        self.assertEqual(profiles[0], profiles[1])
        self.assertIn('k label node "$BREWLET_NODE" brewlet.sh/local-pool=local-java',
                      document)
        self.assertIn("!node-role.kubernetes.io/control-plane", document)
        self.assertIn("!node-role.kubernetes.io/master", document)

    def test_local_kubernetes_helm_inventory_flags_are_supported(self):
        document = (ROOT / "docs/local-kubernetes.md").read_text()
        commands = re.sub(r"\\\n\s*", " ", "\n".join(blocks(document, "bash")))
        command = next(line for line in commands.splitlines() if line.startswith("helm list "))
        args = [arg.replace("$BREWLET_CONTEXT", "unused-test-context")
                for arg in shlex.split(command)[1:]]
        result = subprocess.run(
            [self.helm, *args, "--help"], text=True, capture_output=True, timeout=10,
        )
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_missing_pools_and_missing_jdks_fail_in_the_actual_chart(self):
        inventory = self.inventory((ROOT / "README.md").read_text())
        (self.work / "my-jdks.yaml").write_text(inventory.replace("<64-lowercase-hex>", DIGEST))
        for args, expected in (
            ([], "provisioner.pools is required"),
            (["--values", "my-jdks.yaml"], "provisioner.pools is required"),
            (["--set", "provisioner.pools={java-workers}"], "provisioner.jdks is required"),
        ):
            with self.subTest(args=args):
                result = self.render(args)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(expected, result.stderr)

    def test_control_plane_requires_explicit_opt_in_without_losing_inventory(self):
        inventory = self.inventory((ROOT / "README.md").read_text())
        (self.work / "my-jdks.yaml").write_text(inventory.replace("<64-lowercase-hex>", DIGEST))
        result = self.render(["--values", "my-jdks.yaml",
                              "--set", "provisioner.pools={java-workers}",
                              "--set", "provisioner.includeControlPlane=true"])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("    includeControlPlane: true\n", result.stdout)
        self.assertIn(f"image: docker.io/library/eclipse-temurin@sha256:{DIGEST}", result.stdout)

    def test_notes_do_not_offer_a_mutable_workload_image(self):
        notes = (CHART / "templates/NOTES.txt").read_text()
        self.assertIn("image: registry.example.com/demo/hello@sha256:<64-lowercase-hex>", notes)
        self.assertNotIn("image: registry.example.com/demo/hello:1.0.0", notes)


if __name__ == "__main__":
    unittest.main()
