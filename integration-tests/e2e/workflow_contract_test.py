# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import ast
import itertools
import json
from pathlib import Path
import re
import unittest


ROOT = Path(__file__).resolve().parents[2]
WORKFLOW = (ROOT / ".github/workflows/e2e.yml").read_text()


def block(source, key, indent):
    """Read a named indentation block in this workflow, not arbitrary YAML."""
    lines = source.splitlines()
    starts = [i for i, line in enumerate(lines) if line == " " * indent + key + ":"]
    if len(starts) != 1:
        raise AssertionError(f"expected one {key} block at indent {indent}")
    result = []
    for line in lines[starts[0] + 1:]:
        if line.strip() and not line.lstrip().startswith("#") and len(line) - len(line.lstrip()) <= indent:
            break
        result.append(line)
    return "\n".join(result)


def scalar(source, key, indent):
    matches = re.findall(rf"^{' ' * indent}{re.escape(key)}: (.+)$", source, re.MULTILINE)
    if len(matches) != 1:
        raise AssertionError(f"expected one {key} scalar at indent {indent}")
    return matches[0]


def expression(source, values):
    """Evaluate only the comparisons and boolean operators used by E2E routing."""
    tree = ast.parse(source.replace("&&", " and ").replace("||", " or "), mode="eval")

    def visit(node):
        if isinstance(node, ast.Constant) and isinstance(node.value, str):
            return node.value
        if isinstance(node, ast.Attribute) and isinstance(node.value, ast.Name):
            return values[f"{node.value.id}.{node.attr}"]
        if isinstance(node, ast.Compare) and len(node.ops) == 1:
            left, right = visit(node.left), visit(node.comparators[0])
            if isinstance(node.ops[0], ast.Eq):
                return left == right
            if isinstance(node.ops[0], ast.NotEq):
                return left != right
        if isinstance(node, ast.BoolOp):
            result = visit(node.values[0])
            for value in node.values[1:]:
                if isinstance(node.op, ast.And):
                    result = visit(value) if result else result
                elif isinstance(node.op, ast.Or):
                    result = result if result else visit(value)
                else:
                    raise AssertionError(f"unsupported operator: {ast.dump(node.op)}")
            return result
        raise AssertionError(f"unsupported workflow expression: {ast.dump(node)}")

    return visit(tree.body)


