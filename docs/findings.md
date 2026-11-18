# Findings

Every `AlertRule` carries the findings of its rule in `status.findings`, most severe first, and
their types in `status.findingsSummary` (the FINDINGS column). The `Healthy` condition is
`False` while a critical or warning finding is present, with that finding as its reason.

Inventory findings are recomputed on every sync (an `AlertScope`, `PrometheusRule`,
`Prometheus` or namespace label change triggers one). History findings come from queries to
Prometheus and run once per `quality.resyncInterval` (15 minutes by default); in between, and
when a query could not run, the last verdict stands.

| Finding | Severity | From |
|---|---|---|
| [NotLoaded](#notloaded) | critical | inventory |
| [LoadError](#loaderror) | critical | inventory, history |
| [DeadQuery](#deadquery) | warning | history |
| [Noisy](#noisy) | warning | history |
| [Flapping](#flapping) | warning | history |
| [AlwaysSilenced](#alwayssilenced) | warning | history |
| [Unowned](#unowned) | warning | inventory |
| [NeverFired](#neverfired) | info | history |
| [NoRunbook](#norunbook) | info | inventory |
| [NoSeverity](#noseverity) | info | inventory |

## The window

History verdicts look at `quality.window` (30 days by default, between 1h and 90d), capped to
the retention Prometheus reports in `/api/v1/status/runtimeinfo`. The window actually used is in
`status.effectiveWindow` of the scope and `status.window` of each rule. Behind a Thanos or Mimir
frontend that does not report a retention, the configured window is used as is.

## NotLoaded

**Means:** the alerting rule is in a `PrometheusRule` in scope, but Prometheus does not report it
in `/api/v1/rules`. Whoever wrote it believes they are covered; they are not.

**How:** prometheus-operator writes each `PrometheusRule` to a rule file named
`<namespace>-<name>-<uid>.yaml`; the operator matches loaded groups to objects by that file name
(and by group and alert name for a Prometheus not run by prometheus-operator). With
`spec.prometheus.resourceRef`, `status.notLoadedReason` names the cause:

- the object's labels do not match the Prometheus `ruleSelector` (a `ruleSelector` that is not
  set selects nothing);
- the namespace is outside `ruleNamespaceSelector` (not set: only the Prometheus object's own
  namespace);
- neither: prometheus-operator probably rejected the rule; its logs say why.

**Fix:** add the label the `ruleSelector` asks for (kube-prometheus-stack: `release: <name>`), or
widen the selectors.

**False positives:** a rule created less than five minutes ago without a known reason is shown as
not loaded but without the finding, while Prometheus reloads.

## LoadError

**Means:** Prometheus loaded the rule but its last evaluation failed (`health: err`,
`lastError`), or the expression does not parse at all.

**Fix:** the message carries Prometheus' error, for example `many-to-many matching not allowed`.
`promtool check rules` reproduces parse errors locally.

## DeadQuery

**Means:** the expression can never return anything, because selectors in it match no series:
the metric was renamed, a label value is wrong, the exporter is gone.

**How:** each selector is asked `count(last_over_time(<selector>[1h]))` (the selector's own range
when longer), once per sync however many rules share it. A rule is DeadQuery only when the empty
selectors make the whole expression empty under PromQL's rules: `a or b` needs both sides empty,
`a unless b` only `a`, `a and b` and arithmetic between vectors either side. Selectors inside
`absent()` and `absent_over_time()` are not checked: empty is their point. The message names the
empty selectors.

**False positives:** a series that appears less than once an hour (a nightly job's metric) looks
dead between appearances.

## Noisy

**Means:** the alert became active more than `quality.noisy.firingsAbove` times (50) in the
window. Rules like this teach people to ignore pages.

**How:** `changes(ALERTS_FOR_STATE{alertname=...}[window])` plus one per series: every
activation, pending or firing, of every alert of the rule. Series are matched to the rule by
alert name and by the rule's labels that are not templated, so two rules with the same alert name
are counted apart.

**Fix:** raise `for:`, add `keep_firing_for:`, fix the threshold, or route it somewhere that is
not a pager.

## Flapping

**Means:** activations per day above `quality.flapping.activationsPerDayAbove` (10) **and** a
typical firing duration below `quality.flapping.medianDurationBelow` (10m), with at least three
activations in the window.

**How:** firing periods come from `count_over_time(ALERTS{alertstate="firing"}[step])` as a range
query, at a step of at least a minute and the group's evaluation interval (five minutes for a
30-day window, to stay under Prometheus' point limit). The typical duration is the median of
those periods; when the alert flips faster than the step can separate, it is the firing time
divided by the activations.

**Fix:** `for:` and `keep_firing_for:` smooth a signal that hovers around its threshold.

## AlwaysSilenced

**Means:** more than `quality.alwaysSilenced.silencedFractionAbove` (80 %) of the rule's firing
time was covered by an Alertmanager silence matching its alerts. A rule that is always silenced
is a rule nobody wants; delete or fix it rather than renewing the silence.

**How:** each firing period is intersected with the silences Alertmanager still holds, active and
expired (Alertmanager keeps expired silences for five days by default, so older history counts
as unsilenced). Needs `spec.alertmanager`.

## Unowned

**Means:** no source in `spec.ownership.teamFrom` names a team. Default order: rule label `team`,
label `hs/team` on the PrometheusRule, label `hs/team` on its namespace. The team goes into
`status.team` and the `hs/team` label of the `AlertRule`.

**Fix:** label the namespace (`kubectl label ns team-payments hs/team=payments`): one label
covers every rule in it.

## NeverFired

**Means:** no activation in the window, for a rule the operator has known for at least
`quality.neverFired.after` (30 days). Informational: a quiet rule can be a good rule. Together
with DeadQuery it is not.

## NoRunbook

**Means:** the `runbook_url` annotation is missing or empty, with `ownership.requireRunbook`
(default true).

## NoSeverity

**Means:** the `severity` label is missing or not one of `ownership.requireSeverity` (default
`critical`, `warning`, `info`).

## Query budget

The analysis is bounded by `spec.queryBudget`: a timeout per query (20s), queries in flight (2)
and queries per minute (120), and one sync stops at half the resync interval. A rule group costs
two queries plus one per distinct selector. What does not fit waits for the next sync with its
previous verdict; the scope shows `Ready=False, BudgetExhausted` and
`hs_alert_query_budget_exhausted_total` counts it.
