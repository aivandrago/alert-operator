# hsalert lint

`hsalert lint` runs the webhook's checks ([validation.md](validation.md)) on rule files, so a
pull request learns about a broken or unowned rule before it reaches a cluster.

## Install

Every GitHub release carries `hsalert-linux-amd64`, `hsalert-linux-arm64`,
`hsalert-darwin-amd64`, `hsalert-darwin-arm64`, a `SHA256SUMS` and its cosign signature bundle:

```sh
v=0.2.0
base=https://github.com/aivandrago/alert-operator/releases/download/v$v
curl -fsSLO $base/hsalert-linux-amd64 -O $base/SHA256SUMS -O $base/SHA256SUMS.sigstore.json
cosign verify-blob SHA256SUMS --bundle SHA256SUMS.sigstore.json \
  --certificate-identity-regexp '^https://github.com/aivandrago/alert-operator/\.github/workflows/release\.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --check --ignore-missing SHA256SUMS
install -m 0755 hsalert-linux-amd64 /usr/local/bin/hsalert
```

From a checkout: `make build-cli` (all four into `dist/`) or `go build ./cmd/hsalert`.

## Usage

```sh
hsalert lint [flags] FILE...
```

Files hold PrometheusRule manifests (several documents per file; other kinds are skipped) or
plain Prometheus rule files (`groups:`); `-` reads stdin. Unknown fields are errors.

| Flag | Default | Meaning |
|---|---|---|
| `--team-from` | `rule:team,object:hs/team,namespace:hs/team` | Where the owner comes from, first match wins. |
| `--require-runbook` | `true` | Report rules without `runbook_url`. |
| `--severities` | `critical,warning,info` | Allowed `severity` values. |
| `--prometheus FILE` | - | A Prometheus object manifest; objects its selectors would not load are NotLoaded. |
| `--namespace`, `--namespace-labels` | `default`, - | The namespace the rules go to and its labels (for `namespace:` owners and `ruleNamespaceSelector`). |
| `--prometheus-url`, `--bearer-token-file` | - | A Prometheus to ask whether selectors match series (DeadQuery, a warning). |
| `--format` | `text` | `text`, `json`, or `github` (workflow annotations on the files). |
| `--strict` | `false` | Fail on warnings too. |

Exit codes: **0** no blocking problem, **1** at least one blocking problem (any problem with
`--strict`), **2** usage or read error. Checks that could not run are noted on stderr.

```console
$ hsalert lint rules/payments.yaml
rules/payments.yaml: team-payments/payments: error: payments/PaymentsHighLatency: NoRunbook: annotation runbook_url is missing
1 objects, 1 problems, 1 blocking
```

## In GitHub Actions

```yaml
- name: Lint alert rules
  run: |
    curl -fsSLo hsalert https://github.com/aivandrago/alert-operator/releases/download/v0.2.0/hsalert-linux-amd64
    chmod +x hsalert
    kubectl get prometheus k8s -n monitoring -o yaml > prometheus.yaml   # or commit a copy
    ./hsalert lint --format github --prometheus prometheus.yaml deploy/rules/*.yaml
```

The `github` format turns each problem into an error or warning annotation on the file.
