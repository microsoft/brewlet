#!/usr/bin/env python3
"""Build the landing page and strict MkDocs docs for the latest release (/docs/) and main (/docs/main/)."""

from importlib import metadata
from pathlib import Path
import argparse
import html
import io
import os
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile

from social_card import render as render_social_card

ROOT = Path(__file__).resolve().parents[2]
SITE = ROOT / "site"
OUTPUT = SITE / "_site"
DOCS = OUTPUT / "docs"
MAIN = "main"
STABLE_TAG = re.compile(r"v\d+\.\d+\.\d+")
RELEASE_TAG = re.compile(r"v\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?")


def notices():
    distributions = {}
    pending = ["mkdocs", "mkdocs-material", "Pillow"]
    while pending:
        distribution = metadata.distribution(pending.pop())
        name = distribution.metadata["Name"]
        if name in distributions:
            continue
        distributions[name] = distribution
        for requirement in distribution.requires or []:
            from packaging.requirements import Requirement
            parsed = Requirement(requirement)
            if parsed.marker is None or parsed.marker.evaluate({"extra": ""}):
                pending.append(parsed.name)
    parts = ["Brewlet website\n===============\n",
             "Brewlet source is MIT licensed; see LICENSE.txt.\n"
             "The generated documentation bundles the following dependencies.\n"
             "Versions and license text come from the installed build environment.\n",
             "The social preview uses Pillow's bundled Aileron Regular font.\n"
             "Aileron by dot colon is dedicated to the public domain under CC0 1.0.\n"
             "https://dotcolon.net/fonts/aileron/\n"
             "https://creativecommons.org/publicdomain/zero/1.0/\n",
             "The main website's attribution snapshot below covers vendor browser\n"
             "assets of the same pinned Material version. Some listed assets are\n"
             "not used by this preview (no external fonts or Mermaid are loaded).\n",
             (SITE / "NOTICE.txt").read_text()]
    for name, distribution in sorted(distributions.items()):
        parts.append(f"\n{name} {distribution.version}\n{'-' * len(name)}\n")
        licenses = [file for file in distribution.files or []
                    if any(part.lower().startswith(("license", "copying", "notice"))
                           for part in Path(str(file)).parts)
                    and (".dist-info/" in str(file) or ".egg-info/" in str(file))]
        if not licenses:
            raise RuntimeError(f"Missing installed license text for site dependency: {name}")
        for file in sorted(licenses, key=str):
            parts.append(distribution.locate_file(file).read_text() + "\n")
    return "\n".join(parts)


def git(*args):
    return subprocess.run(["git", *args], cwd=ROOT, check=True, capture_output=True, text=True).stdout


def release_ref(explicit=None):
    """Return the release tag whose documentation is served at /docs/."""
    ref = explicit or os.environ.get("BREWLET_DOCS_RELEASE")
    if not ref:
        # CI passes the published latest release; locally, use the newest stable tag.
        ref = next((tag for tag in git("tag", "--list", "v*", "--sort=-v:refname").split()
                    if STABLE_TAG.fullmatch(tag)), None)
        if not ref:
            raise SystemExit("No vX.Y.Z release tag found: fetch tags or pass --release-ref.")
    if not RELEASE_TAG.fullmatch(ref):
        raise SystemExit(f"Invalid release tag: {ref}")
    try:
        git("rev-parse", "--verify", "--quiet", f"refs/tags/{ref}^{{commit}}")
    except subprocess.CalledProcessError:
        raise SystemExit(f"Release tag {ref} is not available locally: fetch tags first.") from None
    return ref


def mkdocs(source, destination, **env):
    subprocess.run([sys.executable, "-m", "mkdocs", "build", "--strict",
                    "-f", str(source / "site/mkdocs.yml"), "-d", str(destination)],
                   cwd=source, check=True,
                   env={**os.environ, "PYTHONDONTWRITEBYTECODE": "1", **env})


def build_release(ref):
    # Build the tag with its own docs, configuration and hooks so the release pages match it exactly.
    with tempfile.TemporaryDirectory(prefix="brewlet-docs-") as temp:
        archive = subprocess.run(["git", "archive", "--format=tar", f"refs/tags/{ref}"],
                                 cwd=ROOT, check=True, capture_output=True).stdout
        with tarfile.open(fileobj=io.BytesIO(archive)) as tar:
            tar.extractall(temp, filter="data")
        mkdocs(Path(temp), DOCS, BREWLET_DOCS_REF=ref)


