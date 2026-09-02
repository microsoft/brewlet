#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

missing=()
count=0
while IFS= read -r -d '' file; do
  ((count += 1))
  if ! grep -Fq 'Copyright (c) Microsoft Corporation.' "$file" ||
     ! grep -Fq 'Licensed under the MIT License.' "$file"; then
    missing+=("$file")
  fi
done < <(
  git ls-files -z -- \
    '*.go' '*.java' '*.py' '*.sh' '*.js' '*.css' '*.html' \
    '*.yaml' '*.yml' '*.xml' '*.svg' '*.tpl' \
    '*Dockerfile' '*Makefile'
)

if ((${#missing[@]} > 0)); then
  printf 'The following source files are missing the Microsoft MIT header:\n' >&2
  printf '  %s\n' "${missing[@]}" >&2
  exit 1
fi

printf 'Verified Microsoft MIT headers in %d source files.\n' "$count"
