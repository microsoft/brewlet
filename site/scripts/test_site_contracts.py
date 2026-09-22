# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Offline contracts for public site examples and capability claims."""

from html.parser import HTMLParser
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import tempfile
import unittest
from urllib.parse import urlsplit
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

    def test_complete_workload_descriptor_uses_an_explicit_digest_placeholder(self):
        descriptors = [block for _, block in self.page.blocks
                       if "kind: JavaApplication" in block]
        self.assertEqual(len(descriptors), 1)
        descriptor = descriptors[0]
        self.assertIn("apiVersion: apps.brewlet.sh/v1alpha1", descriptor)
        images = re.findall(r"^\s*image:\s*(\S+)\s*$", descriptor, re.MULTILINE)
        self.assertEqual(len(images), 1)
        self.assertEqual(images[0], "registry.example.com/team/app@sha256:<published-digest>")
        self.assertIn("Replace <published-digest> with the full digest printed by brewlet:push",
                      descriptor)

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
        self.assertIn('$ curl -fsSL https://brewlet.sh/install.sh | sh\n', blocks)
        self.assertIn('export PATH="$HOME/.local/bin:$PATH"', blocks)
        self.assertNotIn("BREWLET_VERSION", self.text)
        self.assertLess(blocks.index("install.sh"), blocks.index("export PATH"))
        self.assertLess(blocks.index("export PATH"), blocks.index("git clone"))
        self.assertIn('git clone --depth 1 --branch "v$(brewlet version)" '
                      'https://github.com/microsoft/brewlet.git', blocks)
        self.assertIn("integration-tests/fixtures/demo-app/pom.xml", blocks)
        self.assertTrue((ROOT / "integration-tests/fixtures/demo-app/pom.xml").is_file())
        self.assertIn("Maven Central", blocks)
        self.assertNotIn("maven-install-plugin", blocks)
        self.assertNotIn("releases/download", blocks)
        self.assertIn('sh.brewlet:brewlet-maven-plugin:$(brewlet version):build', blocks)
        self.assertIn('sh.brewlet:brewlet-maven-plugin:$(brewlet version):push', self.text)
        self.assertEqual(blocks.count("demo/hello:local"), 4)
        self.assertNotIn("make binaries", blocks)
        self.assertNotIn("maven-plugin/pom.xml", blocks)
        self.assertNotIn("bin/brewlet", blocks)

    def test_landing_pages_do_not_hardcode_brewlet_release_numbers(self):
        for filename in ("index.html", "index-value-prop.html"):
            with self.subTest(page=filename):
                page = LandingPage((ROOT / "site" / filename).read_text(encoding="utf-8"))
                text = "".join(page.text)
                # Application tags and third-party tool versions are not Brewlet releases.
                text = re.sub(r"(?:app:|demo/hello:|orders / |maven-install-plugin:)"
                              r"\d+\.\d+\.\d+", "example", text)
                self.assertEqual(re.findall(r"\b\d+\.\d+\.\d+\b", text), [])

    def test_documented_quickstarts_use_the_release_installer_by_default(self):
        for filename in ("README.md", "docs/workshops/operations.md",
                         "docs/workshops/developers.md"):
            with self.subTest(document=filename):
                document = (ROOT / filename).read_text(encoding="utf-8")
                primary_path = document.split("### Alternative: build from source")[0]
                self.assertIn("curl -fsSL https://brewlet.sh/install.sh | sh", primary_path)
                self.assertNotRegex(primary_path, r'export BREWLET_VERSION="\d')
                if filename != "docs/workshops/developers.md":
                    self.assertIn('--version latest --install-dir "$HOME/.local/bin"',
                                  primary_path)
                    if filename == "docs/workshops/operations.md":
                        self.assertIn('BREWLET_VERSION="$(brewlet version)"', primary_path)
                        self.assertLess(primary_path.index("install.sh"),
                                        primary_path.index('BREWLET_VERSION="$(brewlet version)"'))
                else:
                    self.assertIn('export BREWLET_VERSION="<version-from-ops-handoff>"',
                                  primary_path)
                    self.assertIn('--version "$BREWLET_VERSION" --install-dir '
                                  '"$HOME/.local/bin"', primary_path)
                    self.assertNotIn("--version latest", primary_path)
                    self.assertLess(primary_path.index("export BREWLET_VERSION"),
                                    primary_path.index("install.sh"))
                self.assertIn('export PATH="$HOME/.local/bin:$PATH"', primary_path)
                self.assertNotIn("make binaries", primary_path)
        developers = (ROOT / "docs/workshops/developers.md").read_text(encoding="utf-8")
        self.assertIn('git clone --depth 1 --branch "v${BREWLET_VERSION}"', developers)
        self.assertIn("Maven Central", developers)
        self.assertNotIn("maven-install-plugin", developers)
        self.assertIn("brewlet:config brewlet:build", developers)
        self.assertIn("  brewlet:push", developers)
        self.assertLess(developers.index("<artifactId>brewlet-maven-plugin</artifactId>"),
                        developers.index("brewlet:config brewlet:build"))

    def test_plugin_guides_only_install_from_central(self):
        for filename in ("maven-plugin/README.md", "docs/building-and-publishing.md",
                         "docs/workshops/developers.md", "site/index.html",
                         "site/scripts/verify-release-artifacts.sh"):
            with self.subTest(document=filename):
                document = (ROOT / filename).read_text(encoding="utf-8")
                self.assertIn("Maven Central", document)
                self.assertNotIn("maven-install-plugin", document)
                self.assertNotIn("github-release-fallback", document)
                self.assertNotRegex(
                    document,
                    r"releases/download/\S*brewlet-maven-plugin",
                )

    def test_usage_examples_do_not_hardcode_brewlet_release_numbers(self):
        documents = (
            "docs/cli-reference.md", "docs/configuration.md", "docs/installation.md",
            "docs/runtime-metrics.md", "docs/building-and-publishing.md",
            "docs/managed-dependency-bundles.md", "kubernetes/README.md",
            "docs/workshops/operations.md", "docs/workshops/developers.md",
            "docs/workshops/index.md",
        )
        release_literal = re.compile(
            r"(?:releases/(?:tag|download)/v|--version[= ]+[\"']?|--branch[= ]+[\"']?v|"
            r"BREWLET_VERSION=[\"']?|brewlet-maven-plugin[:_-]|"
            r"brewlet-(?:operator|admission|node-provisioner):|brewlet_)"
            r"\d+\.\d+\.\d+"
        )
        for filename in documents:
            with self.subTest(document=filename):
                document = (ROOT / filename).read_text(encoding="utf-8")
                for command in blocks(document, "bash"):
                    self.assertNotRegex(command, release_literal)
                self.assertNotRegex(document, r"releases/(?:tag|download)/v\d+\.\d+\.\d+")

    def test_maven_examples_use_the_resolved_release_environment(self):
        count = 0
        for filename in ("docs/building-and-publishing.md", "docs/managed-dependency-bundles.md",
                         "docs/workshops/developers.md", "maven-plugin/README.md"):
            document = (ROOT / filename).read_text(encoding="utf-8")
            for example in blocks(document, "xml"):
                root = ET.fromstring(f"<example>{example}</example>")
                for plugin in root.findall(".//plugin"):
                    if plugin.findtext("artifactId") == "brewlet-maven-plugin":
                        count += 1
                        self.assertEqual(plugin.findtext("version"), "${env.BREWLET_VERSION}")
            self.assertIn("concrete", document)
            self.assertIn("reproducible", document)
        self.assertEqual(count, 7)

    def test_plugin_setup_resolves_and_exports_one_version_without_installing(self):
        document = (ROOT / "docs/building-and-publishing.md").read_text(encoding="utf-8")
        commands = blocks(document, "bash")
        lookup = next(block for block in commands if block.startswith('release_url='))
        export = next(block for block in commands if block.startswith("export BREWLET_VERSION"))
        publish = next(block for block in commands if block.startswith("# Build the fat JAR"))
        one_off = next(block for block in commands
                       if block.startswith('mvn clean package "sh.brewlet:') and
                       "-Dbrewlet.layered" not in block)
        harness = r"""
set -eu
curl() {
  printf '%s\n' "$*" >> "$COMMAND_LOG"
  if [ "$1" = "-fsSL" ]; then
    printf '%s\n' 'https://github.com/microsoft/brewlet/releases/tag/v9.8.7'
  else
    return 99
  fi
}
mvn() {
  printf '%s\n' "$*" >> "$COMMAND_LOG"
  test "$BREWLET_VERSION" = 9.8.7
}
"""
        with tempfile.TemporaryDirectory() as directory:
            log = Path(directory) / "commands.log"
            result = subprocess.run(
                ["bash"], input=harness + lookup + "\n" + export + "\n" + publish + "\n" + one_off
                + '\nsh -c \'test "$BREWLET_VERSION" = 9.8.7\'\n',
                cwd=directory, env={**os.environ, "COMMAND_LOG": str(log),
                                    "BREWLET_VERSION": "0.0.0"},
                text=True, capture_output=True, timeout=10,
            )
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            calls = log.read_text().splitlines()
            self.assertEqual(len(calls), 3)
            self.assertEqual(calls[0], "-fsSL -o /dev/null -w %{url_effective} "
                             "https://github.com/microsoft/brewlet/releases/latest")
            self.assertIn("clean package brewlet:push", calls[1])
            self.assertIn("sh.brewlet:brewlet-maven-plugin:9.8.7:push", calls[2])

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

    def test_landing_pages_link_preview_validation_limits(self):
        for filename, validation_link in (
            ("index.html", "/docs/live-validation/"),
            ("index-value-prop.html", "/docs/#preview-status-and-validation"),
        ):
            with self.subTest(page=filename):
                page = LandingPage((ROOT / "site" / filename).read_text(encoding="utf-8"))
                text = " ".join("".join(page.text).split())
                self.assertIn("pre-1.0 preview", text)
                self.assertIn("disposable evaluation environment", text)
                links = [href for href, _ in page.links]
                self.assertIn(validation_link, links)
                self.assertIn("https://github.com/microsoft/brewlet/issues/95", links)
                self.assertNotIn("full (Services, probes, HPA, logs)", text)
                self.assertNotIn("HPA and Services all work unchanged", text)

    def test_docs_homepage_describes_zero_x_as_unstable_preview(self):
        document = (ROOT / "docs/README.md").read_text(encoding="utf-8")
        self.assertIn("0.x versions are preview releases, not stable releases", document)
        self.assertNotRegex(document, r"\b\d+\.\d+\.\d+\b")

    def test_validation_status_distinguishes_smoke_component_and_live_coverage(self):
        document = (ROOT / "docs/README.md").read_text(encoding="utf-8")
        text = " ".join(document.split())
        self.assertIn("## Preview status and validation", document)
        for boundary in ("does not install the chart or provision nodes",
                         "substituted registry access and plugin transport",
                         "simulated HPA ownership",
                         "fixed-shim candidate",
                         "packed-layer GC scale-out failure",
                         "corrected Verifier manifest",
                         "47 real registry",
                         "two consecutive fresh disposable clusters",
                         "existing E2E harness permits skips"):
            self.assertIn(boundary, text)
        for issue in (13, 93, 94, 95):
            self.assertIn(f"https://github.com/microsoft/brewlet/issues/{issue}", document)

    def test_operational_guides_scope_live_candidate_validation(self):
        guides = {
            "docs/admission-enforcement.md": 95,
            "admission/README.md": 95,
            "docs/security.md": 95,
            "docs/deploying-workloads.md": 94,
            "docs/observability.md": 94,
        }
        for filename, issue in guides.items():
            with self.subTest(document=filename):
                text = " ".join((ROOT / filename).read_text(encoding="utf-8").split())
                self.assertIn(f"https://github.com/microsoft/brewlet/issues/{issue}", text)
                self.assertIn("0.5.0", text)
                if issue == 95:
                    self.assertIn("corrected Verifier manifest", text)
                    self.assertIn("fixture-only", text)
                else:
                    self.assertIn("fixed-shim candidate", text)
                    self.assertIn("cold startup", text)
                self.assertIn("disposable", text)
                self.assertNotIn("production admission integration", text)
                self.assertNotIn("Production admission policy that", text)
                self.assertNotIn("HPA and metrics-server work.", text)
                self.assertNotIn("HPA works against CPU/memory or custom/Prometheus metrics as usual.", text)

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

    def test_in_page_links_point_to_existing_sections(self):
        for href, text in self.page.links:
            if href and href.startswith("#"):
                with self.subTest(link=text):
                    self.assertIn(href[1:], self.page.ids)


class LocalGuideSetupTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.documents = {
            name: (ROOT / f"docs/{name}.md").read_text(encoding="utf-8")
            for name in ("getting-started",)
        }
        cls.commands = {name: blocks(document, "bash")
                        for name, document in cls.documents.items()}

    def setup_commands(self, name):
        commands = self.commands[name]
        installer = next(block for block in commands
                         if block.startswith("curl -fsSL https://brewlet.sh/install.sh"))
        source = next(block for block in commands
                      if block.startswith("export BREWLET_SOURCE="))
        return installer + "\n" + source

    def run_setup(self, name, home, source=None, after=""):
        harness = r"""
set -euo pipefail
curl() {
  [ "$*" = "-fsSL https://brewlet.sh/install.sh" ] || return 99
  cat <<'INSTALLER'
set -eu
test "$1" = --version
test "$2" = latest
test "$3" = --install-dir
test "$4" = "$HOME/.local/bin"
test "$#" = 4
mkdir -p "$4"
printf '#!/bin/sh\n[ "$1" = version ] || exit 99\nprintf "9.8.7\\n"\n' > "$4/brewlet"
chmod +x "$4/brewlet"
INSTALLER
}
git() {
  [ "$#" = 7 ] || return 99
  [ "$1 $2 $3 $4 $5 $6" = \
    "clone --depth 1 --branch v9.8.7 https://github.com/microsoft/brewlet.git" ] || return 99
  printf '%s\n' "$7" >> "$HOME/clones.log"
  mkdir -p "$7/integration-tests/fixtures/demo-app" \
    "$7/integration-tests/fixtures/spring-petclinic"
  touch "$7/integration-tests/fixtures/demo-app/pom.xml" \
    "$7/integration-tests/fixtures/spring-petclinic/build.sh"
}
"""
        env = {**os.environ, "HOME": str(home), "BREWLET_VERSION": "0.0.0"}
        env.pop("BREWLET_SOURCE", None)
        if source is not None:
            env["BREWLET_SOURCE"] = str(source)
        return subprocess.run(
            ["bash"], input=harness + self.setup_commands(name) + "\n" + after,
            cwd=home, env=env, text=True, capture_output=True, timeout=10,
        )

    def test_quickstart_uses_latest_installer_and_matching_checkout(self):
        for name, document in self.documents.items():
            with self.subTest(document=name):
                self.assertNotRegex(document, r'export BREWLET_VERSION="\d')
                setup = self.setup_commands(name)
                self.assertIn('--version latest --install-dir "$HOME/.local/bin"', setup)
                self.assertIn('BREWLET_VERSION="$(brewlet version)"', setup)
                self.assertLess(setup.index("install.sh"), setup.index("brewlet version"))
                self.assertLess(setup.index("brewlet version"), setup.index("git clone"))
                self.assertNotIn("make binaries", setup)
        workflow = (ROOT / ".github/workflows/site-pages.yml").read_text()
        self.assertIn("run: ./site/scripts/verify-release-artifacts.sh\n", workflow)

    def test_quickstart_reruns_clone_once_across_new_terminals(self):
        for order in (("getting-started", "getting-started"),):
            with self.subTest(order=order), tempfile.TemporaryDirectory() as directory:
                home = Path(directory) / "home with spaces"
                home.mkdir()
                source = home / "brewlet-examples"
                result = self.run_setup(order[0], home)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn("Using Brewlet 9.8.7", result.stdout)
                marker = source / "local-changes"
                marker.write_text("keep my work")
                result = self.run_setup(order[1], home)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertEqual((home / "clones.log").read_text().splitlines(), [str(source)])
                self.assertEqual(marker.read_text(), "keep my work")

    def test_existing_source_is_reused_without_git_metadata(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            source = home / "existing release source"
            fixtures = []
            for fixture in ("demo-app/pom.xml", "spring-petclinic/build.sh"):
                path = source / "integration-tests/fixtures" / fixture
                path.parent.mkdir(parents=True)
                path.write_text("existing fixture")
                fixtures.append(path)
            for name in self.documents:
                with self.subTest(document=name):
                    result = self.run_setup(name, home, source)
                    self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                    self.assertFalse((home / "clones.log").exists())
                    for path in fixtures:
                        self.assertEqual(path.read_text(), "existing fixture")

    def test_unrelated_existing_directory_is_not_overwritten(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            source = home / "brewlet-examples"
            source.mkdir()
            for name in self.documents:
                with self.subTest(document=name):
                    result = self.run_setup(name, home)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("BREWLET_SOURCE must point to Brewlet source", result.stderr)
                    self.assertFalse((home / "clones.log").exists())
                    self.assertEqual(list(source.iterdir()), [])

    def test_quickstart_output_does_not_replace_kubernetes_run_state(self):
        quickstart = next(block for block in self.commands["getting-started"]
                          if block.startswith("export BREWLET_QUICKSTART_WORK="))
        quickstart = quickstart.split("\nmvn ", 1)[0]
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            after = r"""
export BREWLET_WORK="$HOME/kubernetes-run"
export BREWLET_STORE="$BREWLET_WORK/oci"
export BREWLET_NAMESPACE="petclinic-existing-run"
export BREWLET_RUN="existing-run"
export BREWLET_INSTALLED_HERE=true
export BREWLET_POOL_LABEL_ADDED=true
""" + quickstart + r"""
test "$BREWLET_WORK" = "$HOME/kubernetes-run"
test "$BREWLET_STORE" = "$HOME/kubernetes-run/oci"
test "$BREWLET_NAMESPACE" = petclinic-existing-run
test "$BREWLET_RUN" = existing-run
test "$BREWLET_INSTALLED_HERE" = true
test "$BREWLET_POOL_LABEL_ADDED" = true
test "$BREWLET_QUICKSTART_STORE" != "$BREWLET_STORE"
test -d "$BREWLET_QUICKSTART_WORK"
"""
            result = self.run_setup("getting-started", home, after=after)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        commands = "\n".join(self.commands["getting-started"])
        self.assertNotRegex(commands, r"\bBREWLET_(WORK|STORE|NAMESPACE|RUN)=")

    def test_getting_started_shell_examples_are_syntactically_valid(self):
        for number, block in enumerate(self.commands["getting-started"], 1):
            with self.subTest(block=number):
                result = subprocess.run(
                    ["bash", "-n"], input=block, text=True,
                    capture_output=True, timeout=10,
                )
                self.assertEqual(result.returncode, 0, result.stderr)

    def test_optional_checks_are_collapsed_but_required_steps_remain_visible(self):
        optional = re.compile(
            r'^\?\?\? note "Optional: [^"]+"\n((?:[ \t]+[^\n]*\n|\n)*)',
            re.MULTILINE,
        )
        checks = {
            "getting-started": (
                "java -version", "brewlet inspect", "brewlet bundle",
                "curl -f http://127.0.0.1:8080/healthz",
            ),
        }
        required = {
            "getting-started": (
                "export BREWLET_SOURCE=", "mvn -f",
                "brewlet push", "brewlet run",
                "curl -f http://127.0.0.1:8080/hello",
            ),
        }
        for name, document in self.documents.items():
            with self.subTest(document=name):
                hidden = "\n".join(blocks("\n".join(optional.findall(document)), "bash"))
                visible = "\n".join(blocks(optional.sub("", document), "bash"))
                self.assertNotIn("???+", document, "Optional sections should start closed")
                for command in checks[name]:
                    self.assertIn(command, hidden)
                    self.assertNotIn(command, visible)
                for command in required[name]:
                    self.assertIn(command, visible)
        self.assertEqual(
            re.findall(r"^## (\d+)\.", self.documents["getting-started"], re.MULTILINE),
            ["1", "2", "3", "4"],
        )


class LocalKubernetesGuideTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.document = (ROOT / "docs/local-kubernetes.md").read_text(encoding="utf-8")
        cls.commands = blocks(cls.document, "sh")

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

    def test_copy_paste_does_not_reconfigure_or_exit_the_interactive_shell(self):
        self.assertGreater(len(self.commands), 15)
        for number, command in enumerate(self.commands):
            with self.subTest(block=number):
                for forbidden in ("set -", "export ", "source ", "exit ", "k()", "use-context"):
                    self.assertNotIn(forbidden, command)
                self.assertNotIn("try-brewlet.sh", command)
                self.assertNotRegex(command, r"(?m)^(?:bash|KUBECONFIG=.*)$")
                for shell in ("sh", "bash"):
                    result = subprocess.run([shell, "-n"], input=command, text=True, capture_output=True)
                    self.assertEqual(result.returncode, 0, result.stderr)

    def test_walkthrough_teaches_install_then_build_then_deploy(self):
        headings = re.findall(r"^## (\d+)\. (.+)$", self.document, re.MULTILINE)
        self.assertEqual(headings, [
            ("1", "Create a disposable cluster"), ("2", "Install Brewlet"),
            ("3", "Build PetClinic"), ("4", "Package and load the application"),
            ("5", "Deploy PetClinic"), ("6", "Open PetClinic"),
            ("7", "Clean up this cluster"),
        ])
        commands = "\n".join(self.commands)
        stages = ("kind create cluster", "helm install brewlet", "mvn -q -B",
                  '"$BREWLET_WORK/bin/brewlet" push', "images import --digests",
                  'apply -f "$BREWLET_WORK/petclinic.yaml"', "port-forward", "kind delete cluster")
        positions = [commands.index(stage) for stage in stages]
        self.assertEqual(positions, sorted(positions))
        self.assertIn("--version \"$BREWLET_VERSION\"", commands)
        self.assertIn("pullPolicy: Never", commands)
        self.assertIn('PETCLINIC_IMAGE="${PETCLINIC_OCI_REF%:*}@$PETCLINIC_DIGEST"', commands)
        self.assertIn("This does not delete the cluster", self.document)
        script = (ROOT / "site/try-brewlet.sh").read_text()
        revision = re.search(r"^PETCLINIC_REVISION=([0-9a-f]{40})$", commands, re.MULTILINE).group(1)
        self.assertIn("petclinic_revision=" + revision, script)

    def test_image_tag_is_not_interpreted_as_a_zsh_variable_modifier(self):
        package = next(command for command in self.commands if command.startswith("PETCLINIC_OCI_REF="))
        assignment = package.splitlines()[0]
        for shell in ("sh", "bash", "zsh"):
            if not shutil.which(shell):
                continue
            with self.subTest(shell=shell):
                result = subprocess.run(
                    [shell], input=assignment + '\nprintf "%s\\n" "$PETCLINIC_OCI_REF"\n',
                    env={**os.environ, "BREWLET_CLUSTER": "brewlet-lab-test"},
                    text=True, capture_output=True, timeout=10,
                )
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout.strip(), "localhost/brewlet/brewlet-lab-test:local")

    def test_petclinic_build_uses_local_maven_and_jdk_21(self):
        self.assertIn("**JDK 21** and **Maven 3.9 or newer**", self.document)
        self.assertIn("java -version", self.commands[0])
        self.assertIn("mvn -version", self.commands[0])
        build = next(command for command in self.commands if command.startswith("mvn -q -B"))
        self.assertIn('-f "$BREWLET_WORK/petclinic/pom.xml"', build)
        self.assertIn('cp "$BREWLET_WORK/petclinic/target/"*.jar "$BREWLET_WORK/petclinic.jar"', build)
        self.assertNotIn("docker", build)
        self.assertNotIn("MAVEN_CONFIG", build)
        self.assertNotIn("petclinic-build", self.document)
        self.assertNotIn("You do not need a host JDK or Maven", self.document)

    def test_private_context_is_explicit_and_automated_demo_is_optional(self):
        self.assertIn("disposable kind cluster", self.document)
        self.assertIn("Windows with WSL 2", self.document)
        self.assertIn("Your existing Kubernetes cluster is not used", self.document)
        self.assertIn("Press **Ctrl+C**", self.document)
        self.assertIn("private kubeconfig", self.document)
        for command in self.commands[1:]:
            for invocation in re.findall(r"\bkubectl[^\n]*", command):
                self.assertIn('kubectl --kubeconfig "$BREWLET_KUBECONFIG"', invocation)
        commands = "\n".join(self.commands)
        self.assertIn('--kube-context "kind-$BREWLET_CLUSTER"', commands)
        self.assertIn("env -u HELM_KUBEAPISERVER HELM_DRIVER=secret", commands)
        self.assertIn('DOCKER_CONTEXT="$BREWLET_DOCKER_CONTEXT" KIND_EXPERIMENTAL_PROVIDER=docker', commands)
        self.assertNotIn("docker system prune", commands)
        self.assertNotIn("helm uninstall", commands)
        self.assertGreater(self.document.index("optional\n[demo script]"),
                           self.document.index("## Where to go next"))
        self.assertTrue((ROOT / "site/try-brewlet.sh").is_file())
        workflow = (ROOT / ".github/workflows/site-pages.yml").read_text()
        self.assertIn("site/ site/_site/", workflow)
        self.assertNotIn("--exclude='try-brewlet.sh'", workflow)

    def test_failed_download_does_not_run_an_old_copy_or_exit_the_terminal(self):
        installer = next(command for command in self.commands
                         if command.startswith("curl -fL https://brewlet.sh/install.sh"))
        for shell in ("sh", "bash"):
            with self.subTest(shell=shell), tempfile.TemporaryDirectory() as directory:
                work = Path(directory)
                (work / "install.sh").write_text("touch incorrectly-executed\n")
                result = subprocess.run(
                    [shell], input="curl() { return 22; }\n" + installer + "\necho TERMINAL_OPEN\n",
                    cwd=work, env={**os.environ, "BREWLET_WORK": str(work)},
                    text=True, capture_output=True, timeout=10,
                )
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn("TERMINAL_OPEN", result.stdout)
                self.assertFalse((work / "incorrectly-executed").exists())

    def test_cleanup_refuses_missing_or_foreign_private_context(self):
        cleanup = next(command for command in self.commands if command.startswith('if [ -n "$BREWLET_CLUSTER" ]'))
        harness = r"""
kubectl() {
  [ "$1" = --kubeconfig ] && [ "$2" = "$BREWLET_KUBECONFIG" ] || return 99
  printf '%s\n' "$MOCK_CONTEXT"
}
env() { printf 'DELETE %s\n' "$*"; }
"""
        with tempfile.TemporaryDirectory() as directory:
            config = Path(directory) / "kubeconfig"
            for cluster, context, exists, allowed in (
                ("brewlet-lab-owned", "kind-brewlet-lab-owned", True, True),
                ("brewlet-lab-owned", "docker-desktop", True, False),
                ("brewlet-lab-owned", "kind-brewlet-lab-owned", False, False),
                ("", "kind-", True, False),
            ):
                with self.subTest(cluster=cluster, context=context, exists=exists):
                    if exists:
                        config.write_text("private context")
                    else:
                        config.unlink(missing_ok=True)
                    result = subprocess.run(
                        ["sh"], input=harness + cleanup, text=True, capture_output=True,
                        env={**os.environ, "BREWLET_CLUSTER": cluster,
                             "BREWLET_KUBECONFIG": str(config), "BREWLET_DOCKER_CONTEXT": "desktop-linux",
                             "MOCK_CONTEXT": context}, timeout=10,
                    )
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual("DELETE " in result.stdout, allowed)
                    if allowed:
                        self.assertIn("--name brewlet-lab-owned --kubeconfig " + str(config), result.stdout)
                    else:
                        self.assertIn("Stop: recover this run", result.stderr)

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
                    path = ROOT / "docs" / urlsplit(href).path.removeprefix("/docs/")
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
