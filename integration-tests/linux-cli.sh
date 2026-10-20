#!/bin/sh
set -eu

: "${BREWLET_TEST_BINARY:?Set an absolute path to a Linux arm64 CLI binary}"
: "${BREWLET_TEST_WORK_PARENT:?Set the invocation-owned fixture work parent}"
: "${BREWLET_TEST_IMAGE:?Set the pinned, locally built test runtime image}"

exec docker run --platform linux/arm64 --pull never --rm --network "${BREWLET_TEST_NETWORK:-none}" \
    --user "$(id -u):$(id -g)" \
    --cap-drop ALL --security-opt no-new-privileges --read-only \
    --tmpfs /tmp:rw,nosuid,nodev \
    --entrypoint /brewlet \
    --mount "type=bind,source=$BREWLET_TEST_BINARY,target=/brewlet,readonly" \
    --mount "type=bind,source=$PWD,target=$PWD,readonly" \
    --mount "type=bind,source=$BREWLET_TEST_WORK_PARENT,target=$BREWLET_TEST_WORK_PARENT" \
    --workdir "${BREWLET_TEST_CLI_CWD:-$BREWLET_TEST_WORK_PARENT}" "$BREWLET_TEST_IMAGE" "$@"
