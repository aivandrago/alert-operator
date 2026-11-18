# Install

alert-operator reads Prometheus, Alertmanager and your `PrometheusRule` objects and writes one
read-only `AlertRule` object per alerting rule, with its owner and a quality verdict. It changes
nothing else (see [safety.md](safety.md)).

## Prerequisites

- Kubernetes 1.30 or later (the chart protects `AlertRule` objects with a
  ValidatingAdmissionPolicy, GA since 1.30).
- [prometheus-operator](https://prometheus-operator.dev)'s CRDs (`PrometheusRule`,
  `Prometheus`), for example from kube-prometheus-stack. The operator watches them and does not
  start without them.
- A Prometheus the operator can reach over HTTP(S), and optionally its Alertmanager (for
  `AlwaysSilenced`). Any server that speaks the Prometheus HTTP API for `/api/v1/rules` and
  `/api/v1/query` works, a Thanos Query or Mimir endpoint included.

## Helm

The chart is published as an OCI artifact on ghcr.io and on charts.hypersurgery.dev; both
carry the same signed release:

```sh
helm install alert-operator oci://ghcr.io/aivandrago/charts/alert-operator --version 0.2.0 \
  --namespace alert-operator --create-namespace
```

or

```sh
helm repo add hypersurgery https://charts.hypersurgery.dev
helm install alert-operator hypersurgery/alert-operator --version 0.2.0 \
  --namespace alert-operator --create-namespace
```

The image is `ghcr.io/aivandrago/alert-operator:0.2.0`, for linux/amd64 and linux/arm64.
Image and chart are signed with cosign keyless by the release workflow; check them before
installing:

```sh
cosign verify ghcr.io/aivandrago/alert-operator:0.2.0 \
  --certificate-identity-regexp '^https://github.com/aivandrago/alert-operator/\.github/workflows/release\.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
cosign verify ghcr.io/aivandrago/charts/alert-operator:0.2.0 \
  --certificate-identity-regexp '^https://github.com/aivandrago/alert-operator/\.github/workflows/release\.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

From a checkout, with an image of your own: `make docker-build docker-push IMG=...` and
`helm install alert-operator charts/alert-operator --set image.repository=... --set image.tag=...`.

Useful values (all in [values.yaml](../charts/alert-operator/values.yaml)):

| Value | Default | What it does |
|---|---|---|
| `scope.create`, `scope.spec` | `false` | Create an `AlertScope` with the chart. |
| `metrics.serviceMonitor.enabled` | `false` | A ServiceMonitor for the operator's own metrics. Bind the scraping Prometheus' service account to the `<release>-metrics-reader` ClusterRole. |
| `prometheusRule.enabled` | `false` | The operator's own alerts (sync stale, Prometheus unreachable, budget exhausted). |
| `grafanaDashboard.enabled` | `false` | The "Alert quality" dashboard as a ConfigMap for the Grafana sidecar. The JSON is also in [deploy/grafana](../deploy/grafana/alert-quality.json). |
| `networkPolicy.enabled` | `false` | Egress only to DNS, the API server (`networkPolicy.apiServer.cidrs`) and the Prometheus/Alertmanager peers; ingress only to the metrics port. |
| `rbac.aggregateToView` | `true` | Everyone with `view` in a namespace can read its `AlertRule` objects. |
| `alertRuleProtection.enabled` | `true` | Only the operator may write `AlertRule` objects. |
| `webhook.enabled` | `false` | The validating webhook for `PrometheusRule` objects ([validation.md](validation.md)). |
| `webhook.defaultMode` | `warn` | Mode for namespaces without the label `alerts.hypersurgery.dev/validation`. |
| `webhook.failurePolicy` | `Ignore` | What the API server does when the webhook does not answer. |
| `webhook.certificate.certManager` | `auto` | cert-manager when installed, else a certificate signed by the chart. |

## The first AlertScope

One `AlertScope` per Prometheus. With kube-prometheus-stack installed as `kps` in `monitoring`:

```yaml
apiVersion: alerts.hypersurgery.dev/v1alpha1
kind: AlertScope
metadata:
  name: main
spec:
  prometheus:
    url: http://prometheus-operated.monitoring.svc:9090
    # With this, a rule that is defined but not loaded comes with the reason.
    resourceRef: { namespace: monitoring, name: kps-kube-prometheus-stack-prometheus }
  alertmanager:
    url: http://alertmanager-operated.monitoring.svc:9093
```

Everything else has defaults: all namespaces, team from the rule label `team`, then the label
`hs/team` on the PrometheusRule, then on its namespace; a 30-day window capped to the retention;
a sync every 15 minutes; at most two queries at a time and 120 a minute. The full list is in the
[API reference](reference/api.md); the thresholds are explained in [findings.md](findings.md).

If Prometheus needs credentials, put them in a Secret in the operator's namespace and name it in
`authSecretRef`: keys `token` (bearer), `username`/`password` (basic auth), `ca.crt`, and
`tls.crt`/`tls.key` (client certificate).

## Reading the report card

```console
$ kubectl get alertscope
NAME   RULES   LOADED   FINDINGS   WINDOW   READY   LAST SYNC
main   214     199      58         15d      True    2m

$ kubectl get alertrules -A
NAMESPACE       NAME                         ALERT                 TEAM       LOADED   FIRED   LAST FIRED   FINDINGS
team-payments   paymentshighlatency-3f9c1a   PaymentsHighLatency   payments   true     212     40m          Noisy,NoRunbook
team-web        webdown-7c01d2               WebDown               <none>     false    0                    NotLoaded,Unowned
```

- `kubectl get alertscope main -o yaml` has the counts per finding and per team in `status`.
- `kubectl get alertrules -n <namespace>` is one team's clean-up list; `-o yaml` shows each
  finding's message, the measured quality and, for a rule that is not loaded, the reason.
- `kubectl get events --field-selector reason=FindingAdded -A` lists rules that gained a finding.
- Metrics and example queries: [metrics.md](metrics.md).

The first sync measures what history there is. A Prometheus that started yesterday gives
one-day verdicts (`WINDOW` says so), and `NeverFired` only appears for rules the operator has
known for `quality.neverFired.after` (30 days by default).

## Troubleshooting

The `Ready` condition of the `AlertScope` says what is wrong:

| Reason | Meaning |
|---|---|
| `Synced` | All good. |
| `PrometheusUnreachable` | `/api/v1/rules` failed (network, 5xx, 401/403). Nothing is deleted; verdicts stay as they were and `lastEvaluated` stops moving. |
| `AlertmanagerUnreachable` | Silences could not be read; `AlwaysSilenced` keeps its previous verdicts. |
| `BudgetExhausted` | The query budget ran out before everything was measured; the rest is measured in the next sync. Raise `queryBudget.maxQueriesPerMinute` or `quality.resyncInterval`. |
| `Conflict` | An object that is not the operator's has the name an `AlertRule` should get; `status.conflicts` lists them. The operator leaves such objects alone. |
| `InvalidSpec` | The auth Secret is missing or malformed, or a URL does not parse. |

The operator's logs are JSON; `--zap-log-level=debug` (chart value `logLevel: debug`) adds detail.

## Uninstall

```sh
kubectl delete alertscopes --all   # the operator deletes their AlertRule objects
helm uninstall alert-operator -n alert-operator
kubectl delete crd alertscopes.alerts.hypersurgery.dev alertrules.alerts.hypersurgery.dev
```
