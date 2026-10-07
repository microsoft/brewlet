# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Shared checkout-built runtime for the isolated live scenarios."""

import json
import re
import shutil
import xml.etree.ElementTree as ET

from common import (Fixture, JDK_IMAGE, MAVEN_NETWORK_RETRIES, OWNER_LABEL, ROOT,
                    retry_transient, sha256)


class CheckoutFixture(Fixture):
    def __init__(self, scenario, *, workers=0):
        super().__init__(scenario, workers=workers)
        for key in ("BREWLET_REGISTRY_USERNAME", "BREWLET_REGISTRY_PASSWORD", "MAVEN_OPTS",
                    "MAVEN_ARGS", "JAVA_TOOL_OPTIONS"):
            self.env.pop(key, None)
        self.source = ROOT
        self.image_ids = []
        self.built = {}
        self.maven_base = ["mvn", "-B", "--no-transfer-progress", *MAVEN_NETWORK_RETRIES,
                           f"-Dmaven.repo.local={self.private / 'm2'}"]
        self.cleanups.append(self.remove_checkout_images)

    def start(self):
        if not shutil.which("go"):
            raise RuntimeError("Required prerequisite missing: go; no assertions skipped")
        super().start()

    def build_command(self, name, argv, **kwargs):
        """Idempotent checkout build; transient network failures are retried."""
        kwargs["env"] = dict(self.env, **kwargs.get("env", {}))
        def attempt():
            result = self.run(argv, check=False, **kwargs)
            self.save(name + ".log", result.stdout + result.stderr)
            return result
        result = retry_transient(name, attempt)
        if result.returncode:
            raise RuntimeError(f"{name} failed ({result.returncode}); see {self.work}")
        return result

    def build(self):
        self.source_revision = self.run(["git", "rev-parse", "HEAD"], cwd=ROOT).stdout.strip()
        self.cli.parent.mkdir()
        self.build_command("build-cli", ["go", "build", "-trimpath", "-o", self.cli, "./cmd/brewlet"],
                           cwd=ROOT / "core", env={"CGO_ENABLED": "0"}, timeout=600)
        plugin = self.private / "maven-plugin"
        shutil.copytree(ROOT / "maven-plugin", plugin, ignore=shutil.ignore_patterns("target"))
        self.build_command("build-plugin", [*self.maven_base, "--settings", self.private / "settings.xml",
                                            "-f", plugin / "pom.xml", "-DskipTests", "install"], timeout=900)
        self.plugin_version = ET.parse(plugin / "pom.xml").getroot().find(
            "{http://maven.apache.org/POM/4.0.0}version").text
        self.plugin = f"sh.brewlet:brewlet-maven-plugin:{self.plugin_version}"
        installed = (self.private / "m2/sh/brewlet/brewlet-maven-plugin" / self.plugin_version /
                     f"brewlet-maven-plugin-{self.plugin_version}.jar")
        built = plugin / "target" / f"brewlet-maven-plugin-{self.plugin_version}.jar"
        if sha256(installed) != sha256(built):
            raise RuntimeError("Private Maven repository does not hold the checkout-built plugin")
        for component, dockerfile, args in (
            ("operator", "kubernetes/Dockerfile", ["--build-arg", "CMD=manager"]),
            ("admission", "kubernetes/Dockerfile", ["--build-arg", "CMD=admission"]),
            ("provisioner", "provisioner/Dockerfile", []),
        ):
            tag = f"brewlet.local/{self.name}-{component}:candidate"
            try:
                self.build_command(f"build-{component}-image",
                                   ["docker", "build", "--platform", f"linux/{self.arch}",
                                    "--label", f"{OWNER_LABEL}={self.name}", "-t", tag, *args,
                                    "-f", ROOT / dockerfile, ROOT], timeout=1800)
            finally:
                result = self.run(["docker", "image", "inspect", tag], check=False)
                if result.returncode == 0:
                    self.image_ids.append(json.loads(result.stdout)[0]["Id"])
            self.built[component] = tag
        self.chart = ROOT / "kubernetes/charts/brewlet"
        tools = {}
        for tool, argv in (("go", ["go", "version"]), ("java", ["java", "-version"]),
                           ("maven", ["mvn", "-v"]), ("docker", ["docker", "version", "--format", "{{.Server.Version}}"]),
                           ("kind", ["kind", "version"]), ("kubectl", ["kubectl", "version", "--client"]),
                           ("helm", ["helm", "version", "--short"])):
            result = self.run(argv)
            tools[tool] = (result.stdout + result.stderr).strip()
        self.save("versions.json", {
            "runtime": "checkout", "source": self.source_revision,
            "sourceDirty": bool(self.run(["git", "status", "--porcelain"], cwd=ROOT).stdout),
            "cliSHA256": sha256(self.cli), "plugin": self.plugin, "pluginSHA256": sha256(built),
            "images": {c: {"tag": t} for c, t in self.built.items()}, "dockerImageIDs": self.image_ids,
            "chart": str(self.chart.relative_to(ROOT)),
            "chartHashes": {str(p.relative_to(self.chart)): sha256(p)
                            for p in sorted(self.chart.rglob("*")) if p.is_file()},
            "hostArchitecture": self.arch, "jdkImage": JDK_IMAGE, "tools": tools})

    def component_images(self):
        images = {}
        for component, tag in self.built.items():
            self.load_image(tag)
            rows = self.run(["docker", "exec", self.node_id, "ctr", "-n", "k8s.io",
                             "images", "ls"]).stdout.splitlines()
            digest = next((r.split()[2] for r in rows if r.split() and r.split()[0] == tag), "")
            if not re.fullmatch(r"sha256:[a-f0-9]{64}", digest):
                raise RuntimeError(f"Could not pin the imported {component} image")
            pinned = tag.split(":")[0] + "@" + digest
            self.run(["docker", "exec", self.node_id, "ctr", "-n", "k8s.io", "images", "tag",
                      "--force", tag, pinned])
            for node, identifier in self.worker_ids.items():
                self.own_container(node)
                self.run(["docker", "exec", identifier, "ctr", "-n", "k8s.io", "images", "tag",
                          "--force", tag, pinned])
            images[component] = pinned
        versions = json.loads((self.work / "versions.json").read_text())
        versions["components"] = images
        self.save("versions.json", versions)
        self.record("checkout-components-loaded", images)
        return images

    def remove_checkout_images(self):
        errors = []
        for image_id in self.image_ids:
            result = self.run(["docker", "image", "inspect", image_id], check=False)
            if result.returncode:
                errors.append(f"could not inspect checkout image {image_id}: {result.stderr}")
                continue
            if json.loads(result.stdout)[0]["Config"].get("Labels", {}).get(OWNER_LABEL) != self.name:
                errors.append(f"refusing to remove foreign image {image_id}")
                continue
            self.run(["docker", "image", "rm", "-f", image_id])
        if errors:
            raise RuntimeError("; ".join(errors))
