# Releasing and dependency updates

## Versioning

[Semantic Versioning](https://semver.org). Tags are `vMAJOR.MINOR.PATCH`. While the major version is `0`, tool schemas may change in a minor release; from `1.0.0`, a breaking tool-schema change requires a major bump.

## Cutting a release

```sh
git checkout main && git pull
git tag -a v0.2.0 -m "v0.2.0"
git push origin v0.2.0
```

The `Release` workflow runs GoReleaser: it builds linux/darwin/windows for amd64 and arm64 (`CGO_ENABLED=0`, `-trimpath`), publishes archives and `checksums.txt`, and writes release notes from Conventional Commit messages. `go install ...@v0.2.0` works as soon as the tag exists.

## Dependabot

`.github/dependabot.yml` checks Go modules daily and GitHub Actions weekly.

`.github/workflows/dependabot-automerge.yml` enables squash auto-merge on every Dependabot PR: patch, minor and major version bumps, and security updates (the ones that carry a GHSA/CVE id). Major bumps also get a `major-update` label so they are easy to spot in the history.

"Auto-merge" means GitHub merges the PR only after the required status check `ci-ok` passes (Linux, macOS and Windows tests, `govulncheck`, cross-compilation, `goreleaser check`). A failing update is never merged.

Repository settings this relies on (already applied by the maintainer):

- Settings → General → *Allow auto-merge* on;
- branch protection on `main` requiring the `ci-ok` check;
- Dependabot alerts and Dependabot security updates on.

The merge gate is CI. If you want a human to look at major updates, remove the auto-merge step for them in the workflow.
