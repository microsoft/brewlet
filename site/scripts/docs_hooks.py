"""Render the specification and keep repository-source links usable without editing drafts."""

from pathlib import Path
import os
import re

from mkdocs.structure.files import File

# Release builds link repository sources at their tag; edits always target main.
SOURCE = f"https://github.com/microsoft/brewlet/blob/{os.environ.get('BREWLET_DOCS_REF') or 'main'}/"
EDIT = "https://github.com/microsoft/brewlet/edit/main/"
ROOT = Path(__file__).resolve().parents[2]
SPECIFICATION = ROOT / "specs/SPECIFICATION.md"
SPECIFICATION_PAGE = "specification.md"
LINK = re.compile(r"(?s)(```.*?```|`[^`\n]+`)|\[([^\]]+)\]\(([^)]+)\)")


def on_config(config):
    # Main's docs are nested below the release docs (/docs/main/); keep links to the site root working.
    subdir = os.environ.get("BREWLET_DOCS_SUBDIR", "").strip("/")
    if subdir:
        up = "../" * len(subdir.split("/"))
        config.site_url = f"{config.site_url}{subdir}/"
        config.extra["homepage"] = up + config.extra["homepage"]
        config.theme["favicon"] = up + config.theme["favicon"]
        config.extra_css = [up + css if css.startswith("../") else css for css in config.extra_css]
    return config


def on_files(files, config):
    files.append(File.generated(config, SPECIFICATION_PAGE, abs_src_path=str(SPECIFICATION)))
    return files


def on_page_content(html, page, config, files):
    # Material lists only the first H1's children; keep the specification's Part H1s in the TOC.
    if page.file.src_uri == SPECIFICATION_PAGE and len(page.toc.items) > 1:
        title, *parts = page.toc.items
        title.children.extend(parts)
        page.toc.items[:] = [title]
    return html


def on_page_markdown(markdown, page, config, files):
    docs = Path(config["docs_dir"]).resolve()
    source = Path(page.file.abs_src_path).resolve()
    if source == SPECIFICATION:
        page.edit_url = f"{EDIT}specs/SPECIFICATION.md"

    def rewrite(match):
        code, label, target = match.groups()
        if code:
            return code
        if "://" in target or target.startswith("/"):
            return match.group(0)
        filename, separator, anchor = target.partition("#")
        resolved = (source.parent / filename).resolve() if filename else source
        if resolved == SPECIFICATION:
            # GitHub keeps both hyphens around an em dash; Python-Markdown collapses them.
            anchor = re.sub("-+", "-", anchor)
            if not filename:
                return f"[{label}](#{anchor})"
            resolved = docs / SPECIFICATION_PAGE
        elif not filename or (resolved.is_relative_to(docs) and source.is_relative_to(docs)):
            return match.group(0)
        if resolved.is_relative_to(docs):
            path = Path(os.path.relpath(resolved, (docs / page.file.src_uri).parent)).as_posix()
            return f"[{label}]({path}{separator}{anchor})"
        if not resolved.is_relative_to(ROOT) or not resolved.is_file():
            raise ValueError(f"Invalid repository source link in {source.name}: {target}")
        relative = resolved.relative_to(ROOT).as_posix()
        return f"[{label}]({SOURCE}{relative}{separator}{anchor})"

    return LINK.sub(rewrite, markdown)
