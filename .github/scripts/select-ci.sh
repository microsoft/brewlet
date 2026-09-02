#!/usr/bin/env bash
set -euo pipefail

changed_files="${1:?usage: select-ci.sh <changed-files> [--all]}"
mode="${2:-}"

core=false
kubernetes=false
helm=false
maven=false
provisioner=false

if [[ "$mode" == "--all" ]]; then
  core=true
  kubernetes=true
  helm=true
  maven=true
  provisioner=true
else
  while IFS= read -r path; do
    case "$path" in
      .github/workflows/ci.yml|.github/scripts/select-ci.sh|\
      integration-tests/e2e/tier1-unit.sh)
        core=true
        kubernetes=true
        helm=true
        maven=true
        provisioner=true
        ;;
      core/**)
        core=true
        ;;
      kubernetes/charts/**)
        helm=true
        ;;
      kubernetes/**)
        kubernetes=true
        ;;
      maven-plugin/**)
        maven=true
        ;;
      provisioner/**)
        provisioner=true
        ;;
    esac
  done <"$changed_files"
fi

{
  echo "core=$core"
  echo "kubernetes=$kubernetes"
  echo "helm=$helm"
  echo "maven=$maven"
  echo "provisioner=$provisioner"
} >>"${GITHUB_OUTPUT:-/dev/stdout}"
