# alert-operator

A Kubernetes operator that keeps an always-current **inventory of your Prometheus alert rules
with an owner and a quality verdict for each one**: which rules never loaded, which can never
fire, which are noisy, flapping or permanently silenced, and which nobody owns.

Part of the hypersurgery family, next to
[subnet-operator](https://hypersurgery.dev). API group `alerts.hypersurgery.dev`, metrics
`hs_alert_...`.

## Status

**The latest release is v0.2.0**, alpha: the API is `v1alpha1` and may change before 1.0.
Install it with Helm ([docs/install.md](docs/install.md)). What it does:

- Joins the alerting rules of `PrometheusRule` objects with what Prometheus actually loaded and
  publishes one read-only `AlertRule` per rule, in the rule's namespace, with its team.
- Findings: `NotLoaded` (with the selector that is to blame), `LoadError`, `DeadQuery`, `Noisy`,
  `Flapping`, `AlwaysSilenced`, `NeverFired`, `Unowned`, `NoRunbook`, `NoSeverity`
  ([docs/findings.md](docs/findings.md)).
- Per-scope and per-team counts in the `AlertScope` status, `hs_alert_*` metrics
  ([docs/metrics.md](docs/metrics.md)), Events when a rule gains or loses a finding, a Grafana
  dashboard, a Helm chart ([docs/install.md](docs/install.md)).
- Changes nothing it did not create ([docs/safety.md](docs/safety.md)).
- From 0.2: the same checks at the door, as an opt-in validating webhook for `PrometheusRule`
  (warn mode, enforce per namespace; [docs/validation.md](docs/validation.md)), and in CI with
  the `hsalert lint` CLI ([docs/cli.md](docs/cli.md)).

What is proven and how:

| Check | Command | Where it ran |
|---|---|---|
| Unit and envtest (API validation, admission policy, reconciler lifecycle, analyzers against a real Prometheus with backfilled history, chart RBAC) | `make test` | CI |
| The whole operator against a real kube-apiserver, Prometheus and Alertmanager | `make test-integration` | CI |
| Install with the chart next to kube-prometheus-stack in Kind; the webhook denies and warns; `hsalert lint` exit codes | `make test-e2e` | CI |
| Lint, Semgrep, govulncheck | `make lint semgrep govulncheck` | CI |

Not yet: changing any rule, route or silence (0.3, 0.4); several clusters (0.5). The roadmap is
in [docs/design.md](docs/design.md#8-roadmap) and in the milestones of
this repository.

## In one paragraph

`PrometheusRule` objects are accepted by `kubectl apply` even when Prometheus will never load
them; rules keep querying metrics that were renamed long ago; a handful of noisy rules train
everyone to ignore pages. The operator reads what Prometheus actually loaded, joins it with
the `PrometheusRule` objects that should have been loaded, grades every rule from Prometheus'
own history, and publishes one read-only `AlertRule` object per rule, Prometheus metrics and
a per-team summary. Version 0.1 only reads: it never changes a rule, a route or a silence.

## License

Apache-2.0, see [LICENSE](LICENSE).
