"""Install only named, hash-pinned release tools into an explicit tools directory."""

import argparse
import hashlib
import io
import platform
from pathlib import Path
import tarfile
import urllib.request

TOOLS = {
    "syft": ("https://github.com/anchore/syft/releases/download/v1.55.0/syft_1.55.0_{system}_{arch}.tar.gz", {
        "amd64": "bb65249542db5fedeeb4d3f9b018b75e7264dbfbbb10486f044581a92f90490d",
        "arm64": "929bd163db2d7cd2255d3bcb8967208079813d7b46b0c952f7b794d8e2c7d304",
        "darwin-amd64": "e2e5831cbabaec9a36fe65c34755c1113982c842a685edce3ce0b54a0ded8003",
        "darwin-arm64": "1217c258bd5eb91ef3189cf22a82e02f9cac07e4ce934350f428e93aff674625"}),
    "oras": ("https://github.com/oras-project/oras/releases/download/v1.3.5/oras_1.3.5_{system}_{arch}.tar.gz", {
        "amd64": "f48a73ea6d026a862c3919fc773da9a8c86fc259c2c737d5baed3b8b9614ad22",
        "arm64": "d5d4e5afd5c0781c93eda5ea1976e654e1fdee202c1ddaeaf5f91a8c4fd67d46",
        "darwin-amd64": "0e8a0b4b0bb7179442097152f8b49d7cb22d15f023371a2d30b40914d9ba3ac3",
        "darwin-arm64": "bd164bc9d08e9c5edb918f76e0cf92ecc61f6a50a4a6709b1cb06cf6721b4610"}),
    "ghx": ("https://github.com/brunoborges/ghx/releases/download/v1.6.0/ghx-{system}-{arch}.tar.gz", {
        "amd64": "f3af903ac6a4bb3d4f5066c070c0fb38edcdef15cdf23ba93b41b1bbeed381b5",
        "arm64": "628d40cbe99a369be5c27ace03fa59bd0ae20575a17314fa2eebc57cc4e5da62",
        "darwin-amd64": "24dbc39fc415989eb06caf0bb7598c8f9248266179d02f731de42bb6d4298d9d",
        "darwin-arm64": "bc416f4a896dc180ff78113cc3de64c6c1dcf997c41bf2bcd359e9236ff0b3c5"}),
}


def install(name, destination):
    system = {"Linux": "linux", "Darwin": "darwin"}.get(platform.system())
    if system is None:
        raise ValueError("release tool bootstrap supports Linux and macOS only")
    arch = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine())
    url, hashes = TOOLS[name]
    key = arch if system == "linux" else system + "-" + str(arch)
    if key not in hashes:
        raise ValueError("unsupported tool architecture")
    with urllib.request.urlopen(url.format(arch=arch, system=system), timeout=120) as response:
        data = response.read()
    if hashlib.sha256(data).hexdigest() != hashes[key]:
        raise ValueError("release tool checksum mismatch: " + name)
    with tarfile.open(fileobj=io.BytesIO(data)) as archive:
        matches = [item for item in archive if item.isfile() and item.name.removeprefix("./") == name]
        if len(matches) != 1:
            raise ValueError("missing/duplicate release tool binary: " + name)
        binary = archive.extractfile(matches[0]).read()
    destination.mkdir(parents=True, exist_ok=True)
    path = destination / name
    if path.exists():
        raise ValueError("refusing to replace an existing tool: " + str(path))
    path.write_bytes(binary)
    path.chmod(0o755)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--destination", type=Path, required=True)
    parser.add_argument("tools", choices=TOOLS, nargs="+")
    args = parser.parse_args()
    for tool in args.tools:
        install(tool, args.destination.resolve())
