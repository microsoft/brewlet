"""Offline contracts for scanning, dependency updates and Pages publication."""

from pathlib import Path
import re
import unittest

ROOT = Path(__file__).resolve().parents[1]
WORKFLOWS = ROOT / ".github/workflows"


class InfrastructureWorkflowTests(unittest.TestCase):
    def test_codeql_covers_repository_languages_without_path_filters(self):
        text = (WORKFLOWS / "codeql.yml").read_text()
        self.assertIn("branches: [main]", text)
        for trigger in ("push", "pull_request", "schedule", "workflow_dispatch"):
            self.assertIn("  " + trigger + ":", text)
        self.assertNotIn("paths:", text)
        self.assertNotIn("pull_request_target", text)
        self.assertEqual(
            {"actions", "csharp", "go", "java-kotlin", "javascript-typescript", "python"},
            set(re.findall(r"- language: ([\w-]+)", text)))
        self.assertIn("language: go\n            build-mode: autobuild", text)
        self.assertIn("go-version-file: go.mod", text)
        self.assertIn("global-json-file: integration-tests/fixtures/dotnet/global.json", text)
        self.assertIn("if: matrix.build-mode == 'autobuild'", text)
        self.assertIn("languages: ${{ matrix.language }}", text)
        self.assertIn("build-mode: ${{ matrix.build-mode }}", text)
        self.assertIn("category: /language:${{ matrix.language }}", text)
        self.assertIn("      security-events: write", text)
        self.assertIn("persist-credentials: false", text)

    def test_dependabot_uses_current_manifest_locations(self):
        text = (ROOT / ".github/dependabot.yml").read_text()
        self.assertTrue(text.startswith("version: 2\n"))
        expected = {
            "github-actions": {"/": ".github/workflows"},
            "gomod": {"/": "go.mod"},
            "maven": {"/maven-plugin": "maven-plugin/pom.xml",
                      **{"/integration-tests/fixtures/java/" + name:
                         "integration-tests/fixtures/java/" + name + "/pom.xml"
                         for name in ("app", "bom", "dependency")}},
            "npm": {"/site": "site/package-lock.json"},
            "pip": {"/site": "site/requirements.txt"},
            "nuget": {"/integration-tests/fixtures/dotnet": "integration-tests/fixtures/dotnet/global.json",
                      **{"/integration-tests/fixtures/dotnet/" + name:
                         "integration-tests/fixtures/dotnet/" + name + "/" + name + ".csproj"
                         for name in ("App", "Greeting")}},
        }
        blocks = text.split("  - package-ecosystem: ")[1:]
        self.assertEqual(set(expected), {section.splitlines()[0] for section in blocks})
        for section in blocks:
            ecosystem = section.splitlines()[0]
            self.assertIn("interval: weekly", section)
            directories = re.findall(r"^    directory: (/\S*)$|^      - (/\S*)$",
                                     section, re.MULTILINE)
            self.assertEqual(set(expected[ecosystem]), {single or multiple for single, multiple in directories})
            for manifest in expected[ecosystem].values():
                self.assertTrue((ROOT / manifest).exists(), manifest)

    def test_pages_deploys_only_a_successful_checked_main_build(self):
        text = (WORKFLOWS / "site-deploy.yml").read_text()
        preview = (WORKFLOWS / "site-pages.yml").read_text()
        triggers = text.split("\non:\n", 1)[1].split("\npermissions:", 1)[0]
        self.assertIn("branches: [main]", triggers)
        self.assertIn("  workflow_dispatch:", triggers)
        self.assertNotIn("pull_request", triggers)
        build, deploy = text.split("\n  deploy:\n", 1)
        self.assertIn("if: github.repository == 'microsoft/brewlet' && github.ref == 'refs/heads/main'", build)
        self.assertIn("uses: ./.github/workflows/site-pages.yml", build)
        self.assertIn("expected_sha: ${{ github.sha }}", build)
        self.assertNotIn(": write", build)
        self.assertIn("    needs: build\n", deploy)
        self.assertNotIn("always()", deploy)
        self.assertIn("      name: github-pages", deploy)
        self.assertIn("      pages: write", deploy)
        self.assertIn("      id-token: write", deploy)
        artifact = re.search(r"name: (brewlet-website-preview)", preview).group(1)
        self.assertIn("name: " + artifact, deploy)
        self.assertIn("path: site/_site", deploy)
        self.assertNotIn("actions/checkout", deploy)
        for action in ("download-artifact", "configure-pages", "upload-pages-artifact", "deploy-pages"):
            self.assertIn("actions/" + action + "@", deploy)
        for unsafe in ("deploy-pages", "pages: write", "id-token: write"):
            self.assertNotIn(unsafe, preview)
        self.assertIn("group: site-preview-${{ github.workflow }}-${{ github.ref }}", preview)
        self.assertIn("group: pages\n  cancel-in-progress: false", text)

    def test_new_workflows_pin_actions_and_do_not_expose_secrets(self):
        for name in ("codeql.yml", "site-deploy.yml"):
            text = (WORKFLOWS / name).read_text()
            self.assertIn("permissions:\n  contents: read", text)
            self.assertNotIn("secrets.", text)
            for action in re.findall(r"uses: (\S+)", text):
                if not action.startswith("./"):
                    self.assertRegex(action, r"^[\w/-]+@[0-9a-f]{40}$")


if __name__ == "__main__":
    unittest.main()