class WorkflowContractTests(unittest.TestCase):
    def setUp(self):
        self.jobs = block(WORKFLOW, "jobs", 0)
        self.inputs = block(block(WORKFLOW, "on", 0), "workflow_dispatch", 2)
        self.job_blocks = {name: block(self.jobs, name, 2) for name in ("tiers", "arm64", "live", "workflows")}

    def choices(self, name):
        value = scalar(block(self.inputs, name, 6), "options", 8)
        self.assertRegex(value, r"^\[[a-z, ]+\]$")
        return [part.strip() for part in value[1:-1].split(",")]

    def route(self, event, suite, scenario):
        values = {"github.event_name": event, "inputs.suite": suite, "inputs.scenario": scenario}
        return {name for name, source in self.job_blocks.items()
                if expression(scalar(source, "if", 4), values)}

    def test_selectors_defaults_and_scheduled_manual_only_triggers(self):
        self.assertEqual(self.choices("suite"), ["all", "live", "tiers"])
        self.assertEqual(self.choices("scenario"), ["both", "admission", "hpa", "workflows"])
        self.assertEqual(self.choices("candidate"), ["checkout", "shim", "release"])
        for name, default in (("suite", "all"), ("scenario", "both"), ("candidate", "checkout")):
            self.assertEqual(scalar(block(self.inputs, name, 6), "default", 8), default)
        triggers = block(WORKFLOW, "on", 0)
        self.assertEqual(re.findall(r"^  ([\w_]+):", triggers, re.MULTILINE), ["schedule", "workflow_dispatch"])
        self.assertIn('- cron: "30 6 * * *"', triggers)
        self.assertNotIn("legacy", WORKFLOW)

    def test_schedule_runs_every_job_without_dispatch_inputs(self):
        self.assertEqual(self.route("schedule", "", ""), {"tiers", "arm64", "live", "workflows"})

    def test_every_manual_suite_scenario_pair(self):
        for suite, scenario in itertools.product(self.choices("suite"), self.choices("scenario")):
            expected = set()
            if suite in ("all", "tiers"):
                expected.update(("tiers", "arm64"))
            if suite in ("all", "live"):
                if scenario in ("both", "admission", "hpa"):
                    expected.add("live")
                if scenario in ("both", "workflows"):
                    expected.add("workflows")
            with self.subTest(suite=suite, scenario=scenario):
                self.assertEqual(self.route("workflow_dispatch", suite, scenario), expected)

    def test_all_tiers_and_arm64_invocations_are_preserved(self):
        matrix = block(block(self.job_blocks["tiers"], "strategy", 4), "matrix", 6)
        tier_args = re.findall(r"^            tiers: (.+)$", matrix, re.MULTILINE)
        self.assertEqual(len(tier_args), 15)
        tiers = [int(n) for args in tier_args for n in re.findall(r"--tier (\d+)", args)]
        self.assertEqual(sorted(tiers), list(range(1, 20)))
        self.assertIn("--tier 17", tier_args)  # GC must retain a fresh runner/node.
        self.assertIn('integration-tests/e2e/run.sh ${{ matrix.tiers }} 2>&1 | tee "$E2E_WORK/runner.log"',
                      self.job_blocks["tiers"])
        arm64 = self.job_blocks["arm64"]
        self.assertEqual(scalar(arm64, "runs-on", 4), "ubuntu-24.04-arm")
        self.assertIn('integration-tests/e2e/run.sh --tier 1 --tier 2 --tier 3 2>&1 | tee "$E2E_WORK/runner.log"', arm64)
        self.assertIn('go env GOARCH)" = "arm64"', arm64)

    def test_live_matrix_and_two_fresh_runs_are_preserved(self):
        live = self.job_blocks["live"]
        matrix = block(block(live, "strategy", 4), "matrix", 6)
        value = scalar(matrix, "scenario", 8)
        match = re.fullmatch(r"\$\{\{ fromJSON\((.+)\) \}\}", value)
        self.assertIsNotNone(match)
        for scenario, expected in (
            ("", ["admission", "hpa"]), ("both", ["admission", "hpa"]),
            ("admission", ["admission"]), ("hpa", ["hpa"]),
        ):
            with self.subTest(scenario=scenario):
                self.assertEqual(json.loads(expression(match[1], {"inputs.scenario": scenario})), expected)
        self.assertIn('SCENARIO: ${{ matrix.scenario }}', live)
        self.assertIn('python3 "integration-tests/e2e/live/${SCENARIO}.py"\n'
                      '          python3 "integration-tests/e2e/live/${SCENARIO}.py"', live)
        self.assertIn("BREWLET_LIVE_CANDIDATE: ${{ inputs.candidate || 'checkout' }}", live)
        workflows = self.job_blocks["workflows"]
        matrix = block(block(workflows, "strategy", 4), "matrix", 6)
        self.assertEqual(json.loads(scalar(matrix, "run", 8)), [1, 2])
        self.assertIn("run: python3 integration-tests/e2e/live/workflows.py", workflows)
        for source in (live, workflows):
            self.assertIn("if: always()", source)
            self.assertIn("if-no-files-found: error", source)

    def test_job_budgets_and_sanitized_tier_artifacts(self):
        for job, budget in (("tiers", "60"), ("arm64", "30"), ("live", "180"), ("workflows", "90")):
            self.assertEqual(scalar(self.job_blocks[job], "timeout-minutes", 4), budget)
        for job in ("tiers", "arm64"):
            source = self.job_blocks[job]
            self.assertIn('collect_diagnostics.py "$E2E_WORK" "$RUNNER_TEMP/brewlet-tier-evidence"', source)
            self.assertEqual(source.count("if: always()"), 2)
            self.assertIn("path: ${{ runner.temp }}/brewlet-tier-evidence", source)
            self.assertNotIn("path: ${{ runner.temp }}/brewlet-tier-work", source)
            self.assertIn("if-no-files-found: error", source)

    def test_concurrency_and_permissions_are_unchanged(self):
        concurrency = block(WORKFLOW, "concurrency", 0)
        self.assertEqual(scalar(concurrency, "group", 2), "e2e-${{ github.workflow }}-${{ github.ref }}")
        self.assertEqual(scalar(concurrency, "cancel-in-progress", 2), "true")
        self.assertEqual(scalar(block(WORKFLOW, "permissions", 0), "contents", 2), "read")

    def test_expression_reader_rejects_unsupported_syntax(self):
        for source in ("unknown()", "inputs.suite in ['tiers']", "True", "inputs.suite + 'x'"):
            with self.subTest(source=source), self.assertRaises(AssertionError):
                expression(source, {"inputs.suite": "tiers"})


if __name__ == "__main__":
    unittest.main()
