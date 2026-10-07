# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

from pathlib import Path
import re
import unittest

from ci_plan import JOBS


ROOT = Path(__file__).resolve().parents[1]
WORKFLOW = (ROOT / ".github/workflows/ci.yml").read_text()


def job(name, workflow=WORKFLOW):
    match = re.search(rf"^  {re.escape(name)}:\n(.*?)(?=^  [\w-]+:\n|\Z)",
                      workflow.split("\njobs:\n", 1)[1], re.MULTILINE | re.DOTALL)
    if not match:
        raise AssertionError(f"missing workflow job: {name}")
    return match[1]


class WorkflowTests(unittest.TestCase):
    def test_codeql_reuses_selection_and_skips_empty_matrix(self):
        workflow = (ROOT / ".github/workflows/codeql.yml").read_text()
        source = job("changes", workflow)
        self.assertIn("fetch-depth: 0", source)
        self.assertIn("PR_BASE_SHA: ${{ github.event.pull_request.base.sha }}", source)
        self.assertIn("PR_HEAD_SHA: ${{ github.event.pull_request.head.sha }}", source)
        self.assertIn("run: python3 scripts/ci_plan.py plan", source)
        for output in ("codeql", "codeql_matrix"):
            self.assertIn(f"      {output}: ${{{{ steps.plan.outputs.{output} }}}}", source)
        analyze = job("analyze", workflow)
        self.assertIn("    needs: changes", analyze)
        self.assertIn("    if: needs.changes.outputs.codeql == 'true'", analyze)
        self.assertIn("matrix: ${{ fromJSON(needs.changes.outputs.codeql_matrix) }}", analyze)
        self.assertIn("languages: ${{ matrix.language }}", analyze)
        self.assertIn("build-mode: ${{ matrix.build-mode }}", analyze)
        self.assertIn("if: matrix.build-mode == 'autobuild'", analyze)
        triggers = workflow.split("\njobs:\n")[0]
        self.assertNotIn("paths:", triggers)
        self.assertNotIn("paths-ignore:", triggers)
        for event in ("push", "pull_request", "schedule", "workflow_dispatch"):
            self.assertIn(f"  {event}:", triggers)

    def test_gate_covers_every_job_and_runs_after_skips(self):
        names = set(re.findall(r"^  ([\w-]+):$", WORKFLOW.split("\njobs:\n", 1)[1], re.MULTILINE))
        self.assertEqual(names, {"changes", "safeguards", "pr-checks", *JOBS})
        gate = job("pr-checks")
        dependencies = re.search(r"^    needs: \[(.+)\]$", gate, re.MULTILINE)[1]
        self.assertEqual(set(dependencies.split(", ")), names - {"pr-checks"})
        self.assertIn("    if: always()", gate)
        self.assertIn("name: PR checks", gate)
        self.assertIn("CI_NEEDS: ${{ toJSON(needs) }}", gate)
        self.assertIn("python3 scripts/ci_plan.py gate", gate)

    def test_every_conditional_job_uses_its_selection_output(self):
        for name in JOBS:
            with self.subTest(job=name):
                self.assertIn("    needs: changes", job(name))
                self.assertIn(f"    if: needs.changes.outputs.{name} == 'true'", job(name))
                self.assertIn(f"      {name}: ${{{{ steps.plan.outputs.{name} }}}}", job("changes"))
        self.assertNotIn("    if:", job("safeguards"))
        self.assertNotIn("paths:", WORKFLOW.split("\njobs:\n")[0])
        self.assertNotIn("paths-ignore:", WORKFLOW.split("\njobs:\n")[0])

    def test_diff_has_history_and_shas_not_interpolated_shell(self):
        source = job("changes")
        self.assertIn("fetch-depth: 0", source)
        self.assertIn("PR_BASE_SHA: ${{ github.event.pull_request.base.sha }}", source)
        self.assertIn("PR_HEAD_SHA: ${{ github.event.pull_request.head.sha }}", source)
        self.assertIn("run: python3 scripts/ci_plan.py plan", source)

    def test_required_tiers_have_prerequisites_and_strict_results(self):
        for name, matrix in (("host", "host_matrix"),):
            source = job(name)
            with self.subTest(job=name):
                self.assertIn('E2E_REQUIRE_ALL: "true"', source)
                self.assertIn(f"fromJSON(needs.changes.outputs.{matrix})", source)
                self.assertIn('java-version: "21"', source)
                self.assertIn("actions/setup-go@", source)
                self.assertIn("set -o pipefail", source)
                self.assertIn('run.sh --tier "$TIER"', source)
                self.assertIn("if-no-files-found: error", source)
        self.assertNotIn("--tier 1 ", WORKFLOW)

    def test_lifecycle_tests_run_once_with_required_tools(self):
        source = job("kubernetes")
        self.assertIn('BREWLET_REQUIRE_CLI_INTEGRATION: "true"', source)
        self.assertIn('BREWLET_REQUIRE_HELM: "true"', source)
        self.assertIn("KUBEBUILDER_ASSETS=", source)
        self.assertIn("run: make test", source)
        self.assertIn("run: make helm-render-check", job("helm"))
        self.assertNotIn("make helm-check", job("helm"))
        self.assertIn("helm-check: helm-render-check helm-lifecycle-check",
                      (ROOT / "kubernetes/Makefile").read_text())

    def test_container_inspection_and_both_entrypoints_are_retained(self):
        source = job("containers")
        self.assertIn("needs.changes.outputs.image_integrity == 'true'", source)
        self.assertIn("dockerfile_test.sh", source)
        self.assertIn("--platform linux/amd64 --load", source)
        self.assertIn("-x /opt/brewlet-dist/brewlet-source-policy", source)
        self.assertIn("make container-image-test", source)
        makefile = (ROOT / "Makefile").read_text()
        for cmd in ("manager", "admission"):
            self.assertIn(f"--build-arg CMD={cmd} --output=type=cacheonly", makefile)

    def test_maven_runs_once_on_jdk17_baseline(self):
        source = job("maven")
        self.assertIn('java-version: "17"', source)
        self.assertNotIn("matrix", source)
        self.assertIn("run: mvn -B --no-transfer-progress -Dmaven.compiler.release=17 verify", source)
        self.assertIn("Verify packaged notices", source)
        self.assertIn("if: needs.changes.outputs.maven_release == 'true'", source)
        self.assertIn('"$MONOREPO_DIR/maven-plugin/pom.xml" -DskipTests install',
                      (ROOT / "integration-tests/e2e/tier2-cli.sh").read_text())

    def test_live_smoke_is_checkout_built_and_bounded(self):
        source = job("live")
        self.assertIn("go install sigs.k8s.io/kind@v0.33.0", source)
        self.assertIn("timeout-minutes: 40", source)
        self.assertEqual(source.count("run: python3 integration-tests/e2e/live/smoke.py"), 1)
        self.assertNotIn("matrix:", source)
        self.assertIn("if: always()", source)
        self.assertIn("retention-days: 7", source)

    def test_exhaustive_scenarios_belong_only_to_e2e(self):
        self.assertNotIn("\n  tiers:", WORKFLOW)
        self.assertNotIn("tier_matrix", WORKFLOW)
        self.assertNotIn("live_matrix", WORKFLOW)
        self.assertNotIn("workflow_call:", WORKFLOW)
        for scenario in ("hpa.py", "admission.py", "workflows.py"):
            self.assertNotIn(scenario, WORKFLOW)
        nightly = (ROOT / ".github/workflows/e2e.yml").read_text()
        for tier in (8, 15, 17, 18, 19):
            self.assertIn(f"tiers: --tier {tier}\n", nightly)
        self.assertIn('python3 "integration-tests/e2e/live/${SCENARIO}.py"', nightly)
        self.assertIn("python3 integration-tests/e2e/live/workflows.py", nightly)
        self.assertIn('cron: "30 6 * * *"', nightly)


if __name__ == "__main__":
    unittest.main()
