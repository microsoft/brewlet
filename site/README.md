# Brewlet 2.0 website

The static landing page preserves main's Brewlet brand/story structure while
replacing Java-specific claims and unsupported release commands with the 2.0
contracts. Light/dark modes, keyboard-accessible language examples, copy controls
and responsive navigation require no npm runtime package, remote font or CDN.

MkDocs Material supplies the searchable documentation at `/docs/`. Its direct
dependencies are pinned in `site/requirements.txt`. The comparison
draft is rendered without modifying its source; repository-file links become
branch-specific GitHub source links through the documented hook.

The landing page includes responsive desktop/mobile SVG architecture diagrams,
in main's schematic style but with the 2.0 runtime-infrastructure contract.
They distinguish CRI application pulls, operator reconciliation of node workers,
fenced runtime provisioning and the shim's local validation/lease/composition.
`assets/images/architecture.excalidraw` is the editable desktop topology source;
the mobile SVG rearranges the same flow for narrow screens. Both SVGs have
accessible descriptions and follow the embedding page's light/dark color scheme.

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

For browser checks, install the pinned development-only dependency and Chromium:

```sh
npm ci --prefix site --ignore-scripts
PLAYWRIGHT_SKIP_BROWSER_GC=1 site/node_modules/.bin/playwright install chromium
npm test --prefix site
```

Use `PLAYWRIGHT_CHANNEL=chrome npm test --prefix site` to use an already installed
Chrome instead. The test owns an ephemeral local server and checks desktop/mobile,
both themes, keyboard tabs, clipboard, FAQ, documentation/search, subdirectory
hosting and the no-JavaScript fallback. It closes its browser/server afterward.
Screenshots go to ignored `site/_site-checks/` or `SITE_SCREENSHOTS`.

The initial website verification passed the strict ten-page build and all local
links/anchors, desktop/mobile light/dark browser interactions and search,
prefix hosting and the no-JavaScript fallback. The exact getting-started
descriptor and source were packaged and inspected with the real CLI inside
an isolated Linux container. On the development Mac, the same host packaging
attempt correctly rejected filesystem xattrs; validation was not weakened.

The build runs MkDocs in strict mode, generates third-party notices from
installed dependency license texts, removes unused non-English search
extensions as main does, and verifies every built local link/fragment.
Contract checks also protect honest 2.0 content and the draft's source bytes.

## Deployment

`.github/workflows/site-pages.yml` is the read-only build used by CI and manual
preview runs. It checks the exact source SHA, builds the site, runs the browser
checks and uploads `brewlet-2-website-preview`. Pull requests never deploy.

`.github/workflows/site-deploy.yml` publishes to the existing `brewlet.sh`
GitHub Pages site after relevant changes reach `main`, or through manual
dispatch on `main`. It reuses the same checked build and deploys its artifact
without rebuilding. Only the deployment job has Pages and OIDC write
permissions, and it uses the `github-pages` environment. Runs on other branches
or forks cannot deploy.

The repository must keep Pages configured to use GitHub Actions and retain its
existing custom-domain configuration; this workflow does not change the domain
or create a CNAME file. No old-release installer or release smoke test is
presented as a working 2.0 release workflow.
