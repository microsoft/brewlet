# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import tempfile
import unittest
from pathlib import Path
from xml.dom import minidom

from prepare_central_pom import child, children, prepare, release_profile, text


TEMPLATE = Path(__file__).resolve().parents[1] / "pom.xml"
TAGGED_POM = """<?xml version="1.0" encoding="UTF-8"?>
<!-- Copyright (c) Microsoft Corporation. Licensed under the MIT License. -->
<project xmlns="http://maven.apache.org/POM/4.0.0">
  <modelVersion>4.0.0</modelVersion>
  <groupId>sh.brewlet</groupId>
  <artifactId>brewlet-maven-plugin</artifactId>
  <version>0.1.0-SNAPSHOT</version>
  <packaging>maven-plugin</packaging>
  <properties><jackson.version>original-version</jackson.version></properties>
  <dependencies><dependency>
    <groupId>example</groupId><artifactId>original</artifactId><version>1.0</version>
  </dependency></dependencies>
  <build><plugins><plugin>
    <groupId>example</groupId><artifactId>original-build-plugin</artifactId>
  </plugin></plugins></build>
  <profiles><profile><id>existing-profile</id></profile></profiles>
</project>
"""


class PrepareCentralPomTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.target = Path(self.directory.name) / "pom.xml"
        self.target.write_text(TAGGED_POM)

    def project(self):
        return minidom.parse(str(self.target)).documentElement

    def test_backfill_preserves_tagged_build_and_adds_publishing_metadata(self):
        original = self.project()
        prepare(TEMPLATE, self.target)
        result = self.project()
        for name in ("version", "properties", "dependencies", "build"):
            self.assertEqual(child(original, name).toxml(), child(result, name).toxml())
        self.assertEqual(text(child(result, "developers").getElementsByTagName("name")[0]),
                         "Brewlet contributors")
        self.assertIsNotNone(child(result, "scm"))
        self.assertIsNotNone(release_profile(result))
        self.assertEqual(
            [text(child(profile, "id")) for profile in children(child(result, "profiles"), "profile")],
            ["existing-profile", "central-release"],
        )
        self.assertIn("Copyright (c) Microsoft Corporation", self.target.read_text())

    def test_adds_missing_profiles_container(self):
        self.target.write_text(TAGGED_POM.replace(
            "<profiles><profile><id>existing-profile</id></profile></profiles>", ""
        ))
        prepare(TEMPLATE, self.target)
        self.assertIsNotNone(release_profile(self.project()))

    def test_existing_metadata_is_preserved(self):
        self.target.write_text(TAGGED_POM.replace("</project>", """
  <developers><developer><name>Original maintainer</name></developer></developers>
  <scm><url>https://example.invalid/original</url></scm>
</project>"""))
        original = self.project()
        prepare(TEMPLATE, self.target)
        for name in ("developers", "scm"):
            self.assertEqual(child(original, name).toxml(), child(self.project(), name).toxml())

    def test_replaces_old_release_profile_and_is_idempotent(self):
        self.target.write_text(TAGGED_POM.replace("</profiles>", """
<profile><id>central-release</id><properties><old>value</old></properties></profile>
</profiles>"""))
        prepare(TEMPLATE, self.target)
        result = self.target.read_bytes()
        self.assertNotIn(b"<old>", result)
        prepare(TEMPLATE, self.target)
        self.assertEqual(result, self.target.read_bytes())

    def test_rejects_wrong_coordinates_without_writing(self):
        self.target.write_text(TAGGED_POM.replace("<groupId>sh.brewlet</groupId>",
                                                 "<groupId>other</groupId>"))
        original = self.target.read_bytes()
        with self.assertRaisesRegex(ValueError, "expected groupId"):
            prepare(TEMPLATE, self.target)
        self.assertEqual(original, self.target.read_bytes())

    def test_rejects_missing_template_profile_without_writing(self):
        template = Path(self.directory.name) / "template.xml"
        template.write_text(TAGGED_POM)
        with self.assertRaisesRegex(ValueError, "no central-release profile"):
            prepare(template, self.target)
        self.assertEqual(TAGGED_POM, self.target.read_text())

    def test_rejects_duplicate_release_profiles_without_writing(self):
        self.target.write_text(TAGGED_POM.replace("</profiles>", """
<profile><id>central-release</id></profile>
<profile><id>central-release</id></profile>
</profiles>"""))
        original = self.target.read_bytes()
        with self.assertRaisesRegex(ValueError, "Multiple central-release profiles"):
            prepare(TEMPLATE, self.target)
        self.assertEqual(original, self.target.read_bytes())

    def test_profile_defaults_to_validation_and_allows_explicit_publish(self):
        project = minidom.parse(str(TEMPLATE)).documentElement
        profile = release_profile(project)
        properties = child(profile, "properties")
        self.assertEqual(text(child(properties, "central.autoPublish")), "false")
        self.assertEqual(text(child(properties, "central.waitUntil")), "validated")
        plugins = children(child(child(profile, "build"), "plugins"), "plugin")
        publisher = next(plugin for plugin in plugins
                         if text(child(plugin, "artifactId")) == "central-publishing-maven-plugin")
        configuration = child(publisher, "configuration")
        self.assertEqual(text(child(configuration, "autoPublish")), "${central.autoPublish}")
        self.assertEqual(text(child(configuration, "waitUntil")), "${central.waitUntil}")


if __name__ == "__main__":
    unittest.main()
