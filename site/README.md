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

Keep technical prerequisites, operational safety warnings, and authentication
requirements for users' own clusters and registries. Do not present roadmap
items as implemented features.

Use [the GitHub Pages workflow](../.github/workflows/site-pages.yml) as the source
of truth for building and publishing the landing page and `/docs/`.

## Contents

| Path | Purpose |
|---|---|
| `index.html`, `assets/` | Static landing page and its visual assets |
| `../docs/` | User and operator documentation |
| `../docs/workshops/` | Role-based workshop material for operators and developers |
| `assets/images/` | Brand assets and architecture diagrams |
| `CNAME` | GitHub Pages custom domain |
| `NOTICE.txt` | Browser-delivered third-party license and attribution text |

## Local preview

Landing page:

```bash
python3 -m http.server 8099 --directory site
```

Then open <http://localhost:8099>.

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
