"""Offline guards for preview links, strict documentation sources and theme contracts."""

import importlib.util
from pathlib import Path
import unittest

SPEC = importlib.util.spec_from_file_location("docs_hooks", Path(__file__).with_name("docs_hooks.py"))
HOOKS = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(HOOKS)
ROOT = Path(__file__).resolve().parents[2]


class SiteTests(unittest.TestCase):
    def test_current_branding_matches_release_identity(self):
        import json
        version = json.loads((ROOT / "release.json").read_text())["version"]
        self.assertEqual(version, "1.0.0")
        landing = (ROOT / "site/index.html").read_text()
        self.assertEqual(landing.count(f'<span class="version">{version}</span>'), 2)
        self.assertIn(f"<title>Brewlet {version} ", landing)
        self.assertIn(f"site_name: Brewlet {version}\n", (ROOT / "site/mkdocs.yml").read_text())
        comparison = (ROOT / "docs/legacy-comparison.md").read_text()
        self.assertIn(f"# Brewlet 0.x vs. Brewlet {version}:", comparison)
        self.assertIn('href="docs/legacy-comparison/"', landing)
        for name in ("404.html", "main.html"):
            self.assertIn(f"Brewlet {version}", (ROOT / "site/overrides" / name).read_text())
        for name in ("architecture-desktop.svg", "architecture-mobile.svg"):
            self.assertIn(f"Brewlet {version}", (ROOT / "site/assets/images" / name).read_text())

    def test_source_links_become_branch_links_without_modifying_draft(self):
        from types import SimpleNamespace
        source = ROOT / "docs/legacy-comparison.md"
        original = source.read_text()
        rendered = HOOKS.on_page_markdown(original, SimpleNamespace(
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

    def test_brand_surface_and_documentation_share_tokens(self):
        css = (ROOT / "site/assets/css/theme.css").read_text()
        self.assertIn('html[data-theme="dark"]', css)
        self.assertIn("--cp-accent:", css)
        self.assertIn("assets/css/theme.css", (ROOT / "site/index.html").read_text())
        self.assertIn("../assets/css/theme.css", (ROOT / "site/mkdocs.yml").read_text())

    def test_runtime_is_infrastructure_not_a_second_workload_owner(self):
        landing = (ROOT / "site/index.html").read_text()
        overview = (ROOT / "docs/index.md").read_text()
        for text in (landing, overview):
            self.assertNotIn("Two owners", text)
            self.assertNotIn("Developer owns", text)
            self.assertNotIn("Platform team owns", text)
        self.assertIn("Your application is the workload.", landing)
        self.assertIn("The runtime is infrastructure.", landing)
        self.assertIn("like the OS or Kubernetes", landing)
        self.assertIn("| Application workload | Runtime infrastructure |", overview)

    def test_wasm_entry_point_preserves_runc_and_explicit_qualification_scope(self):
        landing = (ROOT / "site/index.html").read_text()
        guide = (ROOT / "docs/webassembly.md").read_text()
        self.assertIn('href="docs/webassembly/"', landing)
        self.assertIn("does not install runwasi", guide)
        self.assertIn("runtimeImages", guide)
        self.assertIn("native arm64", guide)
        self.assertIn("XDG_CACHE_HOME=/cache/compiled", guide)

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

    def test_architecture_diagrams_preserve_generic_execution_boundaries(self):
        import json
        from xml.etree import ElementTree
        images = ROOT / "site/assets/images"
        for name in ("architecture-desktop.svg", "architecture-mobile.svg"):
            with self.subTest(diagram=name):
                svg = ElementTree.fromstring((images / name).read_text())
                text = " ".join(svg.itertext())
                ids = [element.attrib["id"] for element in svg.iter() if "id" in element.attrib]
                self.assertEqual(len(ids), len(set(ids)))
                for required in ("RuntimeImage", "NodeProfile", "containerd", "Brewlet shim",
                                 "Fenced provisioner", "runc", "generation"):
                    self.assertIn(required, text)
                for forbidden in ("JavaApplication", "java -jar", "/opt/jdk", "Two owners"):
                    self.assertNotIn(forbidden, text)
                for node in ("worker", "containerd", "provisioner", "runtime-root", "shim", "sandbox"):
                    self.assertIn(node, ids)
        source = json.loads((images / "architecture.excalidraw").read_text())
        self.assertEqual(source["type"], "excalidraw")
        source_ids = [element["id"] for element in source["elements"]]
        self.assertEqual(len(source_ids), len(set(source_ids)))
        for element in source["elements"]:
            if element["type"] == "text":
                self.assertGreater(element["width"], 0)
                self.assertGreater(element["height"], 0)
                self.assertEqual(element["strokeColor"], "#000000")
        worker = next(element for element in source["elements"] if element["id"] == "worker")
        self.assertEqual(worker["backgroundColor"], "transparent")

    def test_responsive_diagram_source_is_checked_as_a_local_link(self):
        spec = importlib.util.spec_from_file_location("site_check", Path(__file__).with_name("check.py"))
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        page = module.Page('<source srcset="assets/mobile.svg"><img src="assets/desktop.svg">')
        self.assertEqual(page.links, [("source", "assets/mobile.svg"), ("img", "assets/desktop.svg")])


if __name__ == "__main__":
    unittest.main()
