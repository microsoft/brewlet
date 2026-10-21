"""Collect actual runtime JAR license/notice resources; never assume aligned versions."""

import argparse
from pathlib import Path
import os
import zipfile


def notices(classpath):
    parts = ["Brewlet Maven plugin third-party notices\n"]
    jars = sorted({Path(value) for value in classpath.strip().split(os.pathsep) if value})
    if not jars:
        raise ValueError("runtime dependency classpath is empty")
    for path in jars:
        with zipfile.ZipFile(path) as jar:
            names = sorted(name for name in jar.namelist() if not name.endswith("/")
                           and name.upper().startswith("META-INF/")
                           and Path(name).name.upper().startswith(("LICENSE", "NOTICE", "COPYING")))
            if not any(Path(name).name.upper().startswith(("LICENSE", "COPYING")) for name in names):
                raise ValueError("runtime JAR has no license: " + path.name)
            parts.append("\n" + path.name + "\n")
            for name in names:
                text = jar.read(name).decode("utf-8")
                if not text.strip():
                    raise ValueError("empty license/notice: " + path.name + ":" + name)
                parts.append("--- " + name + " ---\n" + text + "\n")
    return "\n".join(parts)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("classpath", type=Path)
    parser.add_argument("output", type=Path)
    parser.add_argument("license", type=Path)
    args = parser.parse_args()
    text = notices(args.classpath.read_text())
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(text)
    args.output.with_name("LICENSE.txt").write_bytes(args.license.read_bytes())


if __name__ == "__main__":
    main()
