#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

normalize_dockerfile() {
  awk '
    function emit() {
      if (buffer != "") {
        gsub(/[[:space:]]+/, " ", buffer)
        print start_line "\t" buffer
      }
      buffer = ""
    }
    function begin_heredoc(text, search, marker, candidate) {
      if (toupper(substr(text, 1, 3)) != "RUN") {
        return 0
      }
      search = text
      gsub(/"/, "", search)
      gsub(/\047/, "", search)
      marker = ""
      while (match(search, /<<-?[A-Za-z0-9_.-]+/)) {
        candidate = substr(search, RSTART, RLENGTH)
        marker = candidate
        search = substr(search, RSTART + RLENGTH)
      }
      if (marker == "") {
        return 0
      }
      heredoc_strip_tabs = marker ~ /^<<-/
      sub(/^<<-?/, "", marker)
      heredoc = marker
      return heredoc != ""
    }
    {
      line = $0
      sub(/\r$/, "", line)
      if (heredoc != "") {
        terminator = line
        if (heredoc_strip_tabs) {
          sub(/^\t+/, "", terminator)
        }
        sub(/^[[:space:]]+/, "", terminator)
        sub(/[[:space:]]+$/, "", terminator)
        if (terminator == heredoc) {
          heredoc = ""
          emit()
          next
        }
        buffer = buffer " " line
        next
      }
      if (buffer == "" && line ~ /^[[:space:]]*(#.*)?$/) {
        next
      }
      if (buffer != "" && line ~ /^[[:space:]]*#/) {
        next
      }
      if (buffer == "") {
        start_line = NR
        sub(/^[[:space:]]+/, "", line)
      }
      continued = line ~ /\\[[:space:]]*$/
      sub(/\\[[:space:]]*$/, "", line)
      if (line !~ /^[[:space:]]*#/) {
        buffer = buffer line
      }
      if (!continued) {
        if (!begin_heredoc(buffer)) {
          emit()
        }
      }
    }
    END {
      emit()
    }
  ' "$1"
}

is_stage_alias() {
  local candidate="$1"
  local alias
  for alias in "${stage_aliases[@]}"; do
    [[ -n "$alias" ]] || continue
    [[ "$candidate" != "$alias" ]] || return 0
  done
  return 1
}

check_image_reference() {
  local dockerfile="$1"
  local line="$2"
  local image="$3"
  local context="$4"

  if [[ "$image" == "scratch" ]] ||
    [[ "$image" =~ ^[0-9]+$ ]] ||
    is_stage_alias "$image"; then
    return
  fi
  if ! printf '%s\n' "$image" | grep -Eq '@sha256:[0-9a-f]{64}$'; then
    echo "$dockerfile:$line: $context must use a full sha256 digest: $image" >&2
    failures=$((failures + 1))
  fi
}

run_uses_direct_downloader() {
  local command_text="$1"
  local segments segment normalized word basename found

  segments="${command_text//&&/$'\n'}"
  segments="${segments//||/$'\n'}"
  segments="${segments//&/$'\n'}"
  segments="${segments//;/$'\n'}"
  segments="${segments//|/$'\n'}"

  while IFS= read -r segment; do
    normalized="$(
      printf '%s\n' "$segment" |
        tr -d '"' |
        tr -d "'" |
        tr -d '\\' |
        tr '[],()' '     '
    )"
    found=false
    for word in $normalized; do
      basename="${word##*/}"
      if [[ "$basename" == "curl" || "$basename" == "wget" ]]; then
        found=true
        break
      fi
    done
    [[ "$found" == "true" ]] || continue

    if [[ "$segment" != *'$'* && "$segment" != *'`'* &&
      "$segment" != *'('* && "$segment" != *')'* ]] &&
      printf '%s\n' "$normalized" |
        grep -Eiq '^[[:space:]]*(--[^[:space:]]+[[:space:]]+)*(if[[:space:]]+)?(sudo[[:space:]]+)?((apt-get|apt|dnf|yum|microdnf)[[:space:]]+([^[:space:]]+[[:space:]]+)*install|apk[[:space:]]+([^[:space:]]+[[:space:]]+)*add)([[:space:]]|$)'; then
      continue
    fi
    return 0
  done <<<"$segments"

  return 1
}

failures=0
check_dockerfile() {
  local dockerfile="$1"
  local line instruction keyword rest image index token command_text source mount option
  local -a words mount_options
  stage_aliases=("")

  if grep -Eiq '^[[:space:]]*#[[:space:]]*escape=' "$dockerfile"; then
    echo "$dockerfile:1: non-default Dockerfile escape directives are forbidden" >&2
    failures=$((failures + 1))
  fi
  if grep -Eq '\\(u[0-9a-fA-F]{4}|x[0-9a-fA-F]{2})' "$dockerfile"; then
    echo "$dockerfile:1: encoded character escapes are forbidden in Dockerfiles" >&2
    failures=$((failures + 1))
  fi

  while IFS=$'\t' read -r line instruction; do
    [[ -n "$instruction" ]] || continue
    keyword="$(printf '%s\n' "${instruction%% *}" | tr '[:lower:]' '[:upper:]')"
    rest="${instruction#* }"

    case "$keyword" in
      ONBUILD)
        echo "$dockerfile:$line: ONBUILD is forbidden by the container security policy" >&2
        failures=$((failures + 1))
        ;;
      FROM)
        read -r -a words <<<"$rest"
        index=0
        while ((index < ${#words[@]})) && [[ "${words[$index]}" == --* ]]; do
          index=$((index + 1))
        done
        if ((index >= ${#words[@]})); then
          echo "$dockerfile:$line: malformed FROM instruction" >&2
          failures=$((failures + 1))
          continue
        fi

        image="${words[$index]}"
        check_image_reference "$dockerfile" "$line" "$image" "external FROM"

        index=$((index + 1))
        while ((index + 1 < ${#words[@]})); do
          token="$(printf '%s\n' "${words[$index]}" | tr '[:lower:]' '[:upper:]')"
          if [[ "$token" == "AS" ]]; then
            stage_aliases+=("${words[$((index + 1))]}")
            break
          fi
          index=$((index + 1))
        done
        ;;
      ADD)
        if [[ "$rest" == *'$'* ]]; then
          echo "$dockerfile:$line: dynamic ADD sources cannot be verified" >&2
          failures=$((failures + 1))
        elif printf '%s\n' "$rest" |
          grep -Eiq '(^|[[:space:]"\047])((https?|git|ssh)://|[A-Za-z0-9._-]+@[^[:space:]:]+:[^[:space:]]+)'; then
          echo "$dockerfile:$line: remote ADD bypasses checksum verification" >&2
          failures=$((failures + 1))
        fi
        ;;
      COPY)
        read -r -a words <<<"$rest"
        index=0
        while ((index < ${#words[@]})); do
          token="${words[$index]}"
          case "$token" in
            --from=*)
              source="${token#--from=}"
              check_image_reference "$dockerfile" "$line" "$source" "external COPY --from"
              ;;
            --from)
              index=$((index + 1))
              if ((index >= ${#words[@]})); then
                echo "$dockerfile:$line: COPY --from is missing its source" >&2
                failures=$((failures + 1))
                break
              fi
              source="${words[$index]}"
              check_image_reference "$dockerfile" "$line" "$source" "external COPY --from"
              ;;
          esac
          index=$((index + 1))
        done
        ;;
      RUN)
        command_text="$rest"
        read -r -a words <<<"$rest"
        for token in "${words[@]}"; do
          [[ "$token" == --mount=* ]] || continue
          mount="${token#--mount=}"
          IFS=',' read -r -a mount_options <<<"$mount"
          for option in "${mount_options[@]}"; do
            [[ "$option" == from=* ]] || continue
            source="${option#from=}"
            check_image_reference "$dockerfile" "$line" "$source" "external RUN --mount source"
          done
        done
        if run_uses_direct_downloader "$command_text"; then
          echo "$dockerfile:$line: direct curl/wget bypasses download-verified" >&2
          failures=$((failures + 1))
        fi
        ;;
    esac
  done < <(normalize_dockerfile "$dockerfile")
}

dockerfiles=()
if (($# > 0)); then
  dockerfiles=("$@")
else
  while IFS= read -r -d '' dockerfile; do
    dockerfiles+=("$dockerfile")
  done < <(
    find . -type f \
      \( -name Dockerfile -o -name '*Dockerfile' -o -name 'Dockerfile.*' \) \
      -not -path './.git/*' -print0
  )

  helper="provisioner/download-verified.sh"
  if [[ ! -x "$helper" ]] ||
    ! grep -Fq -- "--proto '=https'" "$helper" ||
    ! grep -Fq 'sha256sum --check --status' "$helper"; then
    echo "$helper: verified-download helper is missing required HTTPS or SHA-256 enforcement" >&2
    failures=$((failures + 1))
  fi
fi

if ((${#dockerfiles[@]} == 0)); then
  echo "no Dockerfiles found" >&2
  exit 1
fi

for dockerfile in "${dockerfiles[@]}"; do
  check_dockerfile "$dockerfile"
done

if ((failures > 0)); then
  exit 1
fi

printf 'Verified container build security policy in %d Dockerfiles.\n' "${#dockerfiles[@]}"
