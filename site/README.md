# Brewlet site and documentation

[![Deploy website](https://github.com/microsoft/brewlet/actions/workflows/site-pages.yml/badge.svg)](https://github.com/microsoft/brewlet/actions/workflows/site-pages.yml)
[![License: MIT](https://img.shields.io/github/license/microsoft/brewlet)](../LICENSE.txt)

This directory contains the [brewlet.sh](https://brewlet.sh) static website and
branding assets. User-facing documentation and workshop material live in the
repository-level [`docs/`](../docs/) directory.

## Content policy

Write the landing page, documentation, and workshops for a public release:
Brewlet's source, release downloads, Maven plugin, Helm chart, and component
images are released and accessible. Repository visibility must not change this
reader-facing assumption. Make released artifacts the default installation
path and source builds an optional development path.

Keep landing pages free of hardcoded Brewlet release numbers. Install the latest
CLI and read its version with `brewlet version` to select matching example source
and Maven plugin artifacts. Keep version-specific validation history in the docs.

Keep technical prerequisites, operational safety warnings, and authentication
requirements for users' own clusters and registries. Do not present roadmap
items as implemented features.

Use [the GitHub Pages workflow](../.github/workflows/site-pages.yml) as the source
of truth for building and publishing the landing page and `/docs/`.

## Contents

| Path | Purpose |
|---|---|
| `index.html`, `assets/` | Static landing page and its visual assets |
| `index-value-prop.html` | Standalone value-proposition landing page; intentionally not linked from `index.html` |
| `../docs/` | User and operator documentation |
| `../docs/workshops/` | Role-based workshop material for operators and developers |
| `assets/images/` | Brand assets and architecture diagrams |
| `CNAME` | GitHub Pages custom domain |
| `try-brewlet.sh` | Disposable kind/PetClinic demo; private tools and kubeconfig, automatic scoped cleanup |
| `NOTICE.txt` | Browser-delivered third-party license and attribution text |

## Local preview

Landing page:

```bash
python3 -m http.server 8099 --directory site
```

Then open <http://localhost:8099>.

The alternate landing page is available directly at
<http://localhost:8099/index-value-prop.html>. It has isolated styles and does
not change the primary homepage. It is marked `noindex` to keep this parallel
version out of search results; the URL remains publicly accessible.

Documentation site (`/docs/` with left navigation):

```bash
python3 -m pip install -r site/requirements.txt
npm ci --ignore-scripts --prefix site/notices
python3 site/scripts/generate-notice.py
python3 -m mkdocs serve -f site/mkdocs.yml
```

Then open <http://localhost:8000/docs/>.

## Deployment

Pushes to `main` that change the web assets, documentation, or Pages
workflow build and deploy the static landing page plus a rendered MkDocs site at
`/docs/`. The workflow regenerates `NOTICE.txt` and fails if the checked-in
notice is stale.

The same workflow copies `index-value-prop.html` to the publish directory,
making it available at <https://brewlet.sh/index-value-prop.html> without
adding a navigation link from the primary homepage.

It also publishes the optional automated demo, `try-brewlet.sh`, at
<https://brewlet.sh/try-brewlet.sh>. The [local Kubernetes article](../docs/local-kubernetes.md)
is the step-by-step walkthrough, not a wrapper around this script.
The script is self-contained: do not make it depend on unpublished working-tree
files. Run it with `bash site/try-brewlet.sh` for live validation. It downloads
the released CLI and pinned PetClinic source, creates a uniquely named kind cluster on the
selected local Docker engine, and deletes only that cluster on exit. All shell
options and environment changes belong to the child process, never the user's
interactive terminal. Offline lifecycle regressions run in `make site-contract-check`.

### Public release validation

Before publishing installation guidance, run the same release smoke test as
the Pages workflow:

```bash
./site/scripts/verify-release-artifacts.sh
```

It requires `curl`, Maven, JDK 21+, Helm, Docker CLI, and an authenticated
GitHub CLI for provenance verification. Artifact downloads and registry pulls
are anonymous; the script isolates curl, Docker, and Helm configuration so a
maintainer's saved credentials cannot hide inaccessible packages. It installs
the CLI into a temporary directory, exercises the local Java example and Maven
plugin, pulls and renders the chart, and verifies component manifests and
release provenance. By default it resolves the latest release through the
installer and uses that exact version for all subsequent checks. Pass a version
as the first argument only when investigating a specific release. It does not
install anything into a cluster.

Making the repository public does not change existing GHCR package visibility.
A package administrator must separately make `charts/brewlet`,
`brewlet-operator`, `brewlet-admission`, and `brewlet-node-provisioner` public
under the `microsoft` organization. Verify anonymous access to all four, not
just the chart.

Run **Deploy website** with `skip_release_smoke` left **false** to validate the
public release before deployment. A green deployment with that input enabled
does not establish that the installation paths work for public users. The
release workflow's provenance smoke job also runs automatically for public
tagged releases.
