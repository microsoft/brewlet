"""Offline guards for preview links and strict documentation sources."""

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
        source = ROOT / "docs/index.md"
        original = source.read_text()
        rendered = HOOKS.on_page_markdown("[process policy](../pkg/node/process.go)", SimpleNamespace(
            file=SimpleNamespace(abs_src_path=str(source))), {"docs_dir": str(ROOT / "docs")}, None)
        self.assertIn("blob/main/pkg/node/process.go", rendered)
        self.assertNotIn("](../pkg/node/process.go)", rendered)
        self.assertEqual(source.read_text(), original)

    def test_invalid_source_link_is_not_silently_published(self):
        from types import SimpleNamespace
        for target in ("../not-a-real-source.go", "../../foreign-source.md"):
            with self.subTest(target=target), self.assertRaisesRegex(ValueError, "Invalid repository"):
                HOOKS.on_page_markdown(f"[source]({target})", SimpleNamespace(
                    file=SimpleNamespace(abs_src_path=str(ROOT / "docs/index.md"))),
                    {"docs_dir": str(ROOT / "docs")}, None)

    def test_site_is_assembly_not_production_deployment(self):
        source = (ROOT / "site/scripts/build.py").read_text()
        self.assertIn('"--strict"', source)
        self.assertNotIn("deploy-pages", source)

    def test_parser_preserves_fragment_and_relative_links(self):
        spec = importlib.util.spec_from_file_location("site_check", Path(__file__).with_name("check.py"))
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        page = module.Page('<a href="docs/#runtime">Runtime</a><h2 id="runtime">Runtime</h2>')
        self.assertEqual(page.links, [("a", "docs/#runtime")])
        self.assertEqual(page.ids, {"runtime"})

    def test_responsive_diagram_source_is_checked_as_a_local_link(self):
        spec = importlib.util.spec_from_file_location("site_check", Path(__file__).with_name("check.py"))
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        page = module.Page('<source srcset="assets/mobile.svg"><img src="assets/desktop.svg">')
        self.assertEqual(page.links, [("source", "assets/mobile.svg"), ("img", "assets/desktop.svg")])

    def test_landing_page_links_to_online_and_printable_cheat_sheets(self):
        spec = importlib.util.spec_from_file_location("site_check", Path(__file__).with_name("check.py"))
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        page = module.Page((ROOT / "site/index.html").read_text())
        self.assertIn("cheat-sheet", page.ids)
        self.assertIn(("a", "#cheat-sheet"), page.links)
        self.assertIn(("a", "docs/cheat-sheet/"), page.links)
        self.assertIn(("a", "docs/assets/cheat-sheet/brewlet-cheat-sheet.pdf"), page.links)
        self.assertIn(("img", "docs/assets/cheat-sheet/preview.png"), page.links)
        assets = ROOT / "docs/assets/cheat-sheet"
        self.assertTrue((assets / "brewlet-cheat-sheet.pdf").read_bytes().startswith(b"%PDF-"))
        self.assertTrue((assets / "preview.png").read_bytes().startswith(b"\x89PNG\r\n\x1a\n"))

    def test_printable_cheat_sheet_is_self_contained(self):
        spec = importlib.util.spec_from_file_location("site_check", Path(__file__).with_name("check.py"))
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        source = (ROOT / "docs/assets/cheat-sheet/print.html").read_text()
        page = module.Page(source)
        self.assertIn("size: A3 landscape", source)
        self.assertEqual({tag for tag, _ in page.links}, {"a"})
        self.assertTrue({"platform", "application", "operations"}.issubset(page.ids))


if __name__ == "__main__":
    unittest.main()
