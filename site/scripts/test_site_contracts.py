# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Offline contracts for public site examples and capability claims."""

from html.parser import HTMLParser
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import tempfile
import unittest
import xml.etree.ElementTree as ET

from test_installation_examples import blocks


ROOT = Path(__file__).resolve().parents[2]


class LandingPage(HTMLParser):
    def __init__(self, source):
        super().__init__(convert_charrefs=True)
        self.section = None
        self.pre = None
        self.anchor = None
        self.hidden = 0
        self.blocks = []
        self.links = []
        self.text = []
        self.ids = []
        self.resources = []
        self.metadata = {}
        self.feed(source)

    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        if "id" in attrs:
            self.ids.append(attrs["id"])
        if tag == "img":
            self.resources.append(attrs.get("src"))
        if tag == "meta":
            self.metadata[attrs.get("name") or attrs.get("property")] = attrs.get("content")
        if tag in ("script", "style"):
            self.hidden += 1
        if tag == "section":
            self.section = attrs.get("id")
        elif tag == "pre":
            self.pre = []
        elif tag == "a":
            self.anchor = (attrs.get("href"), [])

    def handle_data(self, data):
        if self.hidden:
            return
        self.text.append(data)
        if self.pre is not None:
            self.pre.append(data)
        if self.anchor is not None:
            self.anchor[1].append(data)

    def handle_endtag(self, tag):
        if tag in ("script", "style"):
            self.hidden -= 1
        if tag == "pre":
            self.blocks.append((self.section, "".join(self.pre)))
            self.pre = None
        elif tag == "a" and self.anchor is not None:
            href, text = self.anchor
            self.links.append((href, " ".join("".join(text).split())))
            self.anchor = None
        elif tag == "section":
            self.section = None


class SiteContractsTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.page = LandingPage((ROOT / "site/index.html").read_text(encoding="utf-8"))
        cls.text = " ".join("".join(cls.page.text).split())

    def test_launch_config_is_copyable_json_not_commented_pseudocode(self):
        configs = [json.loads(block) for _, block in self.page.blocks
                   if block.lstrip().startswith("{")]
        self.assertEqual(len(configs), 1)
        config = configs[0]
        self.assertEqual(config["schemaVersion"], 1)
        self.assertEqual(config["mainJar"], "app.jar")
        self.assertEqual(config["entry"], {"mode": "jar"})
        for deployment_field in ("jdk", "launcher", "ports", "jvmArgs", "resources"):
            self.assertNotIn(deployment_field, config)

    def test_complete_workload_descriptor_uses_an_explicit_example_digest(self):
        descriptors = [block for _, block in self.page.blocks
                       if "kind: JavaApplication" in block]
        self.assertEqual(len(descriptors), 1)
        descriptor = descriptors[0]
        self.assertIn("apiVersion: apps.brewlet.sh/v1alpha1", descriptor)
        images = re.findall(r"^\s*image:\s*(\S+)\s*$", descriptor, re.MULTILINE)
        self.assertEqual(len(images), 1)
        self.assertRegex(images[0], r"^registry\.example\.com/[\w/-]+@sha256:[0-9a-f]{64}$")
        self.assertIn("illustrative", descriptor)
        self.assertIn("Use the digest printed by brewlet:push", descriptor)

    def test_cli_examples_explicitly_use_local_stores(self):
        commands = []
        for _, block in self.page.blocks:
            logical = re.sub(r"\\\n\s*", " ", block)
            for line in logical.splitlines():
                if re.match(r"^\$ (?:bin/)?brewlet (?:push|run|inspect)\b", line):
                    commands.append(shlex.split(line[2:]))
        self.assertGreaterEqual(len(commands), 4)
        for command in commands:
            with self.subTest(command=command):
                self.assertIn("--store", command)
                self.assertEqual(command[command.index("--store") + 1], "./oci")
                if command[1] == "push":
                    self.assertIn("--format", command)
                    self.assertIn(command[command.index("--format") + 1], ("artifact", "image"))
        self.assertIn("Go CLI does not upload to a registry", self.text)
        self.assertIn("Maven plugin publishes", self.text)

    def test_quickstart_uses_released_cli_plugin_and_matching_example_source(self):
        blocks = "\n".join(block for section, block in self.page.blocks if section == "quickstart")
        self.assertIn('export BREWLET_VERSION="0.5.0"', blocks)
        self.assertIn("curl -fsSL https://brewlet.sh/install.sh | sh", blocks)
        self.assertLess(blocks.index("export BREWLET_VERSION"), blocks.index("install.sh"))
        self.assertIn('export PATH="$HOME/.local/bin:$PATH"', blocks)
        self.assertIn('git clone --depth 1 --branch "v${BREWLET_VERSION}" '
                      'https://github.com/microsoft/brewlet.git', blocks)
        self.assertIn("integration-tests/fixtures/demo-app/pom.xml", blocks)
        self.assertTrue((ROOT / "integration-tests/fixtures/demo-app/pom.xml").is_file())
        for extension in ("jar", "pom"):
            self.assertIn("https://github.com/microsoft/brewlet/releases/download/"
                          "v${BREWLET_VERSION}/brewlet-maven-plugin-${BREWLET_VERSION}."
                          + extension, blocks)
        self.assertIn("maven-install-plugin:3.1.4:install-file", blocks)
        self.assertIn('sh.brewlet:brewlet-maven-plugin:${BREWLET_VERSION}:build', blocks)
        self.assertNotIn("make binaries", blocks)
        self.assertNotIn("maven-plugin/pom.xml", blocks)
        self.assertNotIn("bin/brewlet", blocks)

    def test_documented_quickstarts_use_the_release_installer_by_default(self):
        for filename in ("README.md", "docs/getting-started.md", "docs/local-kubernetes.md",
                         "docs/workshops/operations.md", "docs/workshops/developers.md"):
            with self.subTest(document=filename):
                document = (ROOT / filename).read_text(encoding="utf-8")
                primary_path = document.split("### Alternative: build from source")[0]
                self.assertIn('export BREWLET_VERSION="0.5.0"', primary_path)
                self.assertIn("curl -fsSL https://brewlet.sh/install.sh | sh", primary_path)
                self.assertLess(primary_path.index("export BREWLET_VERSION"),
                                primary_path.index("install.sh"))
                path = ('$BREWLET_INSTALL_DIR' if filename == "docs/local-kubernetes.md"
                        else '$HOME/.local/bin')
                self.assertIn(f'export PATH="{path}:$PATH"', primary_path)
                self.assertNotIn("make binaries", primary_path)
        developers = (ROOT / "docs/workshops/developers.md").read_text(encoding="utf-8")
        self.assertIn('git clone --depth 1 --branch "v${BREWLET_VERSION}"', developers)
        self.assertIn("maven-install-plugin:3.1.4:install-file", developers)

    def test_public_content_does_not_condition_release_access_on_repository_visibility(self):
        paths = [ROOT / "README.md", ROOT / "kubernetes/charts/brewlet/README.md",
                 *sorted((ROOT / "site").glob("index*.html")), ROOT / "site/README.md",
                 *sorted((ROOT / "docs").rglob("*.md"))]
        restrictions = (
            r"\bprivate[\s-]+preview\b",
            r"\bsource preview\b",
            r"\brepository and package access\b",
            r"\b(?:authenticated|authorized) (?:source )?checkout\b",
            r"\bonly (?:once|when) (?:release )?assets are publicly accessible\b",
            r"\bwhen the assets are public\b",
            r"\banonymous release downloads are unavailable\b",
            r"\balternative-publicly-accessible-release-assets\b",
        )
        for path in paths:
            with self.subTest(document=str(path.relative_to(ROOT))):
                text = " ".join(re.sub(r"[*`]", "", path.read_text(encoding="utf-8")).split())
                for restriction in restrictions:
                    self.assertNotRegex(text, re.compile(restriction, re.IGNORECASE))

    def test_runtime_and_supply_chain_limits_are_explicit(self):
        for statement in (
            "Each workload retains its own JVM and heap",
            "shared JDK storage does not imply automatic memory savings",
            "Roll workloads to move running JVMs onto the new runtime",
            "gVisor and Kata integrations are not supported",
            "General cosign or standard SLSA admission for application images is not implemented",
            "Component releases carry verifiable build provenance",
        ):
            self.assertIn(statement, self.text)
        for old_claim in ("Use gVisor/Kata", "Brewlet injects no JVM flags of its own",
                          "shim pulls the artifact directly", "~250 MB"):
            self.assertNotIn(old_claim, self.text)

    def test_architecture_images_attribute_registry_pulls_to_cri(self):
        ns = {"svg": "http://www.w3.org/2000/svg"}
        for name in ("architecture-desktop.svg", "architecture-mobile.svg"):
            with self.subTest(image=name):
                root = ET.parse(ROOT / "site/assets/images" / name).getroot()
                description = root.find("svg:desc", ns).text
                labels = " ".join("".join(node.itertext()) for node in root.findall("svg:text", ns))
                self.assertIn("CRI pulls", description)
                self.assertIn("content-store", description)
                self.assertIn("CRI pulls", labels)
                self.assertNotIn("shim pulls", labels)
                self.assertIn("mvn brewlet:push", labels)

    def test_component_links_point_to_their_actual_subprojects(self):
        for title, directory in (("Core runtime", "core"), ("Site and docs", "site")):
            links = [href for href, text in self.page.links if text.startswith(title)]
            self.assertEqual(links, ["https://github.com/microsoft/brewlet/tree/main/" + directory])


class LocalKubernetesGuideTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.document = (ROOT / "docs/local-kubernetes.md").read_text(encoding="utf-8")
        cls.commands = blocks(cls.document, "bash")

    def test_try_it_navigation_leads_to_the_complete_local_guide(self):
        config = (ROOT / "site/mkdocs.yml").read_text(encoding="utf-8")
        try_it = config.split("  - Try it:\n", 1)[1].split("  - Workshops:", 1)[0]
        self.assertIn("Getting started: getting-started.md", try_it)
        self.assertIn("Local Kubernetes: local-kubernetes.md", try_it)
        self.assertNotIn("spring-petclinic.md", try_it)
        for filename in ("docs/README.md", "docs/getting-started.md",
                         "docs/installation.md", "docs/spring-petclinic.md"):
            with self.subTest(document=filename):
                self.assertIn("(local-kubernetes.md)",
                              (ROOT / filename).read_text(encoding="utf-8"))
        page = LandingPage((ROOT / "site/index.html").read_text(encoding="utf-8"))
        self.assertIn(("/docs/local-kubernetes/", "Try local Kubernetes"), page.links)
        legacy = (ROOT / "docs/spring-petclinic.md").read_text(encoding="utf-8")
        self.assertIn("## Layered classpath delivery", legacy)

    def test_preloaded_image_uses_a_digest_and_kubernetes_containerd_store(self):
        commands = "\n".join(self.commands)
        self.assertIn('git clone --depth 1 --branch "v${BREWLET_VERSION}"', commands)
        self.assertIn('"$FIXTURE_DIR/build.sh"', commands)
        self.assertIn("--format image", commands)
        self.assertNotIn("--format artifact", commands)
        self.assertIn('select(.annotations["org.opencontainers.image.ref.name"] == $ref)',
                      commands)
        self.assertIn('PETCLINIC_IMAGE="${PETCLINIC_REF%:*}@${PETCLINIC_DIGEST}"', commands)
        self.assertIn('docker exec -i "$node" ctr -n k8s.io images import --digests -',
                      commands)
        self.assertIn('ctr -n k8s.io images tag', commands)
        self.assertNotIn("--force", commands)
        self.assertIn('crictl inspecti "$PETCLINIC_IMAGE"', commands)
        self.assertIn("brewlet.sh/runtime=ready,brewlet.sh/jdk.temurin-21=true", commands)
        self.assertIn('done <<< "$BREWLET_NODES"', commands)
        self.assertIn('PETCLINIC_REF="localhost/brewlet/${BREWLET_NAMESPACE}:local"', commands)
        self.assertIn("    image: ${PETCLINIC_IMAGE}\n    pullPolicy: Never\n", commands)
        self.assertIn("    version: 21\n    distribution: temurin\n", commands)
        self.assertIn("  replicas: 1\n", commands)
        self.assertNotIn("autoscaling:", commands)
        self.assertIn("path: /actuator/health\n", commands)
        self.assertIn("port-forward service/petclinic 18080:8080", commands)
        self.assertNotIn("service/petclinic 8080:80\n", commands)
        self.assertIn("k wait --for=condition=Ready javaapplication/petclinic", commands)

    def test_existing_installations_are_inspected_not_upgraded_or_reset(self):
        commands = "\n".join(self.commands)
        self.assertIn('k() { kubectl --context "$BREWLET_CONTEXT" "$@"; }', commands)
        self.assertNotIn("kubectl config use-context", commands)
        self.assertIn('helm list --kube-context "$BREWLET_CONTEXT" --all-namespaces --all',
                      commands)
        self.assertIn('find /opt /usr/local/bin /etc/containerd', commands)
        self.assertIn('sed -n "/brewlet/p" /etc/containerd/config.toml', commands)
        self.assertIn("helm install brewlet", commands)
        self.assertNotIn("helm upgrade", commands)
        self.assertNotIn("--overwrite", commands)
        self.assertIn('export BREWLET_INSTALLED_HERE=false', commands)
        self.assertIn('if [ "$BREWLET_INSTALLED_HERE" = true ]; then', commands)
        self.assertIn('if [ "$BREWLET_POOL_LABEL_ADDED" = true ]; then', commands)
        self.assertNotIn("delete namespace petclinic", commands)
        self.assertNotIn("delete crd", commands)
        self.assertIn('BREWLET_NAMESPACE="petclinic-${BREWLET_RUN}"', commands)
        self.assertIn('BREWLET_WORK="$(mktemp -d "$PWD/brewlet-local.XXXXXX")"', commands)
        self.assertIn('export BREWLET_INSTALL_DIR="$BREWLET_WORK/bin"', commands)
        self.assertIn('env -u DOCKER_DEFAULT_PLATFORM kind create cluster', commands)
        self.assertIn('.platform.architecture == $arch', commands)
        self.assertIn('--image "kindest/node@${KIND_DIGEST}"', commands)
        self.assertIn('--kubeconfig "$BREWLET_WORK/kubeconfig"', commands)

    def test_namespace_cleanup_refuses_foreign_ownership(self):
        cleanup = next(block for block in self.commands if block.startswith("RUN_OWNER="))
        harness = """
set -euo pipefail
k() {
  case "$1" in
    get) printf '%s' "$MOCK_OWNER" ;;
    delete) printf 'DELETE %s\\n' "$3" ;;
    *) printf 'Unexpected command\\n' >&2; return 99 ;;
  esac
}
"""
        for owner in ("run-123", "someone-else", ""):
            with self.subTest(owner=owner):
                result = subprocess.run(
                    ["bash"], input=harness + cleanup, text=True, capture_output=True,
                    timeout=10, env={**os.environ, "MOCK_OWNER": owner,
                                     "BREWLET_RUN": "run-123",
                                     "BREWLET_NAMESPACE": "petclinic-run-123"},
                )
                if owner == "run-123":
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertIn("DELETE petclinic-run-123", result.stdout)
                else:
                    self.assertNotEqual(result.returncode, 0)
                    self.assertNotIn("DELETE", result.stdout)
                    self.assertIn("not owned by this tutorial run", result.stderr)

    def test_pool_label_is_added_only_when_absent(self):
        setup = next(block for block in self.commands if block.startswith("POOL_BEFORE="))
        harness = """
set -euo pipefail
export BREWLET_POOL_LABEL_ADDED=false
k() {
  case "$1 $2" in
    "get node") printf '%s' "$MOCK_POOL" ;;
    "get nodes") printf '%s\\n' "$MOCK_NODES" ;;
    "label node") printf 'LABEL %s\\n' "$4" ;;
    *) printf 'Unexpected command\\n' >&2; return 99 ;;
  esac
}
"""
        with tempfile.TemporaryDirectory(prefix="brewlet-guide-test-") as work:
            cases = (("", "selected-worker"), ("local-java", "selected-worker"),
                     ("someone-elses-pool", "selected-worker"),
                     ("local-java", "selected-worker\nother-worker"))
            for pool, nodes in cases:
                with self.subTest(pool=pool, nodes=nodes):
                    result = subprocess.run(
                        ["bash"], input=harness + setup + '\nprintf "ADDED=%s\\n" "$BREWLET_POOL_LABEL_ADDED"\n',
                        text=True, capture_output=True, timeout=10,
                        env={**os.environ, "MOCK_POOL": pool, "MOCK_NODES": nodes,
                             "BREWLET_WORK": work,
                             "BREWLET_NODE": "selected-worker", "JDK_DIGEST": "sha256:" + "a" * 64},
                    )
                    if pool == "someone-elses-pool":
                        self.assertNotEqual(result.returncode, 0)
                        self.assertNotIn("LABEL", result.stdout)
                        self.assertIn("do not overwrite", result.stderr)
                    elif nodes != "selected-worker":
                        self.assertNotEqual(result.returncode, 0)
                        self.assertNotIn("LABEL", result.stdout)
                        self.assertIn("selects other nodes", result.stderr)
                    else:
                        self.assertEqual(result.returncode, 0, result.stderr)
                        self.assertIn("ADDED=" + ("false" if pool else "true"), result.stdout)
                        self.assertEqual("LABEL" in result.stdout, not pool)

    def test_platform_cleanup_preserves_reused_resources_and_stops_on_failure(self):
        cleanup = next(block for block in self.commands
                       if block.startswith('if [ "$BREWLET_INSTALLED_HERE" = true ]; then'))
        harness = """
set -euo pipefail
helm() {
  printf 'UNINSTALL\\n'
  return "$MOCK_HELM_EXIT"
}
k() {
  case "$1" in
    get) printf '%s' "$MOCK_POOL" ;;
    label) printf 'REMOVE_LABEL\\n' ;;
    *) printf 'Unexpected command\\n' >&2; return 99 ;;
  esac
}
"""
        cases = (
            ("false", "false", "local-java", "0", 0, False, False),
            ("true", "false", "local-java", "0", 0, True, False),
            ("true", "true", "local-java", "0", 0, True, True),
            ("true", "true", "local-java", "1", 1, True, False),
            ("true", "true", "changed-pool", "0", 1, True, False),
        )
        for installed, added, pool, helm_exit, exit_code, uninstalled, removed in cases:
            with self.subTest(installed=installed, added=added, pool=pool, helm_exit=helm_exit):
                result = subprocess.run(
                    ["bash"], input=harness + cleanup, text=True, capture_output=True,
                    timeout=10, env={
                        **os.environ, "BREWLET_INSTALLED_HERE": installed,
                        "BREWLET_POOL_LABEL_ADDED": added, "MOCK_POOL": pool,
                        "MOCK_HELM_EXIT": helm_exit, "BREWLET_CONTEXT": "local-test",
                        "BREWLET_NODE": "selected-worker",
                    },
                )
                self.assertEqual(result.returncode, exit_code, result.stderr)
                self.assertEqual("UNINSTALL" in result.stdout, uninstalled)
                self.assertEqual("REMOVE_LABEL" in result.stdout, removed)

    def test_shell_examples_are_syntactically_valid(self):
        self.assertGreater(len(self.commands), 10)
        for number, block in enumerate(self.commands, 1):
            with self.subTest(block=number):
                result = subprocess.run(
                    ["bash", "-n"], input=block, text=True,
                    capture_output=True, timeout=10,
                )
                self.assertEqual(result.returncode, 0, result.stderr)


class ValuePropositionPageTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.source = (ROOT / "site/index-value-prop.html").read_text(encoding="utf-8")
        cls.page = LandingPage(cls.source)
        cls.text = " ".join("".join(cls.page.text).split())

    def test_entry_point_is_independent_and_not_linked_from_homepage(self):
        homepage = (ROOT / "site/index.html").read_text(encoding="utf-8")
        self.assertNotIn("index-value-prop.html", homepage)
        self.assertNotIn("assets/css/styles.css", self.source)
        self.assertNotIn("assets/js/", self.source)
        self.assertEqual(self.page.metadata["robots"], "noindex, follow")
        self.assertEqual(self.page.metadata["og:url"],
                         "https://brewlet.sh/index-value-prop.html")
        self.assertIn('<link rel="canonical" href="https://brewlet.sh/index-value-prop.html"',
                      self.source)

    def test_internal_links_assets_and_workshop_actions_resolve(self):
        self.assertEqual(len(self.page.ids), len(set(self.page.ids)))
        for href, _ in self.page.links:
            with self.subTest(href=href):
                if href.startswith("#"):
                    self.assertIn(href[1:], self.page.ids)
                elif href.startswith("/docs/"):
                    path = ROOT / "docs" / href.removeprefix("/docs/")
                    candidates = [path / "index.md", path / "README.md",
                                  path.with_suffix(".md")]
                    self.assertTrue(any(candidate.is_file() for candidate in candidates))
        for resource in self.page.resources:
            self.assertTrue((ROOT / "site" / resource).is_file())
        links = {href for href, _ in self.page.links}
        self.assertIn("/docs/workshops/operations/", links)
        self.assertIn("/docs/workshops/developers/", links)

    def test_message_distinguishes_lifecycles_from_performance_claims(self):
        for statement in (
            "Ship Java applications.",
            "Govern runtimes centrally.",
            "without republishing application images for runtime-only updates",
            "The restarts remain.",
            "Dependency updates still require new application images",
            "Performance must be no worse than conventional container deployments",
            "not a result Brewlet has already established",
            "21% higher sampled JVM-plus-shim PSS",
            "automatic per-replica runtime reporting remains",
            "Jib or buildpacks",
        ):
            with self.subTest(statement=statement):
                self.assertIn(statement, self.text)

    def test_runtime_exhibit_is_explicitly_illustrative_and_keyboard_operable(self):
        self.assertIn("Illustrative runtime update, not a live deployment", self.text)
        self.assertIn("Running JVMs retain the old runtime until restarted", self.text)
        self.assertIn("Same application image digest in both states", self.text)
        for state in ("before", "after"):
            self.assertIn(f'type="radio" id="{state}" name="runtime-stage"', self.source)
            self.assertIn(f'<label for="{state}">', self.source)
        self.assertIn('#before:checked ~ .after-state', self.source)
        self.assertIn('#after:checked ~ .before-state', self.source)


if __name__ == "__main__":
    unittest.main()
