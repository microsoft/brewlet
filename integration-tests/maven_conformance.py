"""Run every shared validator fixture and actual Maven launch publication."""

import json
from pathlib import Path
import subprocess


class Pairs(list):
    pass


def encode(value):
    if isinstance(value, Pairs):
        return "{" + ",".join(json.dumps(key) + ":" + encode(item) for key, item in value) + "}"
    if isinstance(value, list):
        return "[" + ",".join(encode(item) for item in value) + "]"
    return json.dumps(value)


def normalized(process):
    return {
        "entrypoint": process["entrypoint"],
        "args": process.get("args") or [],
        "workingDir": process.get("workingDir", "/app"),
        "env": process.get("env") or [],
    }


def run(executable, project, work, fixture_file):
    fixture = dict(json.loads(Path(fixture_file).read_text(), object_pairs_hook=Pairs))
    shared = work / "published-artifact-v2.json"
    shared.write_bytes(Path(fixture_file).read_bytes())
    report = work / "maven-shared-conformance.json"
    subprocess.run([
        "mvn", "-B", "-q", "-f", str(project),
        "sh.brewlet:brewlet-maven-plugin:2.0.0-SNAPSHOT:conformance",
        f"-Dbrewlet.executable={executable}",
        f"-Dbrewlet.fixtures={shared.resolve()}",
        f"-Dbrewlet.conformanceResult={report.resolve()}",
    ], check=True)
    actual = json.loads(report.read_text())
    expected = json.loads(subprocess.check_output(
        [str(executable), "conformance", str(shared.resolve())], text=True))
    assert actual == expected, "Maven and CLI normalized fixture results differ"
    assert len(actual) == len(fixture["cases"]) and all(item["passed"] for item in actual)
    print(f"Passed all {len(actual)} shared Maven/CLI validator cases")
    count = 0
    for encoded_case in fixture["cases"]:
        case = dict(encoded_case)
        kind = case["kind"]
        if kind not in ("launch", "id", "platform"):
            continue
        platform = "linux/amd64"
        if kind == "launch":
            descriptor = encode(case["launch"])
        elif kind == "id":
            descriptor = json.dumps({
                "schemaVersion": 2,
                "process": {"entrypoint": ["runner"]},
                "requires": {"runtimeImages": [case["value"]]},
            })
        else:
            platform = case["value"]
            descriptor = '{"schemaVersion":2,"process":{"entrypoint":["runner"]}}'
        source = work / f'conformance-{case["name"]}.json'
        source.write_text(descriptor)
        layout = work / f'conformance-{case["name"]}-oci'
        command = [
            "mvn", "-B", "-q", "-f", str(project), "package",
            "sh.brewlet:brewlet-maven-plugin:2.0.0-SNAPSHOT:push",
            f"-Dbrewlet.executable={executable}",
            f"-Dbrewlet.launch={source}",
            f"-Dbrewlet.store={layout}",
            f'-Dbrewlet.image=fixtures/conformance-{case["name"]}:v2',
            f"-Dbrewlet.platforms={platform}",
        ]
        result = subprocess.run(command, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        reason = case.get("reason")
        if reason:
            assert result.returncode != 0, f'{case["name"]}: Maven accepted invalid input'
            assert reason in result.stdout, f'{case["name"]}: expected {reason}: {result.stdout}'
        else:
            assert result.returncode == 0, f'{case["name"]}: {result.stdout}'
            output = subprocess.check_output([
                str(executable), "inspect", f'fixtures/conformance-{case["name"]}:v2',
                "--store", str(layout),
            ], text=True)
            inspected = json.loads(output)["platforms"][0]["launch"]["process"]
            if kind == "launch":
                expected = json.loads(encode(case["process"]))
                assert normalized(inspected) == normalized(expected), case["name"]
        count += 1
    assert count > 0, "No shared producer conformance fixtures executed"
    print(f"Passed {count} shared Maven/CLI launch, ID and platform cases")
    return count
