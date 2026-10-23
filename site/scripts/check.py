#!/usr/bin/env python3
"""Check the assembled site's local links and anchors."""

from html.parser import HTMLParser
from pathlib import Path
import sys
from urllib.parse import unquote, urlsplit


class Page(HTMLParser):
    def __init__(self, text):
        super().__init__(convert_charrefs=True)
        self.links, self.ids = [], set()
        self.feed(text)

    def handle_starttag(self, tag, attrs):
        attributes = dict(attrs)
        if "id" in attributes:
            self.ids.add(attributes["id"])
        for key in ("href", "src"):
            if attributes.get(key):
                self.links.append((tag, attributes[key]))
        for source in attributes.get("srcset", "").split(","):
            if source.strip():
                self.links.append((tag, source.strip().split()[0]))


def check(root):
    root = root.resolve()
    errors = []
    pages = {path: Page(path.read_text()) for path in root.rglob("*.html")}
    for path, page in pages.items():
        for tag, target in page.links:
            url = urlsplit(target)
            if url.scheme or url.netloc:
                continue
            if url.path.startswith("/"):
                errors.append(f"{path.relative_to(root)}: root-relative link breaks preview paths: {target}")
                continue
            destination = (path.parent / unquote(url.path)).resolve() if url.path else path
            if not destination.is_relative_to(root):
                errors.append(f"{path.relative_to(root)}: escaping local link: {target}")
                continue
            if destination.is_dir():
                destination /= "index.html"
            if not destination.is_file():
                errors.append(f"{path.relative_to(root)}: missing {target}")
            elif url.fragment and destination in pages and unquote(url.fragment) not in pages[destination].ids:
                errors.append(f"{path.relative_to(root)}: missing anchor {target}")
    if errors:
        raise ValueError("\n".join(errors))
    return len(pages)


if __name__ == "__main__":
    root = Path(sys.argv[1]) if len(sys.argv) == 2 else Path(__file__).resolve().parents[1] / "_site"
    print(f"Site links passed: {check(root)} pages; local files and anchors resolve.")
