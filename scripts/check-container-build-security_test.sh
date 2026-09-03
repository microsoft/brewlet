#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
POLICY="$ROOT/scripts/check-container-build-security.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

DIGEST="$(printf 'a%.0s' {1..64})"

cat >"$WORK/good.Dockerfile" <<EOF
FROM example.invalid/build:1@sha256:$DIGEST AS build
RUN apt-get update && apt-get install -y curl
RUN download-verified tool https://example.invalid/tool $DIGEST /tmp/tool
RUN --mount=type=bind,from=build,target=/source true
FROM build AS final
COPY --from=build /tmp/tool /tmp/tool
EOF

cat >"$WORK/unpinned.Dockerfile" <<'EOF'
FROM example.invalid/build:1
EOF

cat >"$WORK/continued-unpinned.Dockerfile" <<'EOF'
FR\
OM example.invalid/build:1
EOF

cat >"$WORK/remote-add.Dockerfile" <<EOF
FROM example.invalid/build:1@sha256:$DIGEST
ADD https://example.invalid/tool /usr/local/bin/tool
EOF

cat >"$WORK/remote-add-ssh.Dockerfile" <<EOF
FROM example.invalid/build:1@sha256:$DIGEST
ADD ssh://git@example.invalid/repo.git /src
EOF

cat >"$WORK/remote-add-scp.Dockerfile" <<EOF
FROM example.invalid/build:1@sha256:$DIGEST
ADD deploy@example.invalid:team/repo.git /src
EOF

cat >"$WORK/dynamic-add.Dockerfile" <<EOF
FROM example.invalid/build:1@sha256:$DIGEST
ARG TOOL_URL
ADD \$TOOL_URL /usr/local/bin/tool
EOF

cat >"$WORK/direct-curl.Dockerfile" <<EOF
FROM example.invalid/build:1@sha256:$DIGEST
RUN set -eu; curl -fsSL https://example.invalid/tool -o /tmp/tool
EOF

cat >"$WORK/direct-wget.Dockerfile" <<EOF
FROM example.invalid/build:1@sha256:$DIGEST
RUN true && wget https://example.invalid/tool -O /tmp/tool
EOF

cat >"$WORK/json-curl.Dockerfile" <<EOF
FROM example.invalid/build:1@sha256:$DIGEST
RUN ["curl", "-fsSL", "https://example.invalid/tool", "-o", "/tmp/tool"]
EOF

cat >"$WORK/json-escaped-curl.Dockerfile" <<EOF
FROM example.invalid/build:1@sha256:$DIGEST
RUN ["sh", "-c", "\\u0063url -fsSL https://example.invalid/tool -o /tmp/tool"]
EOF

cat >"$WORK/json-escaped-add.Dockerfile" <<EOF
FROM example.invalid/build:1@sha256:$DIGEST
ADD ["\\u0024SOURCE", "/usr/local/bin/tool"]
EOF

cat >"$WORK/concatenated-curl.Dockerfile" <<EOF
FROM example.invalid/build:1@sha256:$DIGEST
RUN c""url -fsSL https://example.invalid/tool -o /tmp/tool
EOF

cat >"$WORK/background-curl.Dockerfile" <<EOF
FROM example.invalid/build:1@sha256:$DIGEST
RUN apt-get install -y curl & curl -fsSL https://example.invalid/tool -o /tmp/tool && wait
EOF

cat >"$WORK/mounted-curl.Dockerfile" <<EOF
FROM example.invalid/build:1@sha256:$DIGEST
RUN --mount=type=cache,target=/tmp curl -fsSL https://example.invalid/tool -o /tmp/tool
EOF

cat >"$WORK/env-curl.Dockerfile" <<EOF
FROM example.invalid/build:1@sha256:$DIGEST
RUN env CURL_HOME=/tmp curl -fsSL https://example.invalid/tool -o /tmp/tool
EOF

cat >"$WORK/heredoc-curl.Dockerfile" <<EOF
FROM example.invalid/build:1@sha256:$DIGEST
RUN <<'SCRIPT'
curl -fsSL https://example.invalid/tool -o /tmp/tool
SCRIPT
EOF

cat >"$WORK/numeric-heredoc-curl.Dockerfile" <<EOF
FROM example.invalid/build:1@sha256:$DIGEST
RUN <<123
curl -fsSL https://example.invalid/tool -o /tmp/tool
123
EOF

cat >"$WORK/multiple-heredoc-curl.Dockerfile" <<EOF
FROM example.invalid/build:1@sha256:$DIGEST
RUN <<ONE <<TWO
echo one
ONE
curl -fsSL https://example.invalid/tool -o /tmp/tool
TWO
EOF

