#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

E2E_DIR="$(cd "$(dirname "$0")" && pwd)"
exec "$E2E_DIR/run.sh" --tier 2
