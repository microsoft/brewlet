"""Run every shared validator fixture and actual Maven launch publication."""

from concurrent.futures import ThreadPoolExecutor
import json
import os
from pathlib import Path
import queue
import shutil
import subprocess

PLUGIN = "sh.brewlet:brewlet-maven-plugin:" + json.loads(
    (Path(__file__).resolve().parents[1] / "release.json").read_text())["version"] + ":"

WORKERS = min(4, os.cpu_count() or 1)


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


def publication_case(executable, project, work, case):
    kind = case["kind"]
    platform = "linux/amd64"
    if kind == "launch":
        descriptor = encode(case["launch"])
    elif kind == "id":
        descriptor = json.dumps({
            "schemaVersion": 1,
            "process": {"entrypoint": ["runner"]},
            "runtimeImages": [case["value"]],
        })
    else:
        platform = case["value"]
        descriptor = '{"schemaVersion":1,"process":{"entrypoint":["runner"]}}'
    source = work / f'conformance-{case["name"]}.json'
    source.write_text(descriptor)
    layout = work / f'conformance-{case["name"]}-oci'
    command = [
        "mvn", "-B", "-q", "-f", str(project), "package",
        PLUGIN + "push",
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


def run(executable, project, work, fixture_file, workers=WORKERS):
    fixture = dict(json.loads(Path(fixture_file).read_text(), object_pairs_hook=Pairs))
    shared = work / "published-artifact-v1.json"
    shared.write_bytes(Path(fixture_file).read_bytes())
    report = work / "maven-shared-conformance.json"
    subprocess.run([
        "mvn", "-B", "-q", "-f", str(project),
        PLUGIN + "conformance",
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
    cases = [dict(encoded) for encoded in fixture["cases"]]
    cases = [case for case in cases if case["kind"] in ("launch", "id", "platform")]
    assert cases, "No shared producer conformance fixtures executed"
    # Each worker packages its own project copy so concurrent builds never share target/.
    projects = queue.Queue()
    for index in range(max(1, min(workers, len(cases)))):
        copy = work / f"conformance-project-{index}"
        shutil.copytree(Path(project).parent, copy, ignore=shutil.ignore_patterns("target"))
        projects.put(copy / Path(project).name)
    def publish(case):
        copy = projects.get()
        try:
            publication_case(executable, copy, work, case)
        finally:
            projects.put(copy)
    with ThreadPoolExecutor(max_workers=projects.qsize()) as pool:
        for future in [pool.submit(publish, case) for case in cases]:
            future.result()
    count = len(cases)
    print(f"Passed {count} shared Maven/CLI launch, ID and platform cases")
    return count
