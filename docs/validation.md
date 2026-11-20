# Validation at the door

From 0.2 the operator can check `PrometheusRule` objects when they are applied, with the same
checks `hsalert lint` runs in CI ([cli.md](cli.md)). It is an opt-in validating admission
webhook: off unless the chart value `webhook.enabled` is true, in warn mode when on, and it
writes nothing.

## Turning it on

```sh
helm upgrade alert-operator oci://ghcr.io/aivandrago/charts/alert-operator --version 0.3.0 \
  --namespace alert-operator --reuse-values --set webhook.enabled=true
```

Every namespace is now in warn mode: objects are admitted, and kubectl prints what is wrong.

```console
$ kubectl apply -n team-payments -f payments-rules.yaml
Warning: alert-operator: payments/PaymentsHighLatency: NoRunbook: annotation runbook_url is missing
prometheusrule.monitoring.coreos.com/payments created
```

A namespace whose rules are clean can then refuse new problems:

```sh
kubectl label namespace team-payments alerts.hypersurgery.dev/validation=enforce
```

```console
$ kubectl apply -n team-payments -f payments-rules.yaml
Error from server: error when creating "payments-rules.yaml": admission webhook
"prometheusrules.alerts.hypersurgery.dev" denied the request: alert-operator (namespace
team-payments enforces alerts.hypersurgery.dev/validation): payments/PaymentsHighLatency:
NoRunbook: annotation runbook_url is missing
```

## Modes

The namespace label `alerts.hypersurgery.dev/validation`:

| Value | Effect |
|---|---|
| `enforce` | Denied when a blocking problem exists; the other problems come back as warnings. |
| `warn` | Always admitted; every problem as a warning. |
| `off` | Not sent to the webhook at all. |
| absent | `webhook.defaultMode` (`warn`; set `enforce` to make enforcement the rule and `warn` the exception). |

## What is checked

The checks are those of [design.md §10.1](design.md#101-checks), with the settings of the first
`AlertScope` (by name) whose `namespaceSelector` selects the namespace:

| Check | Blocks in enforce mode | Needs |
|---|---|---|
| `LoadError`: an expression does not parse | yes | - |
| `NotLoaded`: the Prometheus `ruleSelector`/`ruleNamespaceSelector` would not load the object | yes | the scope's `prometheus.resourceRef` |
| `Unowned`, `NoSeverity`, `NoRunbook` | yes | the scope's `ownership` (`requireRunbook: false` turns NoRunbook off) |
| `DeadQuery`: a selector without series in the last hour | **no**, a warning | the scope's Prometheus |

DeadQuery never blocks: rules are often applied before the application that exports the
metric. Without a scope for the namespace, the default ownership rules apply and the Prometheus
checks are skipped; the warnings say so (`not checked: ...`).

## Failure policy and performance

The webhook is registered with `failurePolicy: Ignore` and `timeoutSeconds: 5`: an operator that
is down, being upgraded or slow never stops a `PrometheusRule` change, it only lets it in
unchecked. Set `webhook.failurePolicy: Fail` only if a rule that skips validation is worse for
you than a rule change that cannot be applied during an outage.

A review reads namespaces, scopes and Prometheus objects from the operator's cache. Only
DeadQuery calls out, to Prometheus, with at most 2 seconds per review; answers are cached for 5
minutes per selector, and a check that does not answer in time is skipped with a warning.
Every replica serves the webhook.

## Certificates

`webhook.certificate.certManager: auto` (the default) uses cert-manager when its API is
installed: a self-signed Issuer of the chart's own, or yours with `certificate.issuerRef`, and
the CA injected by cert-manager. Without cert-manager the chart signs a CA and a certificate
itself, valid for `certificate.duration` (a year), and signs new ones on the first `helm
upgrade` within `renewBefore` (30 days) of expiry. With GitOps tools that render with `helm
template` (Argo CD, Flux) use cert-manager or `certificate.existingSecret` with
`certificate.caBundle`: `helm template` cannot read the certificate back and signs a new CA on
every render.

## Metrics

| Metric | Type | Labels |
|---|---|---|
| `hs_alert_webhook_reviews_total` | counter | `mode` (warn, enforce), `result` (allowed, warned, denied) |
| `hs_alert_webhook_review_duration_seconds` | histogram | - |
