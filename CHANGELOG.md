# Changelog

All notable changes to alert-operator. The chart's `artifacthub.io/changes` annotation carries
the same list for each release.

## v0.3.0 — 2026-11-20

Silences as code: the first version that writes outside its own objects, only when asked to,
and only silences it created.

### Added

- `AlertSilence` (namespaced, `hsasil`): matchers, an owner, a reason and exactly one of
  `expiresAt` and `duration`; the operator creates the silence in the scope's Alertmanager, keeps
  it in step with the spec and expires it when the object is deleted ([docs/silences.md](docs/silences.md)).
- `AlertScope.spec.silences`: `mode` (Off, Propose, Apply; Off by default), `maxDuration` (7d),
  `restrictToNamespaceRules` (true: only alerts of the namespace's own rules).
- Opt-in writes: the manager writes only with `--enable-writes` (chart `writes.enabled`, false by
  default). In Propose mode, with `dryRun`, or without writes, the status shows the silence and
  the alerts it would match, and nothing is written.
- Every silence written carries `createdBy: alert-operator` and a marker with the object's UID,
  checked before every update or expiry; a silence ended by a person is not recreated; deletion
  during an Alertmanager outage retries for ten minutes.
- Events and a JSON audit line per write; metrics `hs_alert_silence_writes_total` and
  `hs_alert_silences`.
- The admission policy also protects `alertsilences/status`; the chart's aggregated roles let
  namespace viewers read and editors write AlertSilence objects.

### Fixed

- The AlertRule objects of an AlertScope deleted while no replica held the leader lease stayed;
  the next leader now removes them.

## v0.2.0 — 2026-11-18

Validation at the door: the checks of 0.1 before a rule reaches the cluster, at admission and in
CI. Still writes nothing outside the operator's own objects.

### Added

- An opt-in validating admission webhook for `PrometheusRule` (`webhook.enabled`, off by
  default): `LoadError`, `NotLoaded` (the Prometheus selectors would not load the object),
  `Unowned`, `NoSeverity`, `NoRunbook` and `DeadQuery`, with the settings of the AlertScope that
  selects the namespace ([docs/validation.md](docs/validation.md)).
- Modes per namespace with the label `alerts.hypersurgery.dev/validation`: `enforce` denies on a
  blocking problem, `warn` (the default, `webhook.defaultMode`) admits with warnings, `off` skips
  the namespace. DeadQuery never blocks.
- `failurePolicy: Ignore` and a 5-second timeout; at most 2 seconds per review on Prometheus,
  answers cached for 5 minutes.
- The webhook certificate from cert-manager, signed by the chart, or an existing Secret.
- `hsalert lint`, the same checks for CI pipelines: PrometheusRule manifests and plain rule
  files, text, JSON and GitHub annotations, exit codes 0/1/2 ([docs/cli.md](docs/cli.md)).
  Released for linux and darwin on amd64 and arm64, with cosign-signed checksums.
- Metrics `hs_alert_webhook_reviews_total{mode,result}` and
  `hs_alert_webhook_review_duration_seconds`.

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
