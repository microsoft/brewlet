"""Offline guards for preview links, strict documentation sources and theme contracts."""

import importlib.util
from pathlib import Path
import unittest

SPEC = importlib.util.spec_from_file_location("docs_hooks", Path(__file__).with_name("docs_hooks.py"))
HOOKS = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(HOOKS)
ROOT = Path(__file__).resolve().parents[2]


class SiteTests(unittest.TestCase):
    def test_source_links_become_branch_links_without_modifying_draft(self):
        from types import SimpleNamespace
        source = ROOT / "docs/brewlet-2-comparison.md"
        original = source.read_text()
        rendered = HOOKS.on_page_markdown(original, SimpleNamespace(
            file=SimpleNamespace(abs_src_path=str(source))), {"docs_dir": str(ROOT / "docs")}, None)
        self.assertIn("blob/brewlet-2.0/pkg/node/process.go", rendered)
        self.assertNotIn("](../pkg/node/process.go)", rendered)
        self.assertEqual(source.read_text(), original)

    def test_invalid_source_link_is_not_silently_published(self):
        from types import SimpleNamespace
        for target in ("../not-a-real-source.go", "../../foreign-source.md"):
            with self.subTest(target=target), self.assertRaisesRegex(ValueError, "Invalid repository"):
                HOOKS.on_page_markdown(f"[source]({target})", SimpleNamespace(
                    file=SimpleNamespace(abs_src_path=str(ROOT / "docs/index.md"))),
                    {"docs_dir": str(ROOT / "docs")}, None)

    def test_brand_surface_and_documentation_share_tokens(self):
        css = (ROOT / "site/assets/css/theme.css").read_text()
        self.assertIn('html[data-theme="dark"]', css)
        self.assertIn("--cp-accent:", css)
        self.assertIn("assets/css/theme.css", (ROOT / "site/index.html").read_text())
        self.assertIn("../assets/css/theme.css", (ROOT / "site/mkdocs.yml").read_text())

    def test_light_palette_matches_main_brand_colors(self):
        import re
        css = (ROOT / "site/assets/css/theme.css").read_text().split('html[data-theme="dark"]')[0]
        tokens = dict(re.findall(r"(--cp-[\w-]+):\s*(#[0-9a-f]+);", css))
        for token, value in {
            "--cp-bg": "#fffdf9",
            "--cp-bg-elevated": "#fbf4e6",
            "--cp-surface-soft": "#f5e6c8",
            "--cp-text": "#2a1f18",
            "--cp-text-muted": "#6b5d51",
            "--cp-border": "#e7dcc9",
            "--cp-accent": "#6f4e37",
            "--cp-accent-hover": "#553524",
            "--cp-link": "#326ce5",
            "--cp-link-hover": "#2350b8",
        }.items():
            with self.subTest(token=token):
                self.assertEqual(tokens[token], value)
        icon = (ROOT / "site/assets/images/favicon.svg").read_text()
        self.assertIn("--cp-accent:#6f4e37", icon)

    def test_site_is_assembly_not_production_deployment(self):
        source = (ROOT / "site/scripts/build.py").read_text()
        self.assertIn('"--strict"', source)
        self.assertNotIn("deploy-pages", source)
        self.assertFalse((ROOT / "site/CNAME").exists())

    def test_parser_preserves_fragment_and_relative_links(self):
        spec = importlib.util.spec_from_file_location("site_check", Path(__file__).with_name("check.py"))
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        page = module.Page('<a href="docs/#runtime">Runtime</a><h2 id="runtime">Runtime</h2>')
        self.assertEqual(page.links, [("a", "docs/#runtime")])
        self.assertEqual(page.ids, {"runtime"})


if __name__ == "__main__":
    unittest.main()
