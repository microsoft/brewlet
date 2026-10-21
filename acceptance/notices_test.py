import importlib.util
from pathlib import Path
import tempfile
import unittest
import zipfile

ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location("maven_notice", ROOT / "scripts/maven_notice.py")
MAVEN = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MAVEN)


class NoticeTests(unittest.TestCase):
    def test_runtime_jars_use_their_actual_independent_versions_and_notice_text(self):
        import os
        with tempfile.TemporaryDirectory() as directory:
            jars = []
            for name in ("jackson-core-2.22.0.jar", "jackson-databind-2.22.3.jar"):
                path = Path(directory) / name
                with zipfile.ZipFile(path, "w") as jar:
                    jar.writestr("META-INF/LICENSE", "synthetic license " + name)
                    jar.writestr("META-INF/NOTICE", "synthetic attribution " + name)
                jars.append(str(path))
            result = MAVEN.notices(os.pathsep.join(jars))
            for path in jars:
                self.assertIn("synthetic license " + Path(path).name, result)
                self.assertIn("synthetic attribution " + Path(path).name, result)

    def test_missing_and_empty_license_fail_closed(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "fixture.jar"
            for entries in ({}, {"META-INF/NOTICE": "attribution"}, {"META-INF/LICENSE": ""}):
                with zipfile.ZipFile(path, "w") as jar:
                    for name, text in entries.items():
                        jar.writestr(name, text)
                with self.subTest(entries=entries), self.assertRaises(ValueError):
                    MAVEN.notices(str(path))

    def test_both_container_final_stages_ship_notices_and_own_license(self):
        for name in ("kubernetes", "provisioner"):
            dockerfile = (ROOT / name / "Dockerfile").read_text()
            final = dockerfile.rsplit("\nFROM ", 1)[1]
            self.assertIn(" /LICENSE.txt", final)
            self.assertIn(" /NOTICE.txt", final)
            self.assertIn("go run ./scripts/notice", dockerfile)


if __name__ == "__main__":
    unittest.main()
