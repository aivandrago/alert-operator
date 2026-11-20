# Continuous integration

CI runs in Gitea Actions from [`.gitea/workflows/ci.yml`](../.gitea/workflows/ci.yml).

| Job | What it runs | Needs |
|---|---|---|
| `lint` | `make lint` (golangci-lint with the logcheck plugin), `make semgrep`, `make govulncheck` (binary mode) | Go, Docker (Semgrep image) |
| `test` | generated code and CRDs up to date, `make test` (unit tests and envtest), `make build` | Go |
| `e2e` | `make test-e2e`: Kind, Prometheus, Alertmanager and the operator from the Helm chart | Go, Docker, about 4 GB of memory |

## Runner

Every job asks for the label **`operator-alerts`**: a runner that serves this repository only, so
the CI of this project neither waits for nor delays other projects.

The runner lives on its own VM (VM 114): repository-scoped, capacity 1, 6 GB of memory plus 4 GB
of swap, Docker for Kind. One job runs at a time, so the e2e job's Kind cluster with
kube-prometheus-stack has the machine to itself.

The public mirror on GitHub runs `.github/workflows` (lint, tests, integration, security scans,
CodeQL, Scorecard, and the release on a tag); Gitea Actions reads only `.gitea/workflows`.

## Running the checks locally

```sh
make lint semgrep govulncheck   # semgrep: Docker, or a semgrep on PATH
make test build
make test-e2e                   # needs Docker for Kind
```

govulncheck runs against the built binary. The source scan (`make govulncheck-source`) builds the
whole call graph and needs about 9 GB of memory, which is why CI does not run it.
