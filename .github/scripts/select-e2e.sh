#!/usr/bin/env bash
set -euo pipefail

changed_files="${1:?usage: select-e2e.sh <changed-files> [--full]}"
mode="${2:-}"
tiers=""
reason="No runtime-affecting paths changed"
full_suite=false

add_tier() {
  case " $tiers " in
    *" $1 "*) ;;
    *) tiers="${tiers:+$tiers }$1" ;;
  esac
}

select_full_suite() {
  tiers="2 3 4 5 6 7 8 9 10 11 12 13 14 15"
  reason="$1"
  full_suite=true
}

if [[ "$mode" == "--full" ]]; then
  select_full_suite "Full suite required by event or pull request label"
else
  while IFS= read -r path; do
    case "$path" in
      .github/workflows/e2e.yml|.github/scripts/select-e2e.sh|\
      integration-tests/e2e/run.sh|integration-tests/e2e/lib.sh|\
      integration-tests/e2e/reset.sh|integration-tests/e2e/webhook-cases.sh)
        select_full_suite "Shared E2E infrastructure changed"
        break
        ;;
      integration-tests/e2e/tier1-*) ;;
      integration-tests/e2e/tier2-*) add_tier 2 ;;
      integration-tests/e2e/tier3-*) add_tier 3 ;;
      integration-tests/e2e/tier4-*) add_tier 4 ;;
      integration-tests/e2e/tier5-*) add_tier 5 ;;
      integration-tests/e2e/tier6-*) add_tier 6 ;;
      integration-tests/e2e/tier7-*) add_tier 7 ;;
      integration-tests/e2e/tier8-*) add_tier 8 ;;
      integration-tests/e2e/tier9-*) add_tier 9 ;;
      integration-tests/e2e/tier10-*) add_tier 10 ;;
      integration-tests/e2e/tier11-*) add_tier 11 ;;
      integration-tests/e2e/tier12-*) add_tier 12 ;;
      integration-tests/e2e/tier13-*) add_tier 13 ;;
      integration-tests/e2e/tier14-*) add_tier 14 ;;
      integration-tests/e2e/tier15-*) add_tier 15 ;;
      integration-tests/**)
        select_full_suite "Shared fixtures or integration-test support changed"
        break
        ;;
      core/cmd/brewlet-metrics-exporter/**)
        add_tier 15
        ;;
      core/go.mod|core/go.sum)
        add_tier 2
        add_tier 3
        ;;
      core/internal/runtime/*cds*)
        add_tier 3
        add_tier 8
        ;;
      core/shim/**|core/internal/runtime/**)
        add_tier 3
        ;;
      core/internal/artifact/image*|core/internal/artifact/dependency_bundle*)
        add_tier 2
        add_tier 12
        ;;
      core/**)
        add_tier 2
        ;;
      maven-plugin/**)
        add_tier 2
        ;;
      kubernetes/charts/brewlet/templates/metrics.yaml|\
      kubernetes/charts/brewlet/templates/grafana-dashboard.yaml)
        add_tier 10
        add_tier 15
        ;;
      kubernetes/charts/**)
        add_tier 10
        ;;
      kubernetes/Dockerfile)
        add_tier 10
        ;;
      kubernetes/internal/observability/**)
        add_tier 15
        ;;
      kubernetes/internal/admission/**|kubernetes/cmd/admission/**)
        add_tier 6
        ;;
      kubernetes/api/nodeprofile/**|kubernetes/internal/controller/nodeprofile*|\
      kubernetes/deploy/nodeprofile*|kubernetes/deploy/sample-nodeprofile*)
        add_tier 13
        ;;
      kubernetes/**)
        add_tier 4
        ;;
      provisioner/Dockerfile)
        add_tier 14
        add_tier 15
        ;;
      provisioner/**)
        add_tier 14
        ;;
    esac
  done <"$changed_files"

  if [[ -n "$tiers" ]]; then
    reason="Selected from changed component paths"
  fi
fi

matrix='{"include":[]}'
add_matrix_entry() {
  matrix="$(
    jq -c \
      --arg name "$1" \
      --arg tiers "$2" \
      --argjson java "$3" \
      --argjson cluster "$4" \
      --argjson helm "$5" \
      '.include += [{
        name: $name,
        tiers: $tiers,
        java: $java,
        cluster: $cluster,
        helm: $helm
      }]' <<<"$matrix"
  )"
}

if [[ "$full_suite" == "true" ]]; then
  add_matrix_entry "Local CLI and JVM (tier 2)" "--tier 2" true false false
  add_matrix_entry "Shim to runc (tier 3)" "--tier 3" true false false
  add_matrix_entry "Kubernetes control plane (tiers 4-6, 13)" \
    "--tier 4 --tier 5 --tier 6 --tier 13" false true true
  add_matrix_entry "Spring PetClinic (tier 7)" "--tier 7" true true true
  add_matrix_entry "Node-side AppCDS (tier 8)" "--tier 8" true true false
  add_matrix_entry "Serving and cgroups (tier 9)" "--tier 9" true true false
  add_matrix_entry "Helm installation (tier 10)" "--tier 10" false true true
  add_matrix_entry "Webhook resilience (tier 11)" "--tier 11" false true false
  add_matrix_entry "Runnable image (tier 12)" "--tier 12" true true false
  add_matrix_entry "Custom JDK NodeProfile (tier 14)" "--tier 14" true true false
  add_matrix_entry "Runtime metrics (tier 15)" "--tier 15" true true true
else
  for tier in $tiers; do
    case "$tier" in
      2) add_matrix_entry "Local CLI and JVM (tier 2)" "--tier 2" true false false ;;
      3) add_matrix_entry "Shim to runc (tier 3)" "--tier 3" true false false ;;
      4) add_matrix_entry "Kubernetes control plane (tier 4)" "--tier 4" false true true ;;
      5) add_matrix_entry "Host webhook (tier 5)" "--tier 5" false true false ;;
      6) add_matrix_entry "In-cluster webhook (tier 6)" "--tier 6" false true false ;;
      7) add_matrix_entry "Spring PetClinic (tier 7)" "--tier 7" true true true ;;
      8) add_matrix_entry "Node-side AppCDS (tier 8)" "--tier 8" true true false ;;
      9) add_matrix_entry "Serving and cgroups (tier 9)" "--tier 9" true true false ;;
      10) add_matrix_entry "Helm installation (tier 10)" "--tier 10" false true true ;;
      11) add_matrix_entry "Webhook resilience (tier 11)" "--tier 11" false true false ;;
      12) add_matrix_entry "Runnable image (tier 12)" "--tier 12" true true false ;;
      13) add_matrix_entry "NodeProfile (tier 13)" "--tier 13" false true true ;;
      14) add_matrix_entry "Custom JDK NodeProfile (tier 14)" "--tier 14" true true false ;;
      15) add_matrix_entry "Runtime metrics (tier 15)" "--tier 15" true true true ;;
    esac
  done
fi

has_e2e=false
[[ -n "$tiers" ]] && has_e2e=true

{
  echo "matrix=$matrix"
  echo "has_e2e=$has_e2e"
  echo "selected_tiers=${tiers:-none}"
  echo "reason=$reason"
} >>"${GITHUB_OUTPUT:-/dev/stdout}"
