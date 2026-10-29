#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Regenerate or check the website's pinned browser dependency notices."""

import argparse
import importlib.metadata
import json
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
MATERIAL_VERSION = "9.8.0"
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

LICENSE_FALLBACKS = {
    "delegate": "licenses/zenorocha-mit.txt",
    "good-listener": "licenses/zenorocha-mit.txt",
    "select": "licenses/zenorocha-mit.txt",
}
EXTRA_LICENSES = {
    "focus-visible": ["licenses/w3c-software-document-2015.txt"],
}
VERSIONED_SOURCE_URLS = {
    "@fortawesome/fontawesome-free": "https://github.com/FortAwesome/Font-Awesome/tree/7.3.1",
    "focus-visible": "https://github.com/WICG/focus-visible/tree/v5.2.1",
}
COMPONENT_NOTICES = {
    "@fortawesome/fontawesome-free": [
        "Browser artifact use: Material for MkDocs embeds the Font Awesome Free",
        '"Git Alt" icon as inline SVG in generated documentation pages. No Font',
        "Awesome font files are published by the Brewlet GitHub Pages build.",
        "Modification statement: Material for MkDocs inlines the SVG geometry into",
        "generated HTML; Brewlet makes no other changes.",
        "Creator: Fonticons, Inc. License for the icon: CC BY 4.0.",
        "Licensed material:",
        "https://github.com/FortAwesome/Font-Awesome/blob/7.3.1/svgs/brands/git-alt.svg",
        "License: https://creativecommons.org/licenses/by/4.0/",
    ],
    "focus-visible": [
        "Exact source archive:",
        "https://github.com/WICG/focus-visible/archive/refs/tags/v5.2.1.tar.gz",
        "Modification statement: Brewlet does not modify the upstream source.",
        "Material for MkDocs bundles and minifies it into its browser JavaScript.",
    ],
}


def license_files(directory):
    return sorted(path for path in directory.iterdir()
                  if path.is_file() and path.name.lower().startswith(
                      ("license", "copying", "notice", "patents", "authors")))


def append_component(parts, name, source, files, notices=(), fallback=None):
    if not any(path.name.lower().startswith(("license", "copying")) for path in files):
        if fallback is None:
            raise ValueError(f"no license text found for {name}")
        files = [fallback, *files]
    parts.extend(["", "=" * 80, f"Component: {name}", f"Source: {source}"])
    if notices:
        parts.extend(["", *notices])
    for path in files:
        text = "\n".join(line.rstrip() for line in path.read_text(encoding="utf-8").splitlines())
        if not text.strip():
            raise ValueError(f"empty attribution file for {name}: {path.name}")
        parts.extend(["", f"--- {path.name} ---", "", text.rstrip()])


def generate(repo):
    notice_dir = repo / "site/notices"
    manifest = json.loads((notice_dir / "package.json").read_text(encoding="utf-8"))
    lock = json.loads((notice_dir / "package-lock.json").read_text(encoding="utf-8"))
    if manifest["dependencies"] != lock["packages"][""]["dependencies"]:
        raise ValueError("notice package-lock.json differs from package.json; "
                         "run npm install --package-lock-only --prefix site/notices")
    for name, version in manifest["dependencies"].items():
        if lock["packages"].get(f"node_modules/{name}", {}).get("version") != version:
            raise ValueError(f"notice lock is missing the exact pinned dependency: {name} {version}")

    material = importlib.metadata.distribution("mkdocs-material")
    requirements = (repo / "site/requirements.txt").read_text(encoding="utf-8").splitlines()
    if f"mkdocs-material=={material.version}" not in requirements:
        raise ValueError(f"installed mkdocs-material {material.version} differs from site/requirements.txt")
    if material.version != MATERIAL_VERSION:
        raise ValueError(f"review the browser dependency inventory for mkdocs-material {material.version}; "
                         f"it currently covers {MATERIAL_VERSION}")
    parts = [HEADER.rstrip()]
    material_files = sorted(
        Path(material.locate_file(file)) for file in material.files or []
        if ".dist-info/" in str(file)
        and Path(str(file)).name.lower().startswith(("license", "copying", "notice"))
    )
    append_component(parts, f"Material for MkDocs {material.version}",
                     "https://github.com/squidfunk/mkdocs-material", material_files)

    for location, record in sorted(lock["packages"].items()):
        if not location:
            continue
        package_dir = notice_dir / location
        metadata = json.loads((package_dir / "package.json").read_text(encoding="utf-8"))
        name, version = metadata["name"], metadata["version"]
        expected_name = location.rsplit("node_modules/", 1)[-1]
        if name != expected_name or version != record["version"]:
            raise ValueError(f"installed notice input differs from lock: {location}; "
                             "run npm ci --ignore-scripts --prefix site/notices")
        if name in VERSIONED_SOURCE_URLS and not VERSIONED_SOURCE_URLS[name].endswith(
                "/" + ("v" if name == "focus-visible" else "") + version):
            raise ValueError(f"update source URLs and attribution for {name} {version}")
        repository = metadata.get("repository", "")
        if isinstance(repository, dict):
            repository = repository.get("url", "")
        files = license_files(package_dir)
        fallback = notice_dir / LICENSE_FALLBACKS[name] if name in LICENSE_FALLBACKS else None
        files.extend(notice_dir / path for path in EXTRA_LICENSES.get(name, []))
        append_component(parts, f"{name} {version}",
                         VERSIONED_SOURCE_URLS.get(name, repository or "npm registry"),
                         files, COMPONENT_NOTICES.get(name, ()), fallback)
    return "\n".join(parts) + "\n"


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true", help="fail if site/NOTICE.txt is stale")
    args = parser.parse_args(argv)
    text = generate(ROOT)
    output = ROOT / "site/NOTICE.txt"
    if args.check:
        if not output.is_file() or output.read_text(encoding="utf-8") != text:
            parser.exit(1, "site/NOTICE.txt is stale; run make site-notices\n")
    else:
        output.write_text(text, encoding="utf-8")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
