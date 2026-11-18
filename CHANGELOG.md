# Changelog

All notable changes to alert-operator. The chart's `artifacthub.io/changes` annotation carries
the same list for each release.

## v0.1.0 — 2026-11-18

The first release: a read-only inventory of Prometheus alerting rules with an owner and a
quality verdict for each. Alpha; the API is `alerts.hypersurgery.dev/v1alpha1`.

### Added

- `AlertScope` (cluster-scoped, `hsas`): one Prometheus and optionally its Alertmanager, the
  namespaces in scope, where the owning team comes from, the history window and thresholds, and
  a query budget.
- `AlertRule` (namespaced, `hsar`): a read-only mirror of each alerting rule in the namespace
  of its `PrometheusRule`, with team, measured quality (activations, typical firing duration,
  firing and silenced fractions, last fired) and findings.
- Findings: `NotLoaded` with the selector that excluded the rule, `LoadError`, `DeadQuery`,
  `Noisy`, `Flapping`, `AlwaysSilenced`, `NeverFired`, `Unowned`, `NoRunbook`, `NoSeverity`
  ([docs/findings.md](docs/findings.md)).
- Scope status with counts per finding and per team; Events `FindingAdded` and
  `FindingCleared`; `hs_alert_*` metrics ([docs/metrics.md](docs/metrics.md)).
- Helm chart with least-privilege RBAC, a ValidatingAdmissionPolicy that lets only the
  operator write `AlertRule` objects, optional NetworkPolicy, ServiceMonitor, the operator's own
  alerts and the "Alert quality" Grafana dashboard.
- Multi-arch image (linux/amd64, linux/arm64) and chart, signed with cosign keyless, with an SBOM.

### Safety

- Reads Prometheus and Alertmanager with GET requests only, under a per-scope query budget.
- Writes only its own `AlertRule` objects, `AlertScope` status and Events; never deletes on a
  failed or partial read ([docs/safety.md](docs/safety.md)).
