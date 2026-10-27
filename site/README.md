# Brewlet website

The static landing page preserves main's Brewlet brand/story structure while
replacing Java-specific claims and unsupported release commands with the current
contracts. A single light theme, keyboard-accessible language examples, copy controls
and responsive navigation require no npm runtime package, remote font or CDN.

The Devoxx Belgium 2026 section carries over the recording, slides and local
thumbnail from `brewlet-0.x-java`.

MkDocs Material supplies the searchable documentation at `/docs/`. Its direct
dependencies are pinned in `site/requirements.txt`. The configured production
`site_url` also generates canonical page URLs and the documentation sitemap at
`https://brewlet.sh/docs/sitemap.xml`, which can be submitted to Google Search
Console after deployment. The comparison
draft is rendered without modifying its source; repository-file links become
branch-specific GitHub source links through the documented hook.

The landing page includes responsive desktop/mobile SVG architecture diagrams,
in main's schematic style but with the current runtime-infrastructure contract.
They distinguish CRI application pulls, operator reconciliation of node workers,
fenced runtime provisioning and the shim's local validation/lease/composition.
`assets/images/architecture.excalidraw` is the editable desktop topology source;
the mobile SVG rearranges the same flow for narrow screens. Both SVGs have
accessible descriptions and use the site's light color scheme.

## Build and preview

```sh
python3 -m venv site/.venv
site/.venv/bin/python -m pip install -r site/requirements.txt
make site-build SITE_PYTHON=site/.venv/bin/python
python3 -m http.server 8080 --bind 127.0.0.1 --directory site/_site
```

Open `http://127.0.0.1:8080`. The output follows main's Pages layout:
`site/_site/index.html`, static assets, license/notices and `site/_site/docs/`.
All internal navigation works under a subdirectory as well as at a domain root.

```sh
make site-contract-check
```

The build runs MkDocs in strict mode, generates third-party notices from
installed dependency license texts, removes unused non-English search
extensions as main does, and verifies every built local link/fragment.
Contract checks cover the build tooling (source-link hook, link parser,
tutorial observer) and notices, not the wording of the site content.

## Social previews

The site build renders a shared 1200 x 630 PNG at
`/assets/images/social-card.png` using `scripts/social_card.py`, Pillow's bundled
font and the site's coffee/cream/blue palette. No font downloads or external
rendering service are needed. The generated notices include Pillow's licenses
and attribution for the bundled CC0 Aileron font.

The landing page and documentation supply Open Graph and Twitter large-image
metadata with absolute production URLs. Documentation previews use each page's
title and its front-matter `description` when provided, otherwise the site
description. Edit the renderer to update the shared artwork; `make site-build`
regenerates it with the rest of the site.

## Deployment

`.github/workflows/site-pages.yml` is the read-only build used by Pages deployment
and manual preview runs, not routine CI. It checks the exact source SHA, builds
the site and uploads `brewlet-website-preview`. Pull requests do not run this workflow automatically and never deploy.

`.github/workflows/site-deploy.yml` publishes to the existing `brewlet.sh`
GitHub Pages site after relevant changes reach `main`, or through manual
dispatch on `main`. It reuses the same checked build and deploys its artifact
without rebuilding. Only the deployment job has Pages and OIDC write
permissions, and it uses the `github-pages` environment. Runs on other branches
or forks cannot deploy.

The repository must keep Pages configured to use GitHub Actions and retain its
existing custom-domain configuration; this workflow does not change the domain
or create a CNAME file. No old-release installer or release smoke test is
presented as a working release workflow.
