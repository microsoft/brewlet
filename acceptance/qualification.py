"""Shared test selection; independent proposal requirements constrain the matrix."""

import json
from pathlib import Path


MATRIX_PATH = Path(__file__).with_name("qualification-matrix.json")
REQUIRED_ARCHITECTURES = {"arm64", "amd64"}
REQUIRED_FIXTURES = {
    "native": {"python": "python"},
    "wasm": {"wasmtime": "webassembly", "spin": "webassembly"},
}


def validate_matrix(document):
    if not isinstance(document, dict):
        return ["qualification matrix must be an object"]
    errors = []
    if (set(document) != {"schemaVersion", "architectures", "fixtureGroups", "compositionScopes"}
            or type(document.get("schemaVersion")) is not int or document["schemaVersion"] != 1):
        errors.append("unknown qualification matrix fields/version")
    architectures = document.get("architectures")
    if (not isinstance(architectures, list) or len(architectures) != 2
            or not all(isinstance(value, str) for value in architectures)
            or set(architectures) != REQUIRED_ARCHITECTURES):
        errors.append("both native arm64 and amd64 architectures are mandatory without duplicates")
    groups = document.get("fixtureGroups")
    if not isinstance(groups, dict):
        errors.append("fixtureGroups must be an object")
    else:
        if set(groups) != set(REQUIRED_FIXTURES):
            errors.append("exactly the native and wasm fixture groups are mandatory")
        for group, required in REQUIRED_FIXTURES.items():
            rows = groups.get(group)
            if (not isinstance(rows, list)
                    or not all(isinstance(row, dict) and set(row) == {"id", "category"}
                               and isinstance(row["id"], str) and isinstance(row["category"], str)
                               for row in rows)):
                errors.append(f"{group}: fixtures require explicit id/category records")
                continue
            if len(rows) != len(required) or {row["id"]: row["category"] for row in rows} != required:
                errors.append(f"{group}: all required fixtures/categories must occur exactly once")
    scopes = document.get("compositionScopes")
    required_scopes = {
        **{arch: {"architecture": arch, "group": "native", "scenario": "managed-composition"}
           for arch in REQUIRED_ARCHITECTURES},
        **{"wasm-" + arch: {"architecture": arch, "group": "wasm",
                           "scenario": "managed-wasm-composition"}
           for arch in REQUIRED_ARCHITECTURES},
    }
    if not isinstance(scopes, dict) or scopes != required_scopes:
        errors.append("all native/Wasm composition scopes require their exact architecture, group and scenario")
    return errors


def _unique_fields(pairs):
    document = {}
    for name, value in pairs:
        if name in document:
            raise ValueError("invalid qualification matrix: duplicate field " + name)
        document[name] = value
    return document


def load_matrix(path=MATRIX_PATH):
    document = json.loads(Path(path).read_text(), object_pairs_hook=_unique_fields)
    errors = validate_matrix(document)
    if errors:
        raise ValueError("invalid qualification matrix: " + "; ".join(errors))
    return document


def fixture_names(document, group):
    return tuple(row["id"] for row in document["fixtureGroups"][group])
