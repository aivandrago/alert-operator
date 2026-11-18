# Metrics

The manager serves these on its metrics endpoint (HTTPS with authentication by default, port
8443). Every name starts with `hs_alert_`. Labels are kept low-cardinality: never an
expression, a message or any other free text, and at most one series per rule and finding.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `hs_alert_rules` | gauge | `scope`, `team`, `namespace`, `loaded` | Alerting rules in the scope. |
| `hs_alert_rule_findings` | gauge | `scope`, `namespace`, `team`, `alertname`, `finding`, `severity` | 1 per active finding of a rule (more only when several rules share namespace, team and alert name). |
| `hs_alert_rule_activations` | gauge | `scope`, `namespace`, `team`, `alertname` | Activations in the effective window. Present once the rule has been measured. |
| `hs_alert_rule_firing_fraction` | gauge | same | Share of the effective window the alert was firing. |
| `hs_alert_scope_effective_window_seconds` | gauge | `scope` | The configured window, capped to the retention Prometheus reports. |
| `hs_alert_scope_sync_duration_seconds` | histogram | `scope` | Duration of a sync. |
| `hs_alert_scope_last_sync_timestamp_seconds` | gauge | `scope` | Time of the last complete sync. |
| `hs_alert_query_total` | counter | `scope`, `target`, `result` | Queries to `prometheus` and `alertmanager`; `result` is `ok`, `error` (refused query), `unreachable`, `unauthorized` or `budget_exhausted`. |
| `hs_alert_query_budget_exhausted_total` | counter | `scope` | Work deferred to the next sync because the query budget ran out. |
| `hs_alert_webhook_reviews_total` | counter | `mode`, `result` | PrometheusRule admission reviews; `result` is `allowed`, `warned` or `denied` ([validation.md](validation.md)). |
| `hs_alert_webhook_review_duration_seconds` | histogram | - | Time to answer a review. |

The per-rule and per-scope gauges are served from the snapshot of each scope's last sync: a
rule or a finding that disappears is gone from the next scrape, and deleting an `AlertScope`
removes its series.

## Examples

```promql
# Rules per finding, cluster-wide
sum by (finding) (hs_alert_rule_findings)

# The ten noisiest rules
topk(10, hs_alert_rule_activations)

# Rules Prometheus never loaded, per team
sum by (team) (hs_alert_rules{loaded="false"})

# The operator falls behind: syncs older than twice the resync interval
time() - hs_alert_scope_last_sync_timestamp_seconds > 1800
```
