#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Enforces the workflow supply-chain policy:
#
#   1. Every `uses:` reference resolves to an immutable full commit SHA and
#      carries a trailing version comment so humans can still read the pin.
#   2. No workflow grants a `write` permission at workflow scope; write scopes
#      must be declared on the individual jobs that publish.
#
# Both rules block the tag-moving attack path described in SECURITY-REVIEW.md
# finding 11.

set -euo pipefail

failures=0

fail() {
  echo "$1" >&2
  failures=$((failures + 1))
}

# Strips a trailing YAML comment from a scalar, honouring single and double
# quotes so that a `#` inside a quoted value is not treated as a comment.
split_comment() {
  local text="$1"
  local index=0
  local char quote=""
  value=""
  comment=""

  while ((index < ${#text})); do
    char="${text:index:1}"
    if [[ -n "$quote" ]]; then
      [[ "$char" == "$quote" ]] && quote=""
    elif [[ "$char" == '"' || "$char" == "'" ]]; then
      quote="$char"
    elif [[ "$char" == "#" ]]; then
      if ((index == 0)) || [[ "${text:index-1:1}" == [[:space:]] ]]; then
        comment="${text:index}"
        break
      fi
    fi
    value+="$char"
    index=$((index + 1))
  done

  # Trim surrounding whitespace and matching quotes from the value.
  value="${value#"${value%%[![:space:]]*}"}"
  value="${value%"${value##*[![:space:]]}"}"
  if [[ ${#value} -ge 2 ]]; then
    if [[ "$value" == \"*\" || "$value" == \'*\' ]]; then
      value="${value:1:${#value}-2}"
    fi
  fi
}

check_uses() {
  local workflow="$1"
  local line="$2"
  local raw="$3"
  local value comment first_word

  split_comment "$raw"

  if [[ -z "$value" ]]; then
    fail "$workflow:$line: empty uses: reference"
    return
  fi

  # Reusable workflows and composite actions inside this repository are
  # already immutable: they are read from the checked-out commit.
  if [[ "$value" == ./* || "$value" == .github/* ]]; then
    return
  fi

  if [[ "$value" == docker://* ]]; then
    fail "$workflow:$line: docker:// action references are not permitted: $value"
    return
  fi

  if ! [[ "$value" =~ ^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+(/[A-Za-z0-9._/-]+)?@[0-9a-f]{40}$ ]]; then
    fail "$workflow:$line: uses: must pin a full 40-character commit SHA: $value"
    return
  fi

  if [[ -z "$comment" ]]; then
    fail "$workflow:$line: SHA-pinned uses: must carry a trailing version comment: $value"
    return
  fi

  first_word="${comment#\#}"
  first_word="${first_word#"${first_word%%[![:space:]]*}"}"
  first_word="${first_word%%[[:space:]]*}"
  if ! [[ "$first_word" =~ ^v?[0-9][0-9A-Za-z.+-]*$ ]]; then
    fail "$workflow:$line: version comment must start with the pinned version: $comment"
  fi
}

# Emits `<line>|<scope>|<value>` for every permission entry that belongs to the
# top-level `permissions:` mapping; `<scope>` is empty for the inline shorthand.
# A non-whitespace separator keeps empty fields addressable in `read`.
# Job-scoped permissions are indented below `jobs:` and are deliberately ignored.
top_level_permissions() {
  awk '
    function trim(text) {
      sub(/^[[:space:]]+/, "", text)
      sub(/[[:space:]]+$/, "", text)
      return text
    }
    /^[[:space:]]*(#|$)/ { next }
    /^[^[:space:]#]/ {
      key = $0
      sub(/:.*$/, "", key)
      if (key == "permissions") {
        in_block = 1
        rest = $0
        sub(/^[^:]*:/, "", rest)
        rest = trim(rest)
        sub(/[[:space:]]+#.*$/, "", rest)
        if (rest != "" && rest != "{}") {
          print NR "|" "" "|" rest
          in_block = 0
        }
      } else {
        in_block = 0
      }
      next
    }
    in_block {
      entry = trim($0)
      sub(/[[:space:]]+#.*$/, "", entry)
      if (entry == "") next
      scope = entry
      sub(/:.*$/, "", scope)
      value = entry
      sub(/^[^:]*:/, "", value)
      print NR "|" trim(scope) "|" trim(value)
    }
  ' "$1"
}

check_permissions() {
  local workflow="$1"
  local line scope value found=0

  if ! grep -Eq '^permissions:' "$workflow"; then
    fail "$workflow:1: workflow must declare a top-level permissions: block"
    return
  fi

  while IFS='|' read -r line scope value; do
    [[ -n "$line" ]] || continue
    found=1
    if [[ -z "$scope" ]]; then
      if [[ "$value" != "read-all" ]]; then
        fail "$workflow:$line: workflow-scope permissions must be read-only: $value"
      fi
      continue
    fi
    if [[ "$value" == "write" ]]; then
      fail "$workflow:$line: '$scope: write' must be scoped to the job that needs it"
    fi
  done < <(top_level_permissions "$workflow")

  if ((found == 0)); then
    fail "$workflow:1: top-level permissions: block is empty"
  fi
}

check_workflow() {
  local workflow="$1"
  local line raw

  while IFS=$'\t' read -r line raw; do
    check_uses "$workflow" "$line" "$raw"
  done < <(
    grep -nE '^[[:space:]]*(-[[:space:]]+)?uses:[[:space:]]*[^[:space:]]' "$workflow" |
      sed -E 's/^([0-9]+):[[:space:]]*(-[[:space:]]+)?uses:[[:space:]]*/\1\t/'
  )

  check_permissions "$workflow"
}

workflows=()
if (($# > 0)); then
  workflows=("$@")
else
  while IFS= read -r -d '' workflow; do
    workflows+=("$workflow")
  done < <(find .github/workflows -maxdepth 1 -type f \
    \( -name '*.yml' -o -name '*.yaml' \) -print0 2>/dev/null | sort -z)
fi

if ((${#workflows[@]} == 0)); then
  echo "no workflows found" >&2
  exit 1
fi

for workflow in "${workflows[@]}"; do
  check_workflow "$workflow"
done

if ((failures > 0)); then
  exit 1
fi

printf 'Verified workflow supply-chain policy in %d workflows.\n' "${#workflows[@]}"
