"""Verify generated notices and the actual Maven JAR's packaged legal resources."""

from pathlib import Path
import json
import zipfile

from maven_notice import notices


ROOT = Path(__file__).resolve().parents[1]


def main():
    directory = ROOT / "maven-plugin/target"
    version = json.loads((ROOT / "release.json").read_text())["version"]
    jars = [path for path in directory.glob("brewlet-maven-plugin-*.jar")
            if path.name == f"brewlet-maven-plugin-{version}.jar"]
    if len(jars) != 1:
        raise ValueError("exactly one packaged Maven plugin JAR is required")
    expected = notices((directory / "runtime-classpath.txt").read_text())
    with zipfile.ZipFile(jars[0]) as jar:
        if jar.read("META-INF/LICENSE.txt") != (ROOT / "LICENSE.txt").read_bytes():
            raise ValueError("packaged Maven LICENSE differs from Brewlet license")
        if jar.read("META-INF/NOTICE.txt").decode() != expected:
            raise ValueError("packaged Maven NOTICE differs from resolved runtime dependencies")
    for architecture in ("amd64", "arm64"):
        for component in ("cli", "control", "worker"):
            path = ROOT / "bin/notices" / f"{component}-{architecture}.txt"
            if not path.is_file() or path.stat().st_size == 0:
                raise ValueError("missing generated Go notice: " + str(path))
            if f"Target: linux/{architecture}\n" not in path.read_text():
                raise ValueError("wrong notice target: " + str(path))
    print("Go target notices and Maven legal resources verified")


if __name__ == "__main__":
    main()
