"""Execute the tutorial's exact shell blocks only with an explicit --run."""

import argparse
import os
from pathlib import Path
import re
import signal
import subprocess

ROOT = Path(__file__).resolve().parents[2]
PAGE = ROOT / "docs/local-kubernetes.md"


def steps():
    blocks = re.findall(r"```sh\n(# tutorial-step: [\s\S]*?)\n```", PAGE.read_text())
    names = [block.splitlines()[0].removeprefix("# tutorial-step: ") for block in blocks]
    expected = ["cli", "setup", "build", "cluster", "install", "application",
                "rotation", "replacement", "cleanup"]
    if names != expected:
        raise ValueError("tutorial steps missing, reordered or duplicated: " + repr(names))
    return blocks


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run", action="store_true", help="create one private disposable cluster and execute the guide")
    args = parser.parse_args()
    script = "\n\n".join(steps()) + "\n"
    subprocess.run(["bash", "-n"], input=script, text=True, check=True)
    if args.run:
        process = subprocess.Popen(["bash", "-eu", "-o", "pipefail"],
                                   stdin=subprocess.PIPE, text=True, cwd=ROOT,
                                   start_new_session=True)
        try:
            process.communicate(script, timeout=3600)
        finally:
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGTERM)
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait(timeout=5)
        if process.returncode:
            raise subprocess.CalledProcessError(process.returncode, process.args)
    else:
        print("Tutorial shell and step structure valid; no cluster executed.")


if __name__ == "__main__":
    main()
