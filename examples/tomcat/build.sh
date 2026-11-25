#!/usr/bin/env bash
set -euo pipefail
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
mvn -q package
cp target/ROOT.war build/payload/webapps/ROOT.war
brewlet build example.local/tomcat-war:1 \
  --platform "linux/$arch" \
  --launch launch.json \
  --layer build/payload/webapps:/app/webapps \
  --layer conf:/app/tomcat/conf \
  --store build/image \
  --result build/build.json
brewlet inspect example.local/tomcat-war:1 \
  --store build/image -o json > build/inspect.json
