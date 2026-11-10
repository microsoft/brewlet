# Brewlet release checklist

This is the maintainer runbook for preparing, publishing and verifying a Brewlet
release. See [release distribution and recovery](docs/supported-release.md) for
the published artifacts, compatibility limits and recovery policy, and the
[acceptance guide](acceptance/README.md) for detailed test and evidence scopes.

## How a release starts

Pushing a `vVERSION` tag to `microsoft/brewlet` starts the
[Release workflow](.github/workflows/release.yml). The tag must match the
version in `release.json` at the tagged commit. The workflow builds and tests
that commit, then publishes only if every required gate succeeds.

There is no patch/minor/major selector and the workflow does not bump versions
or create tags. A manual **Release** workflow dispatch is a build-and-verify
dry run: it does not run Release Gate Tests or publish anything.

Versions use `MAJOR.MINOR.PATCH`, optionally followed by `-alpha.N`, `-beta.N`
or `-rc.N`. Choose the next version explicitly:

| Version | Release channel |
| --- | --- |
| Plain `0.x.y` | Beta, published as a normal GitHub release and marked latest |
| Plain `1.0.0` or later | Stable, published as a normal GitHub release and marked latest |
| Any supported suffix, such as `1.0.0-rc.1` | GitHub prerelease, never marked latest |

## 1. Prepare the release

- [ ] Choose an unused version and review the changes, release notes, known
  limitations and any compatibility or migration implications. Do not promise
  an upgrade path without evidence.
- [ ] Update `version` in [`release.json`](release.json) and review
  `publicationPolicy.supportedTransitions`. The current release tests require
  the same-candidate entry's `sourceVersion` and `targetVersion` to match the
  candidate version; updating metadata alone does not prove a transition has
  been tested.
- [ ] Update `maven-plugin/pom.xml`, the `version` and `appVersion` in
  `kubernetes/charts/brewlet/Chart.yaml`, and all four component image tags in
  `kubernetes/charts/brewlet/values.yaml`.
- [ ] Update the plugin version in
  `integration-tests/fixtures/java/app/pom.xml` and review the version,
  channel, policy and artifact-name expectations in
  `acceptance/release_test.py`. Its current expectations are pinned to the
  current beta; a prerelease or stable release also needs appropriate channel
  and legal-policy assertions.
- [ ] Review versioned documentation and examples, including
  `docs/supported-release.md`, `docs/local-kubernetes.md` and the Maven plugin
  README. Update claims about current releases and tested behavior only when
  supported; historical examples and evidence are not bulk replacements.
  Website deployment is separate from the release and can run when relevant
  documentation changes reach `main`, even before the artifacts are published.
- [ ] Optionally add `release-notes/VERSION.md`. Curated notes appear above
  GitHub's generated changelog. For a `0.x` release, include the beta warning
  in curated notes: the automatic warning is added only when no curated file
  exists.
- [ ] Confirm the release owner has permission to push the tag and run Actions.
  The `maven-central` environment must provide `MAVEN_GPG_KEY`,
  `MAVEN_GPG_PASSPHRASE`, `MAVEN_CENTRAL_USERNAME` and
  `MAVEN_CENTRAL_PASSWORD`; the signing key must match
  `publicationPolicy.mavenSigningFingerprint`. Follow any environment
  approval rules. Never put secrets in the repository or release notes.
