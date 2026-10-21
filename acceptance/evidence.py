"""Candidate identities and fail-closed public execution receipts."""

import fnmatch
from functools import lru_cache
import hashlib
import io
import json
from pathlib import Path
import re
import subprocess


ROOT = Path(__file__).resolve().parents[1]
PRODUCTION_INPUTS = ("go.mod", "go.sum", "pkg", "kubernetes", "cmd", "provisioner",
                     "shim", "scripts", "LICENSE.txt")
IGNORE = (".git", "__pycache__", ".DS_Store", ".test-*", "*.test", ".validation.*",
          ".admission-*", ".operator-*", ".node-*")
IDENTITY_VERSION = 1


def _unique(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON field: " + key)
        result[key] = value
    return result


def read_json(path):
    return json.loads(Path(path).read_text(), object_pairs_hook=_unique)


def _digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def production_manifest_bytes(hashes):
    """Encode string paths canonically, not in pathlib's component order."""
    return json.dumps(dict(sorted(hashes.items())), indent=2).encode()


def git(root, *args):
    return subprocess.check_output(["git", "-C", str(root), *args], stderr=subprocess.PIPE)


def _normalized(path, content):
    if path.startswith("acceptance/results/") and path.endswith(".json"):
        return None
    if path == "acceptance/RESULTS.md":
        return None
    if path in ("acceptance/requirements.json", "acceptance/composition-requirements.json"):
        document = json.loads(content, object_pairs_hook=_unique)
        for row in document.get("requirements", []) + document.get("amendments", []):
            row.pop("status", None)
            for evidence in row.get("evidence", []):
                for key in ("outcome", "result"):
                    evidence.pop(key, None)
        for check in document.get("checks", {}).values():
            for key in ("outcome", "result"):
                check.pop(key, None)
        return json.dumps(document, sort_keys=True, separators=(",", ":")).encode()
    return content


def _input_hashes(root, revision, *, working=False):
    """Hash immutable committed inputs using the same production snapshot policy."""
    root = Path(root)
    revision = revision or "HEAD"
    rows = git(root, "ls-tree", "-rz", "--full-tree", revision).split(b"\0")
    qualification, production = {}, {}
    entries = []
    for row in rows:
        if not row:
            continue
        metadata, raw_path = row.split(b"\t", 1)
        mode, kind, blob = metadata.decode().split()
        path = raw_path.decode()
        if kind != "blob" or mode not in ("100644", "100755"):
            raise ValueError("unsupported qualification input: " + path)
        if path == "acceptance/RESULTS.md" or (path.startswith("acceptance/results/") and path.endswith(".json")):
            continue
        entries.append((path, mode, blob))
    stream = None
    if not working:
        process = subprocess.run(["git", "-C", str(root), "cat-file", "--batch"],
                                 input="".join(blob + "\n" for _, _, blob in entries).encode(),
                                 capture_output=True, check=True)
        stream = io.BytesIO(process.stdout)
    for path, mode, blob in entries:
        if working:
            source = root / path
            if source.is_symlink():
                raise ValueError("qualification input became a symlink: " + path)
            mode = "100755" if source.stat().st_mode & 0o111 else "100644"
            content = source.read_bytes()
        else:
            actual_blob, kind, size = stream.readline().decode().strip().split()
            if actual_blob != blob or kind != "blob":
                raise ValueError("qualification input blob unavailable: " + path)
            content = stream.read(int(size))
            if len(content) != int(size) or stream.read(1) != b"\n":
                raise ValueError("truncated qualification input: " + path)
        normalized = _normalized(path, content)
        if normalized is not None:
            qualification[path] = [mode, hashlib.sha256(normalized).hexdigest()]
        if (path.split("/")[0] in PRODUCTION_INPUTS
                and not any(fnmatch.fnmatch(part, pattern) for part in Path(path).parts for pattern in IGNORE)):
            production[path] = hashlib.sha256(content).hexdigest()
    production_hash = hashlib.sha256(production_manifest_bytes(production)).hexdigest()
    return {"identityVersion": IDENTITY_VERSION, "qualificationInputsSHA256": _digest(qualification),
            "productionSourceManifestSHA256": production_hash}


@lru_cache(maxsize=32)
def input_hashes(root=ROOT, revision=None):
    return _input_hashes(root, revision or "HEAD")


def working_identity(root=ROOT):
    root = Path(root)
    try:
        checkout = git(root, "rev-parse", "HEAD").decode().strip()
        if git(root, "status", "--porcelain", "--untracked-files=normal").strip():
            raise ValueError("qualification requires a clean committed checkout")
        committed = input_hashes(root, checkout)
        if _input_hashes(root, checkout, working=True) != committed:
            raise ValueError("working qualification inputs differ from the immutable checkout")
        return {"checkout": checkout, **committed}
    except subprocess.CalledProcessError as error:
        raise ValueError("qualification checkout history unavailable") from error


def candidate_errors(result, root=ROOT, expected=None):
    errors = []
    if type(result.get("identityVersion")) is not int or result["identityVersion"] != IDENTITY_VERSION:
        errors.append("unsupported candidate identity version")
    checkout = result.get("checkout", "")
    if not isinstance(checkout, str) or not re.fullmatch(r"[0-9a-f]{40}", checkout):
        return errors + ["missing immutable checkout identity"]
    if not all(isinstance(result.get(key), str) and re.fullmatch(r"[0-9a-f]{64}", result[key])
               for key in ("qualificationInputsSHA256", "productionSourceManifestSHA256")):
        errors.append("missing candidate input hashes")
    if errors:
        return errors
    try:
        actual = input_hashes(root, checkout)
        expected = expected or working_identity(root)
        subprocess.run(["git", "-C", str(root), "merge-base", "--is-ancestor", checkout,
                        expected["checkout"]], check=True, capture_output=True)
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        return errors + ["candidate history/inputs unavailable or checkout is not an ancestor: " + str(error)]
    for key in ("identityVersion", "qualificationInputsSHA256", "productionSourceManifestSHA256"):
        if result.get(key) != actual[key] or result.get(key) != expected.get(key):
            errors.append("candidate identity mismatch: " + key)
    return errors


def load_receipt(root, relative):
    if not isinstance(relative, str) or not relative:
        raise ValueError("persistent public receipt required")
    root = Path(root).resolve()
    path = (root / relative).resolve()
    if not path.is_relative_to(root / "acceptance" / "results") or not path.is_file():
        raise ValueError("receipt must be an existing file beneath acceptance/results")
    result = read_json(path)
    if not isinstance(result, dict):
        raise ValueError("receipt must be an object")
    return result


def receipt_errors(result, requirements=(), *, live=False, architecture=None,
                   scenario=None, languages=None, root=ROOT, expected=None):
    errors = []
    assertions = result.get("assertions")
    if (not isinstance(assertions, list) or not assertions
            or not all(isinstance(row, dict) and isinstance(row.get("name"), str)
                       and row["name"] and row.get("outcome") == "passed"
                       and isinstance(row.get("requirements"), list)
                       and all(isinstance(value, str) for value in row["requirements"])
                       for row in assertions)):
        errors.append("nonempty, successful assertion records required")
        covered = set()
    else:
        covered = {identifier for row in assertions for identifier in row["requirements"]}
    if result.get("outcome") != "passed" or result.get("interventions"):
        errors.append("clean successful execution required")
    if not set(requirements) <= covered:
        errors.append("missing assertion coverage: " + ", ".join(sorted(set(requirements) - covered)))
    if live:
        destruction = result.get("fixtureDestruction")
        if (result.get("scope") not in ("real-node-execution-only", "real-node-execution-and-remote-delivery")
                or result.get("cleanSelectedSuitePassed") is not True
                or not isinstance(destruction, dict) or destruction.get("outcome") != "passed"
                or result.get("productionCleanupVerified") is not True
                or result.get("architecture") not in ("amd64", "arm64")
                or not isinstance(result.get("selectedScenario"), str) or not result["selectedScenario"]
                or not isinstance(result.get("selectedLanguages"), list) or not result["selectedLanguages"]
                or not all(isinstance(value, str) for value in result["selectedLanguages"])):
            errors.append("clean real-node scope and successful fixture destruction required")
        if architecture and result.get("architecture") != architecture:
            errors.append("wrong native architecture")
        if scenario and result.get("selectedScenario") != scenario:
            errors.append("wrong selected scenario")
        if languages is not None and (not isinstance(result.get("selectedLanguages"), list)
                                      or not all(isinstance(value, str) for value in result["selectedLanguages"])
                                      or len(result["selectedLanguages"]) != len(languages)
                                      or set(result["selectedLanguages"]) != set(languages)):
            errors.append("wrong selected workloads")
    elif result.get("scope") not in ("unit", "contract", "integration", "inspection"):
        errors.append("wrong non-live execution scope")
    errors.extend(candidate_errors(result, root, expected))
    return errors
