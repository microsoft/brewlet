"""Keep repository-source links usable in built documentation without editing drafts."""

from pathlib import Path
import re

SOURCE = "https://github.com/microsoft/brewlet/blob/brewlet-2.0/"
ROOT = Path(__file__).resolve().parents[2]


def on_page_markdown(markdown, page, config, files):
    docs = Path(config["docs_dir"]).resolve()
    source = Path(page.file.abs_src_path)

    def rewrite(match):
        label, target = match.groups()
        if "://" in target or target.startswith(("#", "/")):
            return match.group(0)
        filename, separator, anchor = target.partition("#")
        resolved = (source.parent / filename).resolve()
        if resolved.is_relative_to(docs):
            return match.group(0)
        if not resolved.is_relative_to(ROOT) or not resolved.is_file():
            raise ValueError(f"Invalid repository source link in {source.name}: {target}")
        relative = resolved.relative_to(ROOT).as_posix()
        return f"[{label}]({SOURCE}{relative}{separator}{anchor})"

    return re.sub(r"\[([^\]]+)\]\(([^)]+)\)", rewrite, markdown)