cat >"$WORK/nested-package-curl.Dockerfile" <<EOF
FROM example.invalid/build:1@sha256:$DIGEST
RUN apt-get install -y curl && apt-get install -y "\$(curl -fsSL https://example.invalid/tool -o /usr/local/bin/tool; printf bash)"
EOF

cat >"$WORK/onbuild-curl.Dockerfile" <<EOF
FROM example.invalid/build:1@sha256:$DIGEST AS base
ONBUILD RUN curl -fsSL https://example.invalid/tool -o /tmp/tool
FROM base
EOF

cat >"$WORK/continued-onbuild-curl.Dockerfile" <<EOF
FROM example.invalid/build:1@sha256:$DIGEST AS base
ONBU\
ILD RUN curl -fsSL https://example.invalid/tool -o /tmp/tool
FROM base
EOF

cat >"$WORK/custom-escape.Dockerfile" <<EOF
# escape=\`
FROM example.invalid/build:1@sha256:$DIGEST
EOF

cat >"$WORK/external-copy.Dockerfile" <<EOF
FROM example.invalid/build:1@sha256:$DIGEST
COPY --from=example.invalid/tools:latest /tool /tool
EOF

cat >"$WORK/external-mount.Dockerfile" <<EOF
FROM example.invalid/build:1@sha256:$DIGEST
RUN --mount=type=bind,from=example.invalid/tools:latest,target=/tools true
EOF

"$POLICY" "$WORK/good.Dockerfile" >/dev/null

expect_failure() {
  local file="$1"
  local expected="$2"
  local output
  if output="$("$POLICY" "$file" 2>&1)"; then
    echo "expected policy failure for $file" >&2
    exit 1
  fi
  if [[ "$output" != *"$expected"* ]]; then
    echo "expected '$expected' for $file, got:" >&2
    echo "$output" >&2
    exit 1
  fi
}

expect_failure "$WORK/unpinned.Dockerfile" "external FROM must use a full sha256 digest"
expect_failure "$WORK/continued-unpinned.Dockerfile" "external FROM must use a full sha256 digest"
expect_failure "$WORK/remote-add.Dockerfile" "remote ADD bypasses checksum verification"
expect_failure "$WORK/remote-add-ssh.Dockerfile" "remote ADD bypasses checksum verification"
expect_failure "$WORK/remote-add-scp.Dockerfile" "remote ADD bypasses checksum verification"
expect_failure "$WORK/dynamic-add.Dockerfile" "dynamic ADD sources cannot be verified"
expect_failure "$WORK/direct-curl.Dockerfile" "direct curl/wget bypasses download-verified"
expect_failure "$WORK/direct-wget.Dockerfile" "direct curl/wget bypasses download-verified"
expect_failure "$WORK/json-curl.Dockerfile" "direct curl/wget bypasses download-verified"
expect_failure "$WORK/json-escaped-curl.Dockerfile" "encoded character escapes are forbidden in Dockerfiles"
expect_failure "$WORK/json-escaped-add.Dockerfile" "encoded character escapes are forbidden in Dockerfiles"
expect_failure "$WORK/concatenated-curl.Dockerfile" "direct curl/wget bypasses download-verified"
expect_failure "$WORK/background-curl.Dockerfile" "direct curl/wget bypasses download-verified"
expect_failure "$WORK/mounted-curl.Dockerfile" "direct curl/wget bypasses download-verified"
expect_failure "$WORK/env-curl.Dockerfile" "direct curl/wget bypasses download-verified"
expect_failure "$WORK/heredoc-curl.Dockerfile" "direct curl/wget bypasses download-verified"
expect_failure "$WORK/numeric-heredoc-curl.Dockerfile" "direct curl/wget bypasses download-verified"
expect_failure "$WORK/multiple-heredoc-curl.Dockerfile" "direct curl/wget bypasses download-verified"
expect_failure "$WORK/nested-package-curl.Dockerfile" "direct curl/wget bypasses download-verified"
expect_failure "$WORK/onbuild-curl.Dockerfile" "ONBUILD is forbidden by the container security policy"
expect_failure "$WORK/continued-onbuild-curl.Dockerfile" "ONBUILD is forbidden by the container security policy"
expect_failure "$WORK/custom-escape.Dockerfile" "non-default Dockerfile escape directives are forbidden"
expect_failure "$WORK/external-copy.Dockerfile" "external COPY --from must use a full sha256 digest"
expect_failure "$WORK/external-mount.Dockerfile" "external RUN --mount source must use a full sha256 digest"

echo "Container build security policy tests passed."
