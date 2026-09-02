#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

if [[ $# -lt 3 ]]; then
  echo "usage: $0 <module-directory> <output-file> <package> [<package> ...]" >&2
  exit 2
fi

module_dir="$1"
output="$2"
shift 2

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

modules="$work/modules"
for package in "$@"; do
  go -C "$module_dir" list -deps \
    -f '{{if and (not .Standard) .Module}}{{if not .Module.Main}}{{.Module.Path}}	{{.Module.Version}}	{{if .Module.Replace}}{{.Module.Replace.Dir}}{{else}}{{.Module.Dir}}{{end}}{{end}}{{end}}' \
    "$package"
done | awk 'NF' | LC_ALL=C sort -u > "$modules"

mkdir -p "$(dirname "$output")"
cat > "$output" <<'EOF'
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

append_component() {
  local name="$1"
  local source="$2"
  shift 2

  {
    printf '\n================================================================================\n'
    printf 'Component: %s\n' "$name"
    printf 'Source: %s\n' "$source"
  } >> "$output"

  local file
  for file in "$@"; do
    {
      printf '\n--- %s ---\n\n' "$(basename "$file")"
      cat "$file"
      printf '\n'
    } >> "$output"
  done
}

goroot="$(go env GOROOT)"
go_license="$goroot/LICENSE"
[[ -f "$go_license" ]] || go_license="$(dirname "$goroot")/LICENSE"
[[ -f "$go_license" ]] || {
  echo "Go toolchain LICENSE not found under $goroot" >&2
  exit 1
}
go_files=("$go_license")
[[ -f "$goroot/PATENTS" ]] && go_files+=("$goroot/PATENTS")
append_component "Go standard library $(go env GOVERSION)" \
  "https://go.dev/" "${go_files[@]}"

while IFS=$'\t' read -r path version directory; do
  [[ -n "$path" && -n "$directory" ]] || {
    echo "incomplete module metadata for $path $version" >&2
    exit 1
  }

  files=()
  while IFS= read -r file; do
    files+=("$file")
  done < <(
    find "$directory" -maxdepth 1 -type f \
      \( -iname 'LICENSE*' -o -iname 'COPYING*' -o -iname 'NOTICE*' \
         -o -iname 'PATENTS*' -o -iname 'AUTHORS*' -o -iname 'CONTRIBUTORS*' \) \
      -print | LC_ALL=C sort
  )

  if [[ ${#files[@]} -eq 0 ]]; then
    echo "no attribution files found for $path $version in $directory" >&2
    exit 1
  fi

  append_component "$path $version" "https://$path" "${files[@]}"
done < "$modules"
