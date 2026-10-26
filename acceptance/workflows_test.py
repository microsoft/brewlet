"""Execute the actual workflow guards offline; never execute live jobs."""

import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

from acceptance.native_contracts import SUITES, validate_output
from acceptance.run_check import CHECKS

ROOT = Path(__file__).resolve().parents[1]


def block(text, marker):
    tail = text.split(marker, 1)[1].split("        run: |\n", 1)[1]
    return "\n".join(line[10:] for line in re.match(r"(?:          [^\n]*\n)+", tail).group().splitlines()) + "\n"


def triggers(text):
    section = text.split("\non:\n", 1)[1].split("\npermissions:", 1)[0].split("\nconcurrency:", 1)[0]
    return [line for line in section.splitlines() if line.startswith("  ") and not line.startswith("   ")]


class WorkflowTests(unittest.TestCase):
    def test_routine_and_qualification_share_fail_closed_native_contract_runner(self):
        checks = (ROOT / ".github/workflows/checks.yml").read_text()
        website = (ROOT / ".github/workflows/site-pages.yml").read_text()
        callers = [(ROOT / ".github/workflows" / name).read_text() for name in ("pr.yml", "full-validation.yml")]
        self.assertIn("python3 acceptance/run_check.py native-contracts", checks)
        self.assertEqual(["python3", "acceptance/native_contracts.py"], CHECKS["native-contracts"]["command"])
        for name in CHECKS:
            self.assertEqual(1, (checks + website).count("python3 acceptance/run_check.py " + name), name)
        for text in [checks] + callers:
            self.assertNotIn("run_check.py all", text)
            self.assertNotIn("make acceptance", text)
            self.assertNotIn("continue-on-error", text)
            self.assertNotIn("pull_request_target", text)
            self.assertNotIn("paths:", text)
        for text in callers:
            self.assertNotIn("run_check.py", text)
            self.assertIn("uses: ./.github/workflows/checks.yml", text)
            self.assertIn("uses: ./.github/workflows/e2e.yml", text)
            self.assertIn("uses: ./.github/workflows/codeql.yml", text)
        self.assertNotIn("uses: ./.github/workflows/site-pages.yml", callers[1])
        self.assertEqual(["  workflow_call:"], triggers(checks))
        self.assertIn("  workflow_call:", website)
        self.assertNotIn("  pull_request:", website)
        self.assertNotIn("  push:", website)
        self.assertEqual({"artifact", "shim", "node", "provisioner", "worker"}, set(SUITES))
        validate_output("--- PASS: required test\nPASS\n")
        for output in ("--- SKIP: required test\n--- PASS: unrelated\nPASS\n", "", "PASS\n"):
            with self.assertRaises(ValueError):
                validate_output(output)

    def test_pr_workflow_runs_only_for_pull_requests_and_requires_exactly_the_impacted_checks(self):
        text = (ROOT / ".github/workflows/pr.yml").read_text()
        self.assertEqual(["  pull_request:"], triggers(text))
        self.assertIn("cancel-in-progress: true", text)
        guard = block(text, "Validate immutable pull request source")
        for event, source, success in (("pull_request", "a" * 40, True), ("push", "a" * 40, False),
                                       ("workflow_dispatch", "a" * 40, False), ("pull_request", "main", False),
                                       ("pull_request", "", False)):
            with tempfile.TemporaryDirectory() as temporary:
                environment = {"EVENT": event, "SOURCE": source, "REPOSITORY": "microsoft/brewlet",
                    "GITHUB_OUTPUT": str(Path(temporary) / "output")}
                result = subprocess.run(["bash", "-eu", "-c", guard], env=environment, capture_output=True)
                self.assertEqual(success, result.returncode == 0, (event, source))
        listing = text.split("List exact pull request changes", 1)[1].split("      - name:", 1)[0]
        self.assertIn('test "$head" = "$SOURCE"', listing)
        self.assertIn("previous_filename", listing)
        aggregate = text.split("\n  validation:", 1)[1]
        self.assertIn("name: Brewlet PR validation", aggregate)
        self.assertIn("if: always()", aggregate)
        self.assertIn("needs: [context, checks, e2e, codeql, website]", aggregate)
        program = block(aggregate, "Require every impacted check")
        base = {"CONTEXT": "success", "CHECKS_REQUIRED": "true", "CHECKS": "success",
                "E2E_REQUIRED": "true", "E2E": "success", "CODEQL_LANGUAGES": '["go"]', "CODEQL": "success",
                "WEBSITE_REQUIRED": "false", "WEBSITE": "skipped"}
        run = lambda environment: subprocess.run(["bash", "-eu", "-c", program], env=environment,
                                                 capture_output=True).returncode == 0
        self.assertTrue(run(base))
        self.assertTrue(run(dict(base, CODEQL_LANGUAGES="[]", CODEQL="skipped")))
        for required, result_key, flag in (("CHECKS_REQUIRED", "CHECKS", None), ("E2E_REQUIRED", "E2E", None),
                                           ("WEBSITE_REQUIRED", "WEBSITE", None), (None, "CODEQL", '["go"]')):
            for value in ("success", "failure", "skipped", "cancelled", ""):
                for selected in ("true", "false", ""):
                    environment = dict(base)
                    if required:
                        environment[required] = selected
                        expected = {"true": "success", "false": "skipped"}.get(selected)
                    else:
                        environment["CODEQL_LANGUAGES"] = {"true": flag, "false": "[]", "": ""}[selected]
                        expected = {"true": "success", "false": "skipped"}.get(selected)
                    environment[result_key] = value
                    self.assertEqual(expected == value, run(environment), (result_key, selected, value))
        for value in ("failure", "skipped", "cancelled", ""):
            self.assertFalse(run(dict(base, CONTEXT=value)), value)

    def classify(self, files, listed="true"):
        text = (ROOT / ".github/workflows/pr.yml").read_text()
        program = block(text, "Classify impacted checks")
        with tempfile.TemporaryDirectory() as temporary:
            listing = Path(temporary) / "files"
            listing.write_text("".join(name + "\n" for name in files))
            output = Path(temporary) / "output"
            environment = {"LISTED": listed, "FILES": str(listing), "GITHUB_OUTPUT": str(output),
                "GITHUB_STEP_SUMMARY": str(Path(temporary) / "summary"), "PATH": os.environ["PATH"]}
            subprocess.run(["bash", "-eu", "-c", program], env=environment, check=True, capture_output=True)
            outputs = dict(line.split("=", 1) for line in output.read_text().splitlines())
        outputs["codeql"] = json.loads(outputs["codeql"])
        return outputs

    def test_pr_classifier_selects_impacted_checks_and_fails_closed_to_everything(self):
        flags = ("go", "maven", "python", "legal", "producer", "overlay", "e2e", "website")
        every = ["actions", "csharp", "go", "java-kotlin", "javascript-typescript", "python"]
        def expect(files, selected, languages=(), listed="true"):
            outputs = self.classify(files, listed)
            for flag in flags:
                self.assertEqual("true" if flag in selected else "false", outputs[flag], (files, flag))
            checks = any(flag in selected for flag in ("go", "maven", "python", "legal", "producer", "overlay"))
            self.assertEqual("true" if checks else "false", outputs["checks"], files)
            self.assertEqual(sorted(languages), outputs["codeql"], files)
            self.assertNotIn("e2e_languages", outputs)
        runtime = {"go", "maven", "legal", "producer", "overlay", "e2e"}
        expect(["README.md", "kubernetes/README.md"], set())
        expect(["docs/index.md", "site/index.html"], {"website"})
        expect(["docs/README.md"], {"go", "python", "website"})
        expect(["specs/SPECIFICATION.md"], {"python", "website"})
        expect(["release-notes/v2.md"], {"python"})
        expect(["release.json"], {"python", "legal", "maven"})
        expect(["NOTICE.txt"], {"legal", "maven", "python", "website"})
        expect([".github/dependabot.yml"], {"python"})
        expect([".github/workflows/site-pages.yml"], {"go", "python", "website"}, ["actions"])
        expect([".github/workflows/release.yml"], {"go", "python"}, ["actions"])
        expect(["pkg/runtime/x.go"], runtime, ["go"])
        expect(["go.sum", "docs/index.md"], runtime | {"website"}, ["go"])
        expect(["maven-plugin/src/main/java/A.java"], {"maven", "legal", "producer"}, ["java-kotlin"])
        expect(["integration-tests/fixtures/python/app/main.py"], {"producer", "e2e"}, ["python"])
        expect(["integration-tests/fixtures/check.py"], {"producer", "e2e"}, ["python"])
        expect(["integration-tests/fixtures/runtimes/python.Dockerfile"], {"producer", "e2e"})
        # Java feeds the Maven producer check; the other examples run only in their daily workflows.
        expect(["integration-tests/fixtures/java/app/pom.xml"], {"producer"}, ["java-kotlin"])
        expect(["integration-tests/fixtures/dotnet/App/Program.cs"], set(), ["csharp"])
        expect(["integration-tests/fixtures/node/src/main.mjs"], set(), ["javascript-typescript"])
        expect(["integration-tests/fixtures/php/src/main.php"], set())
        expect(["integration-tests/live-v2/harness.py"], {"python", "e2e"}, ["python"])
        expect(["scripts/release.py"], {"go", "python", "legal", "maven"}, ["python"])
        everything = set(flags)
        for files in ([".github/workflows/pr.yml"], [".github/workflows/checks.yml"],
                      [".github/workflows/e2e.yml"], [".github/workflows/codeql.yml"], ["Makefile"],
                      ["acceptance/check.py"], ["docs/../go.mod.md"], ["/etc/passwd"], ["unknown/file"], []):
            expect(files, everything, every)
        for listed in ("false", ""):
            expect(["docs/index.md"], everything, every, listed=listed)
        expect(["unknown/file", "integration-tests/fixtures/java/app/pom.xml"], everything, every)

    def test_pull_requests_run_native_checks_on_amd64_only_and_full_validation_keeps_both(self):
        pr = (ROOT / ".github/workflows/pr.yml").read_text()
        full = (ROOT / ".github/workflows/full-validation.yml").read_text()
        checks = (ROOT / ".github/workflows/checks.yml").read_text()
        self.assertIn("architectures: '[\"amd64\"]'", pr.split("\n  checks:", 1)[1].split("\n  e2e:", 1)[0])
        self.assertIn("architectures: '[\"amd64\",\"arm64\"]'", full.split("\n  checks:", 1)[1])
        self.assertIn("default: '[\"amd64\",\"arm64\"]'", checks)
        for job in ("producer-integration", "linux-overlay-contract"):
            section = checks.split("\n  " + job + ":", 1)[1]
            self.assertIn("architecture: ${{ fromJSON(inputs.architectures) }}", section)
            self.assertIn("matrix.architecture == 'arm64' && 'ubuntu-24.04-arm' || 'ubuntu-24.04'", section)
        e2e = full.split("\n  e2e:", 1)[1].split("\n  codeql:", 1)[0]
        self.assertIn("architecture: [amd64, arm64]", e2e)
        self.assertIn("architecture: ${{ matrix.architecture }}", e2e)
        self.assertNotIn("languages:", e2e)
        self.assertIn("scenario: pr", e2e)
        self.assertIn("architecture: amd64", pr.split("\n  e2e:", 1)[1].split("\n  codeql:", 1)[0])

    def test_legal_checks_always_package_the_maven_plugin_they_inspect(self):
        checks = (ROOT / ".github/workflows/checks.yml").read_text()
        contracts = checks.split("\n  contracts:", 1)[1].split("\n  producer-integration:", 1)[0]
        maven = contracts.split("Execute Maven contracts", 1)[1].split("      - name:", 1)[0]
        self.assertIn("if: inputs.maven || inputs.legal", maven)
        self.assertLess(contracts.index("run_check.py maven"), contracts.index("make legal-check"))

    def test_go_jobs_restore_per_job_caches_that_full_validation_refreshes(self):
        text = (ROOT / ".github/workflows/checks.yml").read_text()
        contracts = text.split("\n  contracts:", 1)[1].split("\n  producer-integration:", 1)[0]
        producer = text.split("\n  producer-integration:", 1)[1].split("\n  linux-overlay-contract:", 1)[0]
        for name, job in (("contracts", contracts), ("producer", producer)):
            self.assertIn("cache: false", job)
            self.assertIn(f"key: go-{name}-${{{{ runner.os }}}}-${{{{ runner.arch }}}}-"
                          "${{ hashFiles('go.sum') }}-${{ github.sha }}", job)
            self.assertIn("actions/cache/restore@", job)
            save = job.split(f"      - name: Save {name} Go caches", 1)[1]
            self.assertIn("github.event_name != 'pull_request' || steps.go-cache.outputs.cache-matched-key == ''", save)
            self.assertIn("key: ${{ steps.go-cache.outputs.cache-primary-key }}", save)
            self.assertLess(job.index("Restore " + name), job.index("Save " + name))

    def test_full_validation_is_weekly_manual_or_release_called_and_requires_everything(self):
        text = (ROOT / ".github/workflows/full-validation.yml").read_text()
        self.assertEqual(["  schedule:", "  workflow_dispatch:", "  workflow_call:"], triggers(text))
        self.assertRegex(text, r'cron: "\d+ \d+ \* \* [0-6]"')
        self.assertNotIn("purpose", text)
        self.assertNotIn("archive-history", text)
        dispatch = text.split("  workflow_dispatch:", 1)[1].split("  workflow_call:", 1)[0]
        self.assertEqual("\n", dispatch)
        checks = text.split("\n  checks:", 1)[1].split("\n\n", 1)[0]
        for scope in ("go", "maven", "python", "legal", "producer", "overlay"):
            self.assertNotIn("      " + scope + ":", checks)
        codeql = text.split("\n  codeql:", 1)[1].split("\n\n", 1)[0]
        self.assertIn("security-events: write", codeql)
        self.assertNotIn("languages:", codeql)
        aggregate = text.split("\n  validation:", 1)[1]
        self.assertIn("needs: [context, checks, e2e, codeql]", aggregate)
        self.assertIn("if: always()", aggregate)
        program = block(aggregate, "Require every prerequisite")
        dependencies = ("CONTEXT", "CHECKS", "E2E", "CODEQL")
        for dependency in dependencies:
            for value in ("success", "failure", "skipped", "cancelled", ""):
                environment = dict.fromkeys(dependencies, "success")
                environment[dependency] = value
                result = subprocess.run(["bash", "-eu", "-c", program], env=environment, capture_output=True)
                self.assertEqual(value == "success", result.returncode == 0, (dependency, value))

    def test_full_validation_source_guard_is_executed_before_tests(self):
        text = (ROOT / ".github/workflows/full-validation.yml").read_text()
        program = block(text, "Validate immutable source")
        for event, ref, expected, success in (
                ("schedule", "refs/heads/main", "", True),
                ("schedule", "refs/heads/candidate", "", False),
                ("schedule", "refs/heads/main", "a" * 40, False),
                ("workflow_dispatch", "refs/heads/candidate", "", True),
                ("workflow_dispatch", "refs/heads/main", "", True),
                ("workflow_dispatch", "refs/heads/candidate", "a" * 40, False),
                ("workflow_dispatch", "refs/tags/v1.0.0", "", False),
                ("workflow_dispatch", "refs/tags/v1.0.0", "a" * 40, False),
                ("push", "refs/tags/v1.0.0", "a" * 40, True),
                ("push", "refs/tags/v1.0.0", "b" * 40, False),
                ("push", "refs/tags/v1.0.0", "", False),
                ("push", "refs/heads/main", "a" * 40, False),
                ("pull_request", "refs/pull/1/merge", "", False),
                ("pull_request", "refs/pull/1/merge", "a" * 40, False)):
            with tempfile.TemporaryDirectory() as temporary:
                environment = {"EVENT": event, "EXPECTED": expected,
                    "SOURCE": "a" * 40, "REPOSITORY": "microsoft/brewlet", "REF": ref,
                    "GITHUB_OUTPUT": str(Path(temporary) / "output")}
                result = subprocess.run(["bash", "-eu", "-c", program], env=environment, capture_output=True)
                self.assertEqual(success, result.returncode == 0, (event, ref, expected))

    def test_promotion_is_manual_reusable_and_fetches_verified_artifacts_before_strict_gate(self):
        text = (ROOT / ".github/workflows/promotion-qualification.yml").read_text()
        triggers = text.split("\non:\n", 1)[1].split("\npermissions:", 1)[0]
        self.assertEqual(["  workflow_dispatch:", "  workflow_call:"],
                         [line for line in triggers.splitlines() if line.startswith("  ") and not line.startswith("    ")])
        self.assertIn("actions: read", text)
        self.assertIn("fetch-depth: 0", text)
        self.assertIn("persist-credentials: false", text)
        self.assertIn("python3 acceptance/evidence_index.py fetch", text)
        self.assertIn("make acceptance", text)
        self.assertLess(text.index("evidence_index.py fetch"), text.index("make acceptance"))
        self.assertLess(text.index('test "$SOURCE" = "$EXPECTED_SOURCE"'), text.index("- uses: actions/checkout"))
        for bypass in ("continue-on-error", "||", "--structure-only", "qualify.py --run", "workflow run"):
            self.assertNotIn(bypass, text)

    def test_e2e_is_reusable_real_execution_only_and_pr_guard_rejects_forks(self):
        text = (ROOT / ".github/workflows/e2e.yml").read_text()
        self.assertIn("  workflow_call:", text)
        self.assertNotIn("  pull_request:", text)
        self.assertNotIn("pull_request_target", text)
        self.assertNotIn("secrets.", text)
        self.assertNotIn("QUALIFICATION_MODE", text)
        self.assertNotIn("run_check.py", text)
        self.assertNotIn("make acceptance", text)
        self.assertNotIn("unittest discover", text)
        self.assertIn("harness.py --scenario pr \\\n", text)
        self.assertIn('--verify-pr "acceptance-export/pr-e2e-${ARCHITECTURE}.json"', text)
        # Python is the only qualification fixture; example toolchains belong to language-e2e.yml.
        for removed in ("--pr-language", "--languages", "LANGUAGES", "inputs.languages",
                        "setup-java", "setup-node", "setup-dotnet"):
            self.assertNotIn(removed, text)
        export = text.split("      - name: Export only public PR outcomes", 1)[1].split(
            "      - name:", 1)[0]
        self.assertIn("steps.guard.outcome == 'success'", export)
        self.assertNotIn("${{ inputs.", export.split("        run: |", 1)[1])
        self.assertIn("persist-credentials: false", text)
        for name in ("Restore PR E2E host Go caches", "Restore PR E2E image Go build cache",
                     "Seed BuildKit Go build cache mount", "Extract BuildKit Go build cache mount"):
            step = text.split("      - name: " + name, 1)[1].split("      - ", 1)[0]
            self.assertIn("inputs.scenario == 'pr'", step, name)
        self.assertIn("cache: ${{ inputs.scenario != 'pr' }}", text)
        self.assertLess(text.index("Seed BuildKit"), text.index("harness.py --scenario pr"))
        self.assertLess(text.index("harness.py --scenario pr"), text.index("Extract BuildKit"))
        program = block(text, "Refuse unsupported execution contexts")
        for event, ref, head, scenario, success in (
                ("pull_request", "refs/pull/1/merge", "microsoft/brewlet", "pr", True),
                ("pull_request", "refs/pull/1/merge", "fork/brewlet", "pr", False),
                ("pull_request", "refs/pull/1/merge", "microsoft/brewlet", "complete", False),
                ("schedule", "refs/heads/main", "microsoft/brewlet", "pr", True),
                ("schedule", "refs/heads/candidate", "microsoft/brewlet", "pr", False),
                ("schedule", "refs/heads/main", "microsoft/brewlet", "complete", False),
                ("push", "refs/tags/v1.0.0", "microsoft/brewlet", "pr", True),
                ("push", "refs/tags/v1.0.0", "microsoft/brewlet", "complete", False),
                ("push", "refs/heads/main", "microsoft/brewlet", "pr", False),
                ("push", "refs/tags/untrusted", "microsoft/brewlet", "pr", False),
                ("workflow_dispatch", "refs/heads/candidate", "microsoft/brewlet", "complete", True),
                ("workflow_dispatch", "refs/heads/candidate", "microsoft/brewlet", "pr", True),
                ("workflow_dispatch", "refs/tags/untrusted", "microsoft/brewlet", "pr", False)):
            environment = {"EVENT": event, "REF": ref, "HEAD_REPOSITORY": head, "SCENARIO": scenario,
                "REPOSITORY": "microsoft/brewlet", "SOURCE": "a" * 40, "EXPECTED_SOURCE": "a" * 40,
                "ARCHITECTURE": "amd64"}
            result = subprocess.run(["bash", "-eu", "-c", program], env=environment, capture_output=True)
            self.assertEqual(success, result.returncode == 0, (event, ref, head, scenario))
        self.assertLess(text.index('test "$SOURCE" = "$EXPECTED_SOURCE"'), text.index("- uses: actions/checkout"))

    def test_dispatch_moved_head_bad_architecture_and_unknown_scope_fail_closed(self):
        text = (ROOT / ".github/workflows/e2e.yml").read_text()
        program = block(text, "Refuse unsupported execution contexts")
        for field, values in {
                "EXPECTED_SOURCE": ("b" * 40, "", "main", "A" * 40, "a" * 39),
                "ARCHITECTURE": ("emulated", "", "AMD64"),
                "SCENARIO": ("publish", "verify-only", "non-live", "")}.items():
            for value in values:
                environment = {"EVENT": "workflow_dispatch", "REF": "refs/heads/candidate",
                    "HEAD_REPOSITORY": "microsoft/brewlet", "REPOSITORY": "microsoft/brewlet",
                    "SOURCE": "a" * 40, "EXPECTED_SOURCE": "a" * 40, "SCENARIO": "pr",
                    "ARCHITECTURE": "amd64"}
                environment[field] = value
                result = subprocess.run(["bash", "-eu", "-c", program], env=environment, capture_output=True)
                self.assertNotEqual(0, result.returncode, (field, value))


    def test_example_languages_run_daily_in_their_own_non_gating_workflows(self):
        reusable = (ROOT / ".github/workflows/language-e2e.yml").read_text()
        self.assertEqual(["  workflow_call:"], triggers(reusable))
        self.assertIn("permissions:\n  contents: read", reusable)
        self.assertIn("persist-credentials: false", reusable)
        self.assertNotIn("secrets.", reusable)
        self.assertNotIn("continue-on-error", reusable)
        self.assertIn('--without-pinned-scenarios --local-language "$LANGUAGE"', reusable)
        self.assertIn('if test "$LANGUAGE" = java; then scenario=maven; fi', reusable)
        crons = set()
        for language in ("java", "node", "dotnet", "php"):
            text = (ROOT / ".github/workflows" / f"language-{language}.yml").read_text()
            self.assertEqual(["  schedule:", "  workflow_dispatch:"], triggers(text), language)
            cron = re.search(r'cron: "(\d+ \d+) \* \* \*"', text)
            self.assertIsNotNone(cron, language)
            crons.add(cron.group(1))
            self.assertIn("uses: ./.github/workflows/language-e2e.yml", text)
            self.assertIn("language: " + language + "\n", text)
            self.assertIn("permissions:\n  contents: read", text)
        self.assertEqual(4, len(crons), "daily language runs are staggered")
        # Gating PR, full and release validation never run the example languages.
        for name in ("pr.yml", "full-validation.yml", "release.yml", "e2e.yml", "checks.yml"):
            self.assertNotIn("language-e2e.yml", (ROOT / ".github/workflows" / name).read_text(), name)
        program = block(reusable, "Refuse unsupported execution contexts")
        for event, ref, language, architecture, success in (
                ("schedule", "refs/heads/main", "php", "amd64", True),
                ("schedule", "refs/heads/main", "java", "arm64", True),
                ("schedule", "refs/heads/candidate", "node", "amd64", False),
                ("workflow_dispatch", "refs/heads/candidate", "dotnet", "amd64", True),
                ("workflow_dispatch", "refs/tags/v1.0.0", "dotnet", "amd64", False),
                ("pull_request", "refs/pull/1/merge", "php", "amd64", False),
                ("push", "refs/heads/main", "php", "amd64", False),
                ("schedule", "refs/heads/main", "python", "amd64", False),
                ("schedule", "refs/heads/main", "ruby", "amd64", False),
                ("schedule", "refs/heads/main", "php", "emulated", False)):
            environment = {"EVENT": event, "REF": ref, "REPOSITORY": "microsoft/brewlet",
                           "LANGUAGE": language, "ARCHITECTURE": architecture}
            result = subprocess.run(["bash", "-eu", "-c", program], env=environment, capture_output=True)
            self.assertEqual(success, result.returncode == 0, (event, ref, language, architecture))
        environment = {"EVENT": "schedule", "REF": "refs/heads/main", "REPOSITORY": "fork/brewlet",
                       "LANGUAGE": "php", "ARCHITECTURE": "amd64"}
        self.assertNotEqual(0, subprocess.run(["bash", "-eu", "-c", program], env=environment,
                                              capture_output=True).returncode)


if __name__ == "__main__":
    unittest.main()
