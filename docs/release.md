# Cutting a release

The steps to release `vX.Y.Z`, in order. Development happens on the project's Gitea; the public
repository `github.com/aivandrago/alert-operator` carries only released snapshots, and its
release workflow builds, signs and publishes the image and the chart.

## 1. Before the release commit

- `master` is green in Gitea CI: `lint` (golangci-lint, Semgrep, govulncheck on the binary),
  `test` (generated files, chart, unit and envtest), `integration` and `e2e`.
- govulncheck asks a live database: run `make govulncheck` again on the day of the release. A
  vulnerability disclosed since the last push fails it without any change to the code and is
  fixed by the dependency bump it names.
- `artifacthub.io/changes` in `charts/alert-operator/Chart.yaml` lists this release's changes
  and nothing of the previous one.

## 2. The release commit

One commit, `Release vX.Y.Z`, touching only:

| File | What changes |
|---|---|
| `charts/alert-operator/Chart.yaml` | `version: X.Y.Z`, `appVersion: "X.Y.Z"`; `artifacthub.io/images` (`ghcr.io/aivandrago/alert-operator:X.Y.Z`); `artifacthub.io/prerelease` (`"true"` for 0.x and pre-releases); `artifacthub.io/changes`. The release workflow refuses a tag that does not match these |
| `CHANGELOG.md` | The release's section, dated |
| `README.md` | The status section: "The latest release is vX.Y.Z" and what it proves |
| `docs/install.md` | The version in the install and verify commands |
| `test/chart/testdata/default.yaml` | The render snapshot, which carries the chart version (`UPDATE_GOLDEN=1 go test ./test/chart/`) |
| `site/alert-operator/index.html` | The version pill and the roadmap phase. The page itself lives in the sibling's site and is published from there |

Run `make manifests generate api-docs helm-crds` first; `git status` must show the files above
and nothing else.

## 3. Tag

An annotated tag on the release commit, after CI is green on it:

```sh
git tag -a vX.Y.Z -m "alert-operator vX.Y.Z"
git -c http.extraHeader="Authorization: token $(cat /etc/gitea-mcp/token)" push origin vX.Y.Z
```

## 4. Publish

From the clean release clone, with the tag on Gitea:

1. `hack/publish-public.sh`: the working tree of master as one commit authored by Ivan Drago,
   force-pushed over `main` of `github.com/aivandrago/alert-operator` (approved for this
   repository's `main` only), the tag `vX.Y.Z` pushed once and never moved, and a GitHub
   release marked latest.
2. The tag starts `.github/workflows/release.yml` on GitHub: the multi-arch image
   `ghcr.io/aivandrago/alert-operator:X.Y.Z` (also `:vX.Y.Z` and `:latest`) with SBOM and
   provenance, the chart as `oci://ghcr.io/aivandrago/charts/alert-operator`, both signed with
   cosign keyless and verified in the job; the SBOM and the chart package are attached to the
   GitHub release.
3. `make helm-publish` with the ChartMuseum credentials: the same chart on
   `https://charts.hypersurgery.dev`.
4. `gh release edit vX.Y.Z --notes-file` with the CHANGELOG section.

## 5. After publishing

- Verify both signatures:
  ```sh
  cosign verify ghcr.io/aivandrago/alert-operator:X.Y.Z \
    --certificate-identity-regexp '^https://github.com/aivandrago/alert-operator/\.github/workflows/release\.yml@' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
  cosign verify ghcr.io/aivandrago/charts/alert-operator:X.Y.Z \
    --certificate-identity-regexp '^https://github.com/aivandrago/alert-operator/\.github/workflows/release\.yml@' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
  ```
- Install from the published chart into a fresh Kind cluster and run `kubectl get alertscope`.
- The GitHub workflows (CI, Security, CodeQL, Scorecard) on the new `main` are green.
- Close the milestone.

## Decisions (owner, 2026-11-18)

1. The image and the chart are published like subnet-operator's: `ghcr.io/aivandrago/alert-operator`
   and `oci://ghcr.io/aivandrago/charts/alert-operator`, signed, plus `https://charts.hypersurgery.dev`.
2. The public repository is `github.com/aivandrago/alert-operator`, one snapshot commit per
   release force-pushed over `main` (`hack/publish-public.sh`), as for subnet-operator.
3. A release is cut as soon as master CI is green on the release commit.
