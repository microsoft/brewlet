"""Offline guards for preview links and strict documentation sources."""

import importlib.util
from pathlib import Path
import unittest
import unittest.mock

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

    def test_specification_links_use_the_rendered_page(self):
        from types import SimpleNamespace
        docs = {"docs_dir": str(ROOT / "docs")}
        guide = HOOKS.on_page_markdown("[§16](../specs/SPECIFICATION.md#part-ii--composition)",
            SimpleNamespace(file=SimpleNamespace(abs_src_path=str(ROOT / "docs/index.md"),
                                                 src_uri="index.md")), docs, None)
        self.assertEqual(guide, "[§16](specification.md#part-ii-composition)")
        page = SimpleNamespace(file=SimpleNamespace(abs_src_path=str(HOOKS.SPECIFICATION),
                                                    src_uri=HOOKS.SPECIFICATION_PAGE))
        specification = HOOKS.on_page_markdown(
            "[Part I](#part-i--runtime) [guide](../docs/java.md#build) "
            "[ledger](../release.json) `[a-z]([-a-z]*)`", page, docs, None)
        self.assertEqual(specification, "[Part I](#part-i-runtime) [guide](java.md#build) "
                         f"[ledger]({HOOKS.SOURCE}release.json) `[a-z]([-a-z]*)`")
        self.assertEqual(page.edit_url, f"{HOOKS.EDIT}specs/SPECIFICATION.md")

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
        self.assertIn(("a", "docs/native/"), page.links)
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

    def test_nested_main_docs_keep_site_root_links(self):
        from types import SimpleNamespace
        config = SimpleNamespace(site_url="https://brewlet.sh/docs/", extra={"homepage": "../"},
                                 theme={"favicon": "../assets/images/favicon.svg"},
                                 extra_css=["../assets/css/theme.css", "stylesheets/brewlet-docs.css"])
        with unittest.mock.patch.dict("os.environ", {"BREWLET_DOCS_SUBDIR": "main"}):
            HOOKS.on_config(config)
        self.assertEqual(config.site_url, "https://brewlet.sh/docs/main/")
        self.assertEqual(config.extra["homepage"], "../../")
        self.assertEqual(config.theme["favicon"], "../../assets/images/favicon.svg")
        self.assertEqual(config.extra_css, ["../../assets/css/theme.css", "stylesheets/brewlet-docs.css"])

    def test_version_selector_maps_pages_and_falls_back_to_docs_home(self):
        import tempfile
        build = load("build")
        header = '<html><head></head><body><header class="md-header"><label class="md-header__button md-icon" for="__search"></label></header></body></html>'
        with tempfile.TemporaryDirectory() as temp:
            output = Path(temp)
            docs = output / "docs"
            for name in ("index.html", "java/index.html", "old/index.html", "404.html",
                         "main/index.html", "main/java/index.html", "main/new/index.html"):
                (docs / name).parent.mkdir(parents=True, exist_ok=True)
                (docs / name).write_text(header)
            (docs / "main/print.html").write_text("<html><head></head><body>standalone</body></html>")
            with unittest.mock.patch.multiple(build, OUTPUT=output, DOCS=docs):
                build.add_version_selector("v1.2.3")
            release_java = (docs / "java/index.html").read_text()
            self.assertIn('<a href="./" aria-current="page">Latest release (v1.2.3)</a>', release_java)
            self.assertIn('<a href="../main/java/">Development (main)</a>', release_java)
            self.assertIn('href="../../assets/css/docs-version.css"', release_java)
            self.assertNotIn("brewlet-version-banner", release_java)
            self.assertIn('<a href="../main/">Development (main)</a>', (docs / "old/index.html").read_text())
            main_new = (docs / "main/new/index.html").read_text()
            self.assertIn('<a href="../../">Latest release (v1.2.3)</a>', main_new)
            self.assertIn('Switch to the latest release (v1.2.3)', main_new)
            self.assertNotIn("brewlet-version", (docs / "404.html").read_text())
            self.assertNotIn("brewlet-version", (docs / "main/print.html").read_text())

    def test_release_tag_must_be_a_release_version(self):
        build = load("build")
        for ref in ("main", "v1.2", "v1.2.3;rm"):
            with self.subTest(ref=ref), self.assertRaisesRegex(SystemExit, "Invalid release tag"):
                build.release_ref(ref)


def load(name):
    spec = importlib.util.spec_from_file_location(f"site_{name}", Path(__file__).with_name(f"{name}.py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


if __name__ == "__main__":
    unittest.main()
