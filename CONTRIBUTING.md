# Contributing to Brewlet

Thank you for your interest in Brewlet. Contributions of code, tests,
documentation, and issue reports are welcome.

## Before you start

- Search the existing issues and pull requests before opening a new one.
- Open an issue before making a substantial behavioral or architectural change.
- Follow the [Microsoft Open Source Code of Conduct](CODE_OF_CONDUCT.md).
- Report security vulnerabilities privately as described in
  [SECURITY.md](SECURITY.md).

## Development workflow

Development requires Go 1.26 or newer. Maven plugin development requires Maven
3.9 and JDK 17 or newer. Docker with Buildx is required for provisioner images.
Additional environment requirements are documented in the
[README](README.md#build-and-test).

1. Fork the repository and create a focused branch.
2. Make the smallest coherent change that solves the issue.
3. Add or update tests and documentation for changed behavior.
4. Run the repository checks:

   ```bash
   make check-all
   ```

5. Open a pull request that explains the problem, the solution, and any
   compatibility or operational impact.

Cluster-dependent end-to-end tests are documented in
[`integration-tests/AGENTS.md`](integration-tests/AGENTS.md).

## Pull request requirements

- Keep commits and pull requests focused.
- Preserve backward compatibility unless the change is explicitly approved as
  breaking.
- Do not include credentials, proprietary data, or unrelated generated files.
- Ensure commits contain only work that you have the right to contribute.
- Add the language-appropriate Microsoft MIT copyright header to every new
  source file. Run `make license-check` to verify coverage.

This project welcomes contributions and suggestions. Most contributions require
you to agree to a Contributor License Agreement (CLA) declaring that you have
the right to, and actually do, grant us the rights to use your contribution. For
details, visit <https://cla.opensource.microsoft.com>.

When you submit a pull request, a CLA bot will determine whether you need to
provide a CLA and decorate the pull request appropriately. Follow the bot's
instructions if action is required.
