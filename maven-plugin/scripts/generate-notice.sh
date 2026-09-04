#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

mode="${1:---check}"
case "$mode" in
  --check|--update) ;;
  *)
    echo "usage: $0 [--check|--update]" >&2
    exit 2
    ;;
esac

project_dir="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
output="$project_dir/src/main/resources/META-INF/NOTICE.txt"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
generated="$work/NOTICE.txt"

artifacts=(jackson-databind jackson-core jackson-annotations)
classpath="$work/jackson-classpath"
mvn -q dependency:build-classpath \
  -f "$project_dir/pom.xml" \
  -DincludeArtifactIds="$(IFS=,; echo "${artifacts[*]}")" \
  -Dmdep.outputFile="$classpath"
jars=()
for artifact in "${artifacts[@]}"; do
  jar="$(tr ':' '\n' < "$classpath" | grep -E "/$artifact-[^/]+\\.jar$" | head -n 1)"
  [[ -f "$jar" ]] || {
    echo "missing resolved $artifact JAR; run Maven dependency resolution first" >&2
    exit 1
  }
  jars+=("$jar")
done

versions=()
for jar in "${jars[@]}"; do
  versions+=("$(basename "$(dirname "$jar")")")
done

for jar in "${jars[@]:1}"; do
  cmp -s <(unzip -p "${jars[0]}" META-INF/LICENSE) <(unzip -p "$jar" META-INF/LICENSE) || {
    echo "Jackson artifacts no longer carry identical license text" >&2
    exit 1
  }
done

cat > "$generated" <<'EOF'
NOTICES AND INFORMATION
Do Not Translate or Localize

This software incorporates material from third parties. Microsoft makes certain
open source code available at https://3rdpartysource.microsoft.com, or you may
send a check or money order for US $5.00, including the product name, the open
source component name, and version number, to:

Source Code Compliance Team
Microsoft Corporation
One Microsoft Way
Redmond, WA 98052
USA

Notwithstanding any other terms, you may reverse engineer this software to the
extent required to debug changes to any libraries licensed under the GNU Lesser
General Public License.
EOF

{
  printf '\n================================================================================\n'
  printf 'Component: Jackson Databind %s, Jackson Core %s, and Jackson Annotations %s\n' \
    "${versions[0]}" "${versions[1]}" "${versions[2]}"
  printf 'Source: https://github.com/FasterXML\n'
  printf '\n--- LICENSE ---\n\n'
  unzip -p "${jars[0]}" META-INF/LICENSE
  printf '\n'
  for index in "${!artifacts[@]}"; do
    printf '\n--- %s NOTICE ---\n\n' "${artifacts[$index]}"
    unzip -p "${jars[$index]}" META-INF/NOTICE
    printf '\n'
  done
} >> "$generated"

cleaned="$work/NOTICE-clean.txt"
awk '
  {
    sub(/[[:space:]]+$/, "")
    lines[NR] = $0
    if ($0 != "") last = NR
  }
  END {
    for (line = 1; line <= last; line++) print lines[line]
  }
' "$generated" > "$cleaned"
mv "$cleaned" "$generated"

if [[ "$mode" == "--update" ]]; then
  mkdir -p "$(dirname "$output")"
  cp "$generated" "$output"
elif ! cmp -s "$generated" "$output"; then
  echo "$output is stale; run $0 --update" >&2
  diff -u "$output" "$generated" || true
  exit 1
fi