- [ ] For a stable release, confirm the owner has made the legal/container
  policy determination described in the
  [distribution guide](docs/supported-release.md#legal-and-project-supply-chain-evidence).
  The reviewed `publicationPolicy.legalApproval` record is required; do not
  manufacture one or claim approval when further policy text is needed.
- [ ] Run the local checks below, address failures, and merge the reviewed
  release changes to `main`. Record the full merge commit SHA.

```sh
make release-check
make test legal-check
```

`release-check` validates release identity and runs the offline release tests.
`test` runs Go tests with the race detector, Maven verification and the offline
Python tests and ledger structure checks. `legal-check` verifies current
redistribution notices and packaging; running `make test` first also builds
the Maven artifact it inspects. See [CONTRIBUTING.md](CONTRIBUTING.md) for
development prerequisites.

## 2. Optionally check the candidate before tagging

From GitHub Actions, dispatch **Release Gate Tests** on `main` to run the full
release test gate, and/or dispatch **Release** on `main` to build and verify a
candidate without publishing. For example:

```sh
gh workflow run release-gate-tests.yml --repo microsoft/brewlet --ref main
gh workflow run release.yml --repo microsoft/brewlet --ref main
```

Review the run results and confirm each run's source SHA is the reviewed
release commit; a dispatch uses the selected branch's head when it starts.
These are optional early checks. The tag-triggered release runs Release Gate
Tests and builds a new candidate again for the tagged commit.

For an optional local candidate, use a clean, committed checkout with Go, Maven,
Helm, Docker Buildx and the pinned Syft tool available:

```sh
make release-candidate
```

The output is written to `dist/candidate/`, which must be empty before a new
build. This local build is not a substitute for the Actions release gate,
native-platform smoke jobs or GitHub attestations. See the
[candidate instructions](docs/supported-release.md#maintainers-build-and-inspect-a-candidate-without-publishing).

## 3. Tag the reviewed commit

Use a clean checkout of the reviewed `main` commit. Confirm that `origin` is
`microsoft/brewlet`; if it is not, use the official repository remote in the
commands below. These commands fetch current refs, verify the checkout matches
`origin/main`, and show the version, commit and any existing remote tag:

```sh
git remote get-url origin &&
git fetch origin --tags &&
SOURCE="$(git rev-parse 'refs/remotes/origin/main^{commit}')" &&
test "$(git rev-parse HEAD)" = "$SOURCE" &&
test -z "$(git status --porcelain)" &&
VERSION="$(python3 -c 'import json; print(json.load(open("release.json"))["version"])')" &&
printf 'Version: %s\nCommit: %s\n' "$VERSION" "$SOURCE" &&
git ls-remote --tags origin "refs/tags/v$VERSION"
```

Stop if any command fails, the displayed commit is not the reviewed release
commit, the version is wrong, or the final command returns an existing tag.
After reviewing the output, create and push the tag:

```sh
git tag "v$VERSION" "$SOURCE" &&
git push origin "refs/tags/v$VERSION"
```

The push triggers publication. Do not move or force-update a release tag to
point to different bytes.

## 4. Monitor the automated release

The tag-triggered workflow checks the identity, then runs **Release Gate
Tests** and the **Release Candidate Artifacts** workflow in parallel. The
publish job requires both to succeed; failed, skipped or cancelled prerequisites
are not success.

| Required coverage | What runs |
| --- | --- |
| Unit and contract checks | Go race tests, Maven verification, offline Python tests and ledger structure checks; Linux cross-builds and redistribution checks |
| Producer and registry integration | Python fixture and Maven producer checks, plus OCI registry publication and verification, on native Linux amd64 and arm64 |
| Native Linux contracts | Overlay/kernel contract checks and `go vet` on both native architectures |
| Kubernetes integration | Real kind-cluster Python E2E on native amd64 and arm64, including admission, execution, rotation, protected identity and cleanup |
| Security analysis | CodeQL for Actions, C#, Go, Java/Kotlin, JavaScript/TypeScript and Python |
| Candidate artifacts | Repeat-build comparison, package and legal inspection, SPDX SBOMs and provenance; native CLI archive identity smoke tests on Linux/macOS amd64/arm64; Maven plugin consumer conformance on Java 17 and 21 |

The exact definitions are in
[`release-gate-tests.yml`](.github/workflows/release-gate-tests.yml),
[`checks.yml`](.github/workflows/checks.yml),
[`e2e.yml`](.github/workflows/e2e.yml) and
[`release-build.yml`](.github/workflows/release-build.yml). Ordinary pull
request validation runs only the checks impacted by that PR, so it is not a
replacement for the release gate.

After both prerequisites pass, the publish job rechecks the exact candidate,
attestations, tag and owner policy before using credentials. It signs the
already-built Maven payloads, transfers the qualified images and chart to
GHCR, submits the signed Maven bundle to Central, publishes the GitHub release,
and independently fetches and verifies the published distributions. It does
not rebuild the artifacts for publication.

Find the run for the tag in Actions, or list and watch it with the GitHub CLI:

```sh
gh run list --repo microsoft/brewlet --workflow release.yml --limit 10
gh run watch "$RUN_ID" --repo microsoft/brewlet --exit-status
```

Set `RUN_ID` to the tag-triggered run ID. Approve the `maven-central`
environment if required, and wait for the **entire** workflow, including
published-distribution verification, to succeed.

### Additional coverage

The weekly, manually dispatchable
[Language Example E2E](.github/workflows/language-e2e.yml) covers Python, Java,
Node.js, .NET, PHP and WebAssembly examples. It is non-gating, and its results
are not qualification evidence. The required release E2E uses the Python
fixture; it does not run the whole language matrix.

Full promotion qualification, including additional native fault and companion
suites, is separate and optional for a release. It requires reviewed,
matching evidence before `make acceptance` can succeed; follow the
[acceptance guide](acceptance/README.md) rather than treating unit tests or the
release gate as full qualification. The website build is also separate from
Release Gate Tests.

## 5. Verify the published release

- [ ] Confirm the tag-triggered workflow completed successfully and that the
  GitHub release has the intended version, notes, prerelease/latest status and
  assets, including CLI archives, checksums, candidate metadata, SBOMs and
  attestations.
- [ ] Confirm the versioned operator, admission and node-provisioner images and
  Helm chart are available in GHCR, and the matching Maven plugin is available
  in Central. The CLI targets Linux/macOS amd64 and arm64; images target Linux
  amd64 and arm64.
- [ ] If independently repeating verification, use the trusted source checkout,
  the reviewed full commit SHA in `SOURCE`, ghx, ORAS, Helm, Go and the trusted
  Maven public key:

  ```sh
  python3 scripts/verify_release.py --source "$SOURCE"
  ```

  This fetches and verifies published bytes, provenance and Maven signatures;
  configure any required read credentials through the normal CLI environment.
- [ ] Check that any coordinated website/documentation update has deployed
  separately and does not advertise an unavailable or unsupported release.

## If a release fails

Publication across GHCR, Maven Central and GitHub is not transactional. A
failure after publication starts may leave some components published; the
GitHub release can already be visible even if the final verification fails.
Do not treat a partial release as complete.

Preserve the workflow's `publication-reconciliation` artifact and any Central
deployment ID, inspect existing immutable artifacts and reconcile an in-flight
or partial Central deployment before retrying. Do not blindly resubmit, replace
different published bytes or retag a different commit. If a source fix is
needed, review a new commit and use a new version/tag.
