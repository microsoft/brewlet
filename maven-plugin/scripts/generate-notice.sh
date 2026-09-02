#!/usr/bin/env bash
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
version="$(sed -n 's:.*<jackson.version>\([^<]*\)</jackson.version>.*:\1:p' "$project_dir/pom.xml")"
repository="${MAVEN_REPO_LOCAL:-$HOME/.m2/repository}"
output="$project_dir/src/main/resources/META-INF/NOTICE.txt"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
generated="$work/NOTICE.txt"

artifacts=(jackson-databind jackson-core jackson-annotations)
jars=()
for artifact in "${artifacts[@]}"; do
  jar="$repository/com/fasterxml/jackson/core/$artifact/$version/$artifact-$version.jar"
  [[ -f "$jar" ]] || {
    echo "missing $jar; run Maven dependency resolution first" >&2
    exit 1
  }
  jars+=("$jar")
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
  printf 'Component: Jackson Databind, Jackson Core, and Jackson Annotations %s\n' "$version"
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
