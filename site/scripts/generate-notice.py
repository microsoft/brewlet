#!/usr/bin/env python3

import importlib.metadata
import json
from pathlib import Path
import sys


HEADER = """NOTICES AND INFORMATION
Do Not Translate or Localize

This software incorporates material from third parties. Microsoft makes certain
open source code available at https://3rdpartysource.microsoft.com, or you may
send a check or money order for US $5.00, including the product name, the open
source component name, and version number, to:

Source Code Compliance Team
Microsoft Corporation
One Microsoft Way
Redmond, WA 98052
USA

Notwithstanding any other terms, you may reverse engineer this software to the
extent required to debug changes to any libraries licensed under the GNU Lesser
General Public License.
"""

MATERIAL_LICENSES = {
    "Material for MkDocs": "mkdocs_material-9.7.7.dist-info/licenses/LICENSE",
}

LICENSE_FALLBACKS = {
    "delegate": "licenses/zenorocha-mit.txt",
    "good-listener": "licenses/zenorocha-mit.txt",
    "select": "licenses/zenorocha-mit.txt",
}


def license_files(package_dir: Path) -> list[Path]:
    prefixes = ("license", "copying", "notice", "patents", "authors")
    return sorted(
        path
        for path in package_dir.iterdir()
        if path.is_file() and path.name.lower().startswith(prefixes)
    )


def append_component(parts: list[str], name: str, source: str, files: list[Path]) -> None:
    if not files:
        raise RuntimeError(f"no attribution files found for {name}")
    parts.extend([
        "",
        "=" * 80,
        f"Component: {name}",
        f"Source: {source}",
    ])
    for path in files:
        text = "\n".join(
            line.rstrip() for line in path.read_text(encoding="utf-8").splitlines()
        )
        parts.extend(["", f"--- {path.name} ---", "", text.rstrip()])


def npm_packages(node_modules: Path) -> list[Path]:
    packages = []
    for package_json in node_modules.glob("*/package.json"):
        packages.append(package_json.parent)
    for package_json in node_modules.glob("@*/*/package.json"):
        packages.append(package_json.parent)
    return sorted(packages)


def main() -> int:
    repo = Path(__file__).resolve().parents[2]
    notice_dir = repo / "site" / "notices"
    node_modules = notice_dir / "node_modules"
    output = repo / "site" / "NOTICE.txt"

    material = importlib.metadata.distribution("mkdocs-material")
    if material.version != "9.7.7":
        raise RuntimeError(f"expected mkdocs-material 9.7.7, found {material.version}")

    parts = [HEADER.rstrip()]
    for name, relative_path in MATERIAL_LICENSES.items():
        path = Path(material.locate_file(relative_path))
        append_component(
            parts,
            f"{name} {material.version}",
            "https://github.com/squidfunk/mkdocs-material",
            [path],
        )

    expected = json.loads((notice_dir / "package.json").read_text(encoding="utf-8"))["dependencies"]
    found = {}
    for package_dir in npm_packages(node_modules):
        metadata = json.loads((package_dir / "package.json").read_text(encoding="utf-8"))
        name = metadata["name"]
        version = metadata["version"]
        found[name] = version
        repository = metadata.get("repository", "")
        if isinstance(repository, dict):
            repository = repository.get("url", "")
        files = license_files(package_dir)
        if not files and name in LICENSE_FALLBACKS:
            files = [notice_dir / LICENSE_FALLBACKS[name]]
        append_component(parts, f"{name} {version}", repository or "npm registry", files)

    missing = sorted(set(expected) - set(found))
    mismatched = sorted(
        f"{name}: expected {version}, found {found.get(name)}"
        for name, version in expected.items()
        if name in found and found[name] != version
    )
    if missing or mismatched:
        raise RuntimeError(f"notice inputs differ from package.json: missing={missing}, mismatched={mismatched}")

    output.write_text("\n".join(parts) + "\n", encoding="utf-8")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as error:
        print(f"generate-notice: {error}", file=sys.stderr)
        raise SystemExit(1)
