#!/usr/bin/env python3
"""Build the landing page and strict MkDocs docs using the reference Pages layout."""

from importlib import metadata
from pathlib import Path
import os
import shutil
import subprocess
import sys
import re

from social_card import render as render_social_card

ROOT = Path(__file__).resolve().parents[2]
SITE = ROOT / "site"
OUTPUT = SITE / "_site"


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


def main():
    subprocess.run([sys.executable, "-m", "mkdocs", "build", "--strict",
                    "-f", str(SITE / "mkdocs.yml")], cwd=ROOT, check=True,
                   env={**os.environ, "PYTHONDONTWRITEBYTECODE": "1"})
    shutil.copytree(SITE / "assets", OUTPUT / "assets", dirs_exist_ok=True)
    render_social_card(OUTPUT / "assets/images/social-card.png")
    shutil.copy2(SITE / "index.html", OUTPUT / "index.html")
    shutil.copy2(SITE / "install.sh", OUTPUT / "install.sh")
    shutil.copy2(ROOT / "LICENSE.txt", OUTPUT / "LICENSE.txt")
    (OUTPUT / "NOTICE.txt").write_text(notices())
    tokens = (SITE / "assets/css/theme.css").read_text()
    for page in OUTPUT.rglob("*.html"):
        html = page.read_text()
        html = re.sub(r'<link[^>]+href="[^"]*assets/css/theme\.css"[^>]*>',
                      lambda _: f"<style>\n{tokens}</style>", html)
        page.write_text(html)
    search = OUTPUT / "docs/assets/javascripts/lunr"
    if search.is_dir():
        shutil.rmtree(search)
    subprocess.run([sys.executable, str(SITE / "scripts/check.py"), str(OUTPUT)],
                   cwd=ROOT, check=True)
    print(f"Built static website: {OUTPUT}")


if __name__ == "__main__":
    main()