def pages(root, exclude=None):
    return {page.relative_to(root).as_posix() for page in root.rglob("*.html")
            if exclude is None or not page.is_relative_to(exclude)}


def href(target, page):
    """Relative link from a page to a built file, using directory URLs for index pages."""
    if target.name == "index.html":
        return Path(os.path.relpath(target.parent, page.parent)).as_posix() + "/"
    return Path(os.path.relpath(target, page.parent)).as_posix()


def inject(text, marker, fragment, before=True):
    index = text.find(marker)
    if index < 0:
        return None
    index = index if before else index + len(marker)
    return text[:index] + fragment + text[index:]


def add_version_selector(ref):
    """Link each docs page to the same page, or the docs home, of the other version."""
    versions = [("release", DOCS, pages(DOCS, DOCS / MAIN), f"Latest release ({ref})", ref),
                (MAIN, DOCS / MAIN, pages(DOCS / MAIN), "Development (main)", MAIN)]
    for current, root, members, _, short in versions:
        for name in sorted(members):
            if Path(name).name == "404.html":
                continue  # Served at arbitrary paths, so relative links cannot resolve.
            page = root / name
            text = page.read_text()
            if '<header class="md-header' not in text:
                continue  # Standalone HTML assets, such as the printable cheat sheet.
            links = {}
            for version, other, other_members, label, _ in versions:
                target = other / (name if name in other_members else "index.html")
                links[version] = (label, href(target, page))
            selected = ' aria-current="page"'
            items = "".join(
                f'<li><a href="{html.escape(link)}"{selected if version == current else ""}>'
                f'{html.escape(label)}</a></li>'
                for version, (label, link) in links.items())
            menu = (f'<details class="brewlet-version"><summary title="Documentation version"'
                    f' aria-label="Documentation version: {html.escape(links[current][0])}">'
                    f'{html.escape(short)}</summary><ul class="brewlet-version__list">{items}</ul></details>')
            assets = Path(os.path.relpath(OUTPUT, page.parent)).as_posix()
            head = (f'<link rel="stylesheet" href="{assets}/assets/css/docs-version.css">'
                    f'<script src="{assets}/assets/js/docs-version.js" defer></script>')
            text = inject(text, "</head>", head)
            text = text and (inject(text, '<label class="md-header__button md-icon" for="__search">', menu)
                             or inject(text, '<div class="md-header__source">', menu))
            if current == MAIN and text:
                label, link = links["release"]
                banner = ('<aside class="brewlet-version-banner" role="note"><p>You are reading the'
                          ' development documentation for unreleased changes on <code>main</code>.'
                          f' <a href="{html.escape(link)}">Switch to the {html.escape(label.lower())}</a>.</p></aside>')
                text = inject(text, '<header class="md-header', banner)
            if not text:
                raise RuntimeError(f"Cannot place the version selector in {page.relative_to(OUTPUT)}")
            page.write_text(text)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--release-ref", help="release tag served at /docs/ (default: newest vX.Y.Z tag)")
    ref = release_ref(parser.parse_args().release_ref)
    build_release(ref)
    if (DOCS / MAIN).exists():
        raise RuntimeError(f"Release {ref} documentation already uses the {MAIN}/ path")
    mkdocs(ROOT, DOCS / MAIN, BREWLET_DOCS_SUBDIR=MAIN)
    add_version_selector(ref)
    shutil.copytree(SITE / "assets", OUTPUT / "assets", dirs_exist_ok=True)
    render_social_card(OUTPUT / "assets/images/social-card.png")
    shutil.copy2(SITE / "index.html", OUTPUT / "index.html")
    shutil.copy2(SITE / "install.sh", OUTPUT / "install.sh")
    shutil.copy2(ROOT / "LICENSE.txt", OUTPUT / "LICENSE.txt")
    (OUTPUT / "NOTICE.txt").write_text(notices())
    tokens = (SITE / "assets/css/theme.css").read_text()
    for page in OUTPUT.rglob("*.html"):
        text = re.sub(r'<link[^>]+href="[^"]*assets/css/theme\.css"[^>]*>',
                      lambda _: f"<style>\n{tokens}</style>", page.read_text())
        page.write_text(text)
    for search in (DOCS / "assets/javascripts/lunr", DOCS / MAIN / "assets/javascripts/lunr"):
        if search.is_dir():
            shutil.rmtree(search)
    subprocess.run([sys.executable, str(SITE / "scripts/check.py"), str(OUTPUT)],
                   cwd=ROOT, check=True)
    print(f"Built static website: {OUTPUT}")


if __name__ == "__main__":
    main()
