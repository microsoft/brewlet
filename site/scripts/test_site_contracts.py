# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Offline contracts for public site examples and capability claims."""

from html.parser import HTMLParser
import json
from pathlib import Path
import re
import shlex
import unittest
import xml.etree.ElementTree as ET


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
        self.feed(source)

    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
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
        self.assertIn('export BREWLET_VERSION="0.4.0"', blocks)
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
        for filename in ("docs/getting-started.md", "docs/workshops/operations.md",
                         "docs/workshops/developers.md"):
            with self.subTest(document=filename):
                document = (ROOT / filename).read_text(encoding="utf-8")
                primary_path = document.split("### Alternative: build from source")[0]
                self.assertIn('export BREWLET_VERSION="0.4.0"', primary_path)
                self.assertIn("curl -fsSL https://brewlet.sh/install.sh | sh", primary_path)
                self.assertLess(primary_path.index("export BREWLET_VERSION"),
                                primary_path.index("install.sh"))
                self.assertIn('export PATH="$HOME/.local/bin:$PATH"', primary_path)
                self.assertNotIn("make binaries", primary_path)
        developers = (ROOT / "docs/workshops/developers.md").read_text(encoding="utf-8")
        self.assertIn('git clone --depth 1 --branch "v${BREWLET_VERSION}"', developers)
        self.assertIn("maven-install-plugin:3.1.4:install-file", developers)

    def test_public_content_does_not_condition_release_access_on_repository_visibility(self):
        paths = [ROOT / "site/index.html", ROOT / "site/README.md",
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


if __name__ == "__main__":
    unittest.main()
