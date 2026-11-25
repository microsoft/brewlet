#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "Usage: bash build.sh PATH_TO_WAR" >&2
  exit 1
fi
if [[ ! -f "$1" ]]; then
  echo "WAR file not found: $1" >&2
  exit 1
fi
war="$(cd "$(dirname "$1")" && pwd)/$(basename "$1")"
cd "$(dirname "$0")"

if ! command -v brewlet >/dev/null 2>&1; then
  echo "brewlet CLI not found on PATH" >&2
  exit 1
fi

case "$(docker info --format '{{.Architecture}}')" in
  arm64|aarch64) arch=arm64 ;;
  amd64|x86_64) arch=amd64 ;;
  *) echo "Unsupported Docker architecture" >&2; exit 1 ;;
esac

mkdir -p build/payload/webapps
cp "$war" build/payload/webapps/ROOT.war
brewlet build example.local/tomcat-hardened-war:1 \
  --platform "linux/$arch" \
  --launch platform/launch.json \
  --layer build/payload/webapps:/app/webapps \
  --store build/image \
  --result build/build.json
brewlet inspect example.local/tomcat-hardened-war:1 \
  --store build/image -o json > build/inspect.json
