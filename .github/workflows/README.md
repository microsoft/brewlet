# GitHub Actions workflows

This directory contains Brewlet's continuous integration, end-to-end, website,
and release automation. The workflows intentionally separate fast,
component-level checks from expensive tests that create clusters, build images,
or provision a Brewlet runtime.

## Workflow responsibilities

| Workflow | Purpose |
|---|---|
| [`ci.yml`](ci.yml) | Builds and tests only the components affected by a change. |
| [`e2e.yml`](e2e.yml) | Selects representative E2E tiers for pull requests and runs the full suite after merge and nightly. |
| [`site-pages.yml`](site-pages.yml) | Checks site and documentation changes on pull requests and deploys GitHub Pages from `main`. |
| [`release.yml`](release.yml) | Publishes release images, the Helm chart, CLI archives, and the Maven plugin. |

The selectors used by CI and E2E live in
[`../scripts/`](../scripts/). Keep workflow policy in those scripts rather than
duplicating path rules in YAML.

## Design goals

The workflows follow these rules:

1. Pull requests should test the behavior they can affect without rebuilding
   every component or running all 15 integration tiers.
2. Pushes to `main`, scheduled runs, and manually requested runs retain broad
   E2E coverage.
3. Branch protection depends on stable gate jobs, not on conditional matrix
   jobs whose names and presence vary by change.
4. Changes to shared test infrastructure run broader checks because their
   impact cannot be inferred safely.
5. Site pull requests build exactly as the GitHub Pages workflow builds them,
   but only non-PR events may deploy.

## CI workflow

`ci.yml` runs for pull requests, pushes to `main`, merge queue groups, and
manual dispatches.

The `changes` job compares the event's base SHA with `GITHUB_SHA`, writes the
changed paths to a file, and passes it to
[`select-ci.sh`](../scripts/select-ci.sh). The selector enables these jobs:

| Changed path | Selected check |
|---|---|
| `core/**` | Go core runtime |
| `kubernetes/**` | Go Kubernetes platform |
| `kubernetes/charts/**` | Helm chart |
| `maven-plugin/**` | Maven plugin |
| `provisioner/**` | Node provisioner entrypoint tests |

Chart-only changes do not run the Kubernetes Go test suite. Documentation-only
changes can therefore complete CI without starting any component runner.

Changes to `ci.yml`, `select-ci.sh`, or the tier 1 wrapper select all relevant
component checks. Manual dispatches also select every component.

### CI gate

The `CI gate` job always runs after the selector and all conditional component
jobs. It accepts `success` and intentional `skipped` results, but fails when a
selected job fails or is cancelled.

Branch protection should require:

```text
CI / CI gate
```

Do not require the individual component jobs. A job such as `Go core runtime`
is expected to be skipped when unrelated paths change.

## E2E workflow

The E2E harness is divided into the tiers defined by
[`../../integration-tests/e2e/run.sh`](../../integration-tests/e2e/run.sh).
Creating a kind cluster and provisioning node runtimes is substantially more
expensive than the component tests, so pull requests receive a dynamic matrix
from [`select-e2e.sh`](../scripts/select-e2e.sh).

### Pull request selection

The selector accumulates tiers across all changed files and removes duplicate
selections.

| Changed behavior | Selected tier |
|---|---|
| General CLI or core artifact code | 2 |
| Core runtime or containerd shim | 3 |
| AppCDS runtime code | 3 and 8 |
| Runnable image or managed dependency artifact code | 2 and 12 |
| Metrics exporter | 15 |
| Maven plugin | 2 |
| General Kubernetes control-plane code | 4 |
| Admission webhook | 6 |
| NodeProfile API or controller | 13 |
| Helm chart | 10 |
| Helm metrics or Grafana templates | 10 and 15 |
| Kubernetes production Dockerfile | 10 |
| Provisioner implementation | 14 |
| Provisioner Dockerfile | 14 and 15 |
| One tier's test script | That tier |

Documentation and site-only changes do not run E2E.

Tier 1 is not selected by `e2e.yml` because it repeats the Go component tests
owned by `ci.yml`. A change to the tier 1 wrapper selects the corresponding CI
jobs instead.

### Full-suite selection

The full E2E suite runs for:

- pushes to `main`;
- the nightly schedule;
- manual workflow dispatches;
- pull requests carrying the `full-e2e` label; and
- changes to shared E2E orchestration, reset, library, fixture, or workflow
  files.

Adding or removing the `full-e2e` label triggers a new workflow evaluation.

Full-suite entries retain the established grouping for tiers 4-6 and 13 so
those tiers share one kind cluster. Targeted pull request entries remain
independent, which avoids provisioning resources for unrelated tiers.

### E2E gate

The `E2E gate` job always runs. A pull request with no selected E2E tiers
produces an intentionally skipped matrix and a successful gate. Any selected
tier failure causes the gate to fail.

Branch protection should require:

```text
E2E / E2E gate
```

Do not require individual tier job names because the dynamic matrix changes
from one pull request to another.

The selector also writes the chosen tiers and selection reason to the workflow
summary. Check that summary when a pull request appears to run too much or too
little coverage.

## Website and documentation

`site-pages.yml` is the source of truth for both the static landing page and
MkDocs documentation.

Changes under `site/**`, `docs/**`, or to the workflow itself trigger:

1. the public release and Getting Started smoke test;
2. a strict MkDocs build;
3. assembly and verification of `site/_site`; and
4. artifact upload and GitHub Pages deployment for non-PR events.

Pull requests stop after validation and cannot deploy. Pushes to `main` and
manual dispatches reuse the already-built publish directory instead of
rebuilding it in the deploy job.

PR site checks use per-PR concurrency. Deploying events share one
`deployment` concurrency group so two refs cannot race to publish GitHub Pages.

## Release workflow

`release.yml` is intentionally independent of CI and E2E selection. It runs for
version tags and manual releases and publishes:

- operator, admission, and provisioner images;
- the Brewlet Helm chart;
- CLI archives for supported OS and architecture pairs; and
- the Maven plugin artifact.

Pull request workflows must not publish any of these artifacts.

## Maintaining the selectors

When adding a component or E2E tier:

1. Add or update the path rule in `select-ci.sh` or `select-e2e.sh`.
2. Place specific rules before broad rules. Bash `case` uses only the first
   matching pattern for each changed path.
3. Choose the smallest tier that executes the changed production path.
4. Select the full suite when a shared fixture or orchestration change can
   affect multiple tiers unpredictably.
5. Keep the stable gate job in the dependency list when adding a conditional
   job.
6. Update this README with the resulting policy.

The selector scripts can be exercised locally without invoking GitHub Actions:

```bash
printf '%s\n' core/shim/cmd/containerd-shim-brewlet-v2/service_linux.go \
  > /tmp/brewlet-changed-files

GITHUB_OUTPUT=/dev/stdout \
  .github/scripts/select-e2e.sh /tmp/brewlet-changed-files
```

Before committing workflow changes, parse the YAML and lint the shell
selectors:

```bash
shellcheck .github/scripts/select-ci.sh .github/scripts/select-e2e.sh
ruby -e "require 'yaml'; ARGV.each { |f| YAML.parse_file(f) }" \
  .github/workflows/*.yml
```

