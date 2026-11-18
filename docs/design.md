# Alert Operator — design

Status: **phase 0.1 in progress** (milestone v0.1). This document started as the proposal for
the owner's review; the open questions in §9 carry the defaults the build went ahead with.

Brand and naming follow the hypersurgery family (sibling: subnet-operator,
`network.hypersurgery.dev`): API group `alerts.hypersurgery.dev`, object labels and annotations
under `alerts.hypersurgery.dev/...`, Kubernetes label shorthand `hs/...`, Prometheus metrics
`hs_alert_...`. Go module `hypersurgery.dev/alert-operator`.

## 1. The problem

Every Kubernetes cluster that runs the Prometheus stack accumulates alert rules: the
150-odd rules that kube-prometheus ships, the ones each Helm chart brings along, and the ones
teams write themselves. After a year nobody can answer simple questions about them:

- **Which rules actually run?** A `PrometheusRule` whose labels do not match the Prometheus
  `ruleSelector`, or that lives in a namespace outside `ruleNamespaceSelector`, is accepted by
  `kubectl apply` and then silently ignored. The team believes it is covered. It is not.
- **Which rules can never fire?** A rule written against `http_requests_total` keeps "working"
  after the app renamed the metric to `http_server_requests_seconds_count`. The expression is
  valid, it simply selects no series, forever. Nothing complains.
- **Which rules are noise?** The rule that fired 400 times last month, the one that flaps
  every few minutes, the one that has been silenced for 90% of its firing time. These train
  people to ignore pages, which is how the real outage gets missed.
- **Who owns a rule?** The rule has no team label, no runbook, no severity, and the person who
  wrote it has left.

The existing tools each cover a slice and leave the gap in the middle:

| Tool | What it does | What it does not do |
|---|---|---|
| prometheus-operator (`PrometheusRule`, `AlertmanagerConfig`) | Rules and routes as CRDs, namespace-scoped routes | Says nothing about whether a rule loaded, can fire, or is any good |
| kube-prometheus / mixins | A good default rule set | Defaults are tuned for nobody; no feedback loop |
| pint (Cloudflare) | Lints rules in CI, can check that series exist | A CI/CLI tool: no in-cluster inventory, no ownership, no history-based quality |
| Pyrra, Sloth | Generate rules from SLOs | Only for SLO rules; nothing about the rest |
| Robusta | Enriches and routes alerts, playbooks | Works on alerts as they arrive, not on the rule set; much of the value is in its SaaS |
| giantswarm silence-operator | Silences as CRDs | Only silences |
| Grafana OnCall OSS | On-call, escalation | Archived on 2026-03-24; never judged rule quality |
| PagerDuty / incident.io analytics | Noisy-alert reports | Paid SaaS, sees only what reached it, not rules that never fire or never loaded |
| OpenTelemetry | Signals and collection | No alerting-rule model at all |

Nobody keeps an **in-cluster, always-current inventory of alert rules with an owner and a
measured quality verdict per rule**. That is the gap, and it is the same shape of problem
subnet-operator solved for networks: the spreadsheet (here: the wiki page "our alerts") is
always wrong, so generate the inventory from the system itself and flag what is wrong.

## 2. Chosen scope for v0.1

**A read-only alert-rule inventory with a quality report card.** The operator reads what
Prometheus actually loaded, joins it with the `PrometheusRule` objects that should have been
loaded, measures each rule's behaviour from Prometheus' own history, and publishes the result
as one `AlertRule` object per rule, as Prometheus metrics and as a per-team summary.

Why this and not routing or silences first:

1. **Safe on day one.** It reads Prometheus, Alertmanager and the Kubernetes API, and writes
   only its own objects. Installing it cannot break anyone's paging. (The subnet-operator rule:
   read-only core first, writes later and opt-in.)
2. **Original.** "Defined but not loaded", "can never fire" and "noisy / flapping / always
   silenced" in one place, per owner, in the cluster, is not offered by any open tool above.
3. **Foundation for the rest.** Ownership, stable rule identity and quality scores are what
   admission validation (0.2), silences with expiry (0.3) and team routing (0.4) need anyway.

Not in v0.1: changing any rule, any Alertmanager configuration or any silence; admission
webhooks; multi-cluster; notifications of its own beyond what an ordinary Prometheus rule on
its metrics gives you.

## 3. Users

- **Platform / SRE team** that runs the Prometheus stack: installs the operator, defines the
  scope, looks at the cluster-wide picture ("38 rules never loaded, 12 can never fire").
- **Application teams**: `kubectl get hsar -n team-payments` shows their rules and verdicts;
  the per-team summary is their weekly clean-up list. RBAC on `AlertRule` follows namespaces,
  so a team sees its own rules without cluster-wide rights.
- **On-call lead / engineering manager**: the "noisiest rules this month" view and its trend.

## 4. API (v1alpha1)

### 4.1 `AlertScope` (cluster-scoped, short name `hsas`)

Declares where to read from, which rules to inventory and how to judge them. One per
Prometheus/Alertmanager pair; most clusters have exactly one.

```yaml
apiVersion: alerts.hypersurgery.dev/v1alpha1
kind: AlertScope
metadata:
  name: main
spec:
  prometheus:
    url: http://prometheus-operated.monitoring.svc:9090
    # optional: bearer token / basic auth / TLS from a Secret in the operator namespace
    authSecretRef: { name: prometheus-reader }
    # the Prometheus custom resource, so "defined but not loaded" can explain WHY
    # (ruleSelector / ruleNamespaceSelector mismatch)
    resourceRef: { namespace: monitoring, name: k8s }
  alertmanager:
    url: http://alertmanager-operated.monitoring.svc:9093
  # which PrometheusRule objects belong to this scope (default: all namespaces)
  namespaceSelector:
    matchExpressions:
      - { key: kubernetes.io/metadata.name, operator: NotIn, values: [kube-system] }
  ownership:
    # where the owner comes from, first match wins
    teamFrom:
      - ruleLabel: team                 # label on the alerting rule itself
      - objectLabel: hs/team            # label on the PrometheusRule object
      - namespaceLabel: hs/team         # label on the namespace
    requireRunbook: true                # annotation runbook_url
    requireSeverity: [critical, warning, info]
  quality:
    window: 30d                         # capped to the Prometheus retention actually available
    resyncInterval: 15m
    noisy:     { firingsAbove: 50 }     # activations in the window
    flapping:  { activationsPerDayAbove: 10, medianDurationBelow: 10m }
    neverFired: { after: 30d }          # only once the rule has existed that long
    alwaysSilenced: { silencedFractionAbove: 0.8 }
  queryBudget:
    timeout: 20s
    maxConcurrent: 2
    maxQueriesPerMinute: 120
status:
  conditions:
    - { type: Ready, status: "True", reason: Synced }
  lastSync: "2026-11-07T09:12:44Z"
  effectiveWindow: 15d                  # retention was shorter than 30d
  rules: { total: 214, loaded: 199, notLoaded: 15 }
  findings:                             # counts per finding
    NotLoaded: 15
    DeadQuery: 9
    Noisy: 4
    Flapping: 3
    NeverFired: 61
    AlwaysSilenced: 2
    Unowned: 37
    NoRunbook: 52
  teams:
    - { team: payments, rules: 23, findings: 6 }
    - { team: "<none>", rules: 37, findings: 37 }
```

### 4.2 `AlertRule` (namespaced, short name `hsar`) — a read-only mirror

One object per alerting rule, created **by the operator only**, in the namespace of the
`PrometheusRule` it came from (rules Prometheus loaded from plain files, with no object behind
them, go to the operator's namespace). The spec is a copy of the rule; users do not edit it —
an admission policy in the chart refuses writes from anyone but the operator.

```yaml
apiVersion: alerts.hypersurgery.dev/v1alpha1
kind: AlertRule
metadata:
  name: payments-api-highlatency-3f9c1a
  namespace: team-payments
  labels:
    alerts.hypersurgery.dev/scope: main
    alerts.hypersurgery.dev/managed-by: alert-operator
    hs/team: payments
  ownerReferences:          # garbage-collected with its source, never the other way round
    - { apiVersion: monitoring.coreos.com/v1, kind: PrometheusRule, name: payments-api, controller: false }
spec:
  scopeRef: main
  source:
    kind: PrometheusRule
    namespace: team-payments
    name: payments-api
    group: payments.latency
    alert: PaymentsHighLatency
  expr: histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket{job="payments"}[5m]))) > 0.5
  for: 5m
  labels: { severity: warning, team: payments }
  annotations: { summary: "p99 latency above 500 ms" }
status:
  identity: 3f9c1a7e       # stable id: scope + source + group + alert name (survives expr edits)
  loaded: true
  team: payments
  window: 30d
  lastEvaluated: "2026-11-07T09:12:30Z"
  quality:
    activations: 212
    activationsPerDay: 7.1
    medianFiringDuration: 4m
    firingFraction: 0.06
    silencedFraction: 0.02
    lastFired: "2026-11-07T08:40:00Z"
  findings:
    - type: Noisy
      severity: warning
      message: "212 activations in 30d (threshold 50); median firing time 4m"
    - type: NoRunbook
      severity: info
      message: "annotation runbook_url is missing"
  conditions:
    - { type: Healthy, status: "False", reason: Noisy }
```

`kubectl get hsar -A` prints NAMESPACE, ALERT, TEAM, LOADED, FIRED (window), LAST FIRED,
FINDINGS.

### 4.3 Findings in v0.1

| Finding | How it is decided | Severity |
|---|---|---|
| `NotLoaded` | Rule is in a `PrometheusRule` in scope but absent from Prometheus `/api/v1/rules`. With `resourceRef`, the reason is named (selector or namespace mismatch). | critical |
| `LoadError` | Prometheus reports the rule group unhealthy or the rule's last evaluation errored. | critical |
| `DeadQuery` | Every vector selector in `expr` is checked with a cheap `count(...)` query; a selector that matches no series makes the rule unable to fire (unless it sits under `absent()`/`absent_over_time()`, where empty is the point). | warning |
| `NeverFired` | No activation in the window, and the rule has existed for at least `neverFired.after`. Informational: a quiet rule can be a good rule; together with `DeadQuery` it is not. | info |
| `Noisy` | Activations in the window above `noisy.firingsAbove`. | warning |
| `Flapping` | Activations per day above the threshold **and** median firing duration below the threshold. | warning |
| `AlwaysSilenced` | Fraction of firing time covered by a matching Alertmanager silence above the threshold (computed from silences still held by Alertmanager). | warning |
| `Unowned` | No team from any `teamFrom` source. | warning |
| `NoRunbook`, `NoSeverity` | Missing `runbook_url`; severity label missing or not in `requireSeverity`. | info |

How activations are measured, without any database of our own: Prometheus keeps
`ALERTS_FOR_STATE{alertname=...}` whose value is the activation timestamp, so
`changes(ALERTS_FOR_STATE{...}[window])` counts activations, and
`count_over_time(ALERTS{alertstate="firing",...}[window:1m])` gives firing minutes. Queries are
batched per rule group and spread over the resync interval under the `queryBudget`, so a
cluster with a thousand rules does not hammer Prometheus. The window is capped to the
retention Prometheus reports (`/api/v1/status/tsdb`), and the effective window is shown, so a
verdict is never based on history that does not exist.

## 5. Controller behaviour

1. **Scope reconcile** (on `AlertScope` change and every `resyncInterval`): check Prometheus
   and Alertmanager are reachable, read retention, list `PrometheusRule` objects in scope
   (informer), fetch `/api/v1/rules`, fetch Alertmanager silences.
2. **Join**: build the set of rules by stable identity — `PrometheusRule` side (what should
   run) and Prometheus side (what does run). Rules on one side only become `NotLoaded` or
   file-loaded mirrors.
3. **Mirror**: create, update or delete `AlertRule` objects so they match the set. Only objects
   carrying `alerts.hypersurgery.dev/managed-by: alert-operator` and the scope label are ever
   updated or deleted; anything else with the same name is left alone and reported as a
   conflict on the scope.
4. **Analyse**: run the quality and dead-query queries under the budget; write `status` on each
   `AlertRule`; aggregate counts on the `AlertScope`.
5. **Publish**: metrics, and a Kubernetes Event when a rule gains or loses a finding (so
   `kubectl get events` and any event exporter show "PaymentsHighLatency became Noisy").
6. **Degrade safely**: if Prometheus is unreachable the scope goes `Ready=False`, existing
   verdicts are kept and marked stale (`lastEvaluated` stops moving); nothing is deleted
   because a source was temporarily unreadable.

Leader election, two replicas, structured JSON logs, same chart conventions as
subnet-operator.

## 6. Safety rules

- **Never modifies or deletes anything it did not create.** In v0.1 the operator has no write
  access at all to `PrometheusRule`, `AlertmanagerConfig`, `Prometheus`, Alertmanager
  configuration or silences — RBAC grants `get/list/watch` only, and the Alertmanager client
  has no write methods compiled in.
- **Owns exactly one kind of object**, `AlertRule`, identified by its managed-by and scope
  labels. Deleting an `AlertScope` removes its mirrors and nothing else.
- **A mirror never outlives or deletes its source** (ownerReference points from mirror to
  source, `controller: false`).
- **Never deletes on uncertainty**: a failed list or an unreachable Prometheus never leads to
  mirror deletion; deletion happens only after a successful, complete read shows the rule gone.
- **Bounded load on Prometheus**: timeouts, concurrency and rate limits are part of the API and
  enforced; the operator alerts on its own budget exhaustion.
- **No data leaves the cluster.** No telemetry, no SaaS.
- Later phases that write (silences in 0.3, routes in 0.4) are opt-in per scope, behind
  `--enable-writes`, and touch only objects/silences the operator created and can prove it
  created (Alertmanager silence `createdBy` plus a comment marker and the owning `AlertSilence`
  object). Hand-made silences and routes are never edited or expired.

## 7. Metrics

All with prefix `hs_alert_`, labels kept low-cardinality (no `expr`):

| Metric | Type | Labels |
|---|---|---|
| `hs_alert_rules` | gauge | `scope`, `team`, `namespace`, `loaded` |
| `hs_alert_rule_findings` | gauge (1 per active finding) | `scope`, `namespace`, `team`, `alertname`, `finding`, `severity` |
| `hs_alert_rule_activations` | gauge (in the window) | `scope`, `namespace`, `team`, `alertname` |
| `hs_alert_rule_firing_fraction` | gauge | same |
| `hs_alert_scope_effective_window_seconds` | gauge | `scope` |
| `hs_alert_scope_sync_duration_seconds` | histogram | `scope` |
| `hs_alert_scope_last_sync_timestamp_seconds` | gauge | `scope` |
| `hs_alert_query_total` | counter | `scope`, `target` (prometheus/alertmanager), `result` |
| `hs_alert_query_budget_exhausted_total` | counter | `scope` |

The chart ships a Grafana dashboard ("Alert quality") and a small `PrometheusRule` for the
operator's own health, which of course the operator also grades.

## 8. Roadmap

| Phase | Goal | Writes? |
|---|---|---|
| **0.1 Inventory and report card** | Everything in this document: `AlertScope`, `AlertRule`, findings, metrics, dashboard, chart, e2e in Kind with kube-prometheus-stack. | No |
| **0.2 Ownership and validation at the door** | Opt-in validating webhook for `PrometheusRule` (warn mode first, then enforce per namespace): owner, severity, runbook, PromQL that parses, selectors that exist, rule will be picked up by the Prometheus selectors. `hsalert lint` CLI with the same checks for CI. | No (admission only) |
| **0.3 Silences as code** | `AlertSilence` CRD: matchers, mandatory `expiresAt`, maximum duration per scope, owner and reason required, cannot match other teams' alerts. Operator creates and expires only its own silences; report of expired-and-renewed-forever silences. | Opt-in |
| **0.4 Safe team routing** | `TeamRoute`: a team declares its receivers; the operator renders an owned `AlertmanagerConfig` with guard-rails (matchers pinned to the team, no catch-alls, test notification on change). New finding `Unrouted` (alert ends at the default or a null receiver). | Opt-in |
| **0.5 Many clusters** | Hub mode: scopes from several clusters in one view; the same rule deployed to N clusters is one logical rule with N verdicts; duplicates and drift between clusters reported. | No |
| **1.0 Stable** | `v1` API with compatibility rules and tests, upgrade tests, signed multi-arch image and chart, OLM bundle, docs, real-cluster conformance run. | — |

## 9. Open questions

The build of 0.1 started on 2026-11-18 without waiting for answers: each question has a default
that is cheap to change before 0.1.0 is released. The owner can overrule any of them.

1. API group `alerts.hypersurgery.dev` (matching the sibling's `network.hypersurgery.dev`) or
   `monitoring.hypersurgery.dev`, which would leave room for non-alert features later.

   **Decision (default, 2026-11-18):** `alerts.hypersurgery.dev`. It says what the objects are,
   matches the sibling's one-word-per-domain pattern, and a later non-alert feature can have a
   group of its own. Renaming is free until 0.1.0 is published and expensive after.

2. Product name on the site: "Alert Operator" (descriptive) or something with more character.

   **Decision (default, 2026-11-18):** "Alert Operator", repository and chart `alert-operator`,
   next to "Subnet Operator". The family name is the character; the product name says what it
   does.

3. Does v0.1 need Thanos/Mimir as the history source (long windows), or is local Prometheus
   retention enough to start?

   **Decision (default, 2026-11-18):** local retention is enough for 0.1. The window is capped to
   the retention Prometheus reports and the effective window is shown, so a short retention
   gives honest, shorter verdicts rather than wrong ones. Because only the standard HTTP API is
   used, `prometheus.url` may already point at a Thanos Query or Mimir endpoint that serves
   `/api/v1/rules`; a separate long-term history URL is left for a later version.

4. Should mirrors live in the source namespace (team-visible, needs create rights in every
   namespace in scope) or only in the operator namespace (narrower RBAC, cluster-wide read)?

   **Decision (default, 2026-11-18):** in the source namespace, as designed in §4.2. Teams read
   their own rules with namespace RBAC, which is the point of the per-team view. The extra
   right is narrow: create/update/delete of `alertrules.alerts.hypersurgery.dev` only, a kind
   the operator owns, protected from other writers by the ValidatingAdmissionPolicy. Rules
   loaded from plain files go to the operator namespace.

## 10. Version 0.2: validation at the door

0.1 reports a bad rule after it reached the cluster. 0.2 says so when it is applied, or before
that, in CI. Two front ends, one set of checks:

- an **opt-in validating admission webhook** for `PrometheusRule` objects, in warn mode first
  and in enforce mode for the namespaces that ask for it;
- a **`hsalert lint` CLI** that runs the same checks on rule files in a pipeline.

Still no writes: the webhook answers admission reviews and changes nothing.

### 10.1 Checks

One package, `internal/lint`, takes a `PrometheusRule` (the CLI also accepts plain Prometheus
rule files, wrapped into one) and returns problems. The webhook and the CLI only gather the
inputs and present the result, so a rule judged in CI is judged the same at the door, and the
same vocabulary as the 0.1 findings is used:

| Check | Input it needs | Blocks in enforce mode |
|---|---|---|
| `LoadError`: an expression does not parse (alerting and recording rules) | none | yes |
| `NotLoaded`: the Prometheus `ruleSelector` / `ruleNamespaceSelector` would not pick the object up | the Prometheus object, namespace labels | yes |
| `Unowned`: no team from the `teamFrom` sources | object and namespace labels | yes |
| `NoSeverity`: severity missing or not allowed | ownership settings | yes |
| `NoRunbook`: `runbook_url` missing, with `requireRunbook` | ownership settings | yes |
| `DeadQuery`: a selector matches no series in the last hour | a reachable Prometheus | **never** (warning only) |

The NotLoaded reasoning is the inventory's code, moved where both can call it.

**Decision (default, 2026-11-18): DeadQuery never blocks.** Rules are routinely shipped
before the application that exports the metric (GitOps applies both together, the rule first).
A selector with no series yet is worth a warning, not a refusal.

**Decision (default, 2026-11-18): every other check blocks in enforce mode.** Which ones apply
is already configurable through the scope's `ownership` (`requireRunbook: false`,
`requireSeverity`), so no second list of switches is needed.

### 10.2 Modes, per namespace

The namespace label `alerts.hypersurgery.dev/validation` picks the mode:

| Value | Effect |
|---|---|
| `enforce` | denied when a blocking problem exists; the other problems as admission warnings |
| `warn` | always admitted; every problem as an admission warning (kubectl prints them) |
| `off` | not sent to the webhook at all (the webhook's namespaceSelector excludes it) |
| absent | the manager's `--webhook-default-mode`, chart value `webhook.defaultMode` |

**Decision (default, 2026-11-18): `webhook.defaultMode: warn`.** Turning the webhook on must
not break anyone's deploys; a team opts into `enforce` with one label on its namespace.

### 10.3 Which scope's settings

The webhook uses the first `AlertScope`, by name, whose `namespaceSelector` selects the
object's namespace: its ownership settings, its Prometheus object (`resourceRef`) for NotLoaded
and its Prometheus URL and auth for DeadQuery. Without such a scope the default ownership
settings apply and the Prometheus checks are skipped (said in a warning).

### 10.4 Failure policy and budget

**Decision (default, 2026-11-18): `failurePolicy: Ignore`, `timeoutSeconds: 5`.** An operator
that is down, being upgraded or slow must never stop rules from being applied; validation is a
guard rail, not a gate the cluster depends on. With `Fail`, an outage of the operator would
block every PrometheusRule change, including the one that fixes the outage's alerts.

Within a review the checks that need no I/O take microseconds. DeadQuery is the only one that
calls out: it gets at most 2 seconds per review in total, answers are cached per selector for
5 minutes (a rollout applying the same rules to ten namespaces asks once), and whatever does not
answer in time is skipped with a warning. Reads of namespaces, scopes and Prometheus objects
come from the manager's cache. The webhook is served by every replica, not only the leader.

### 10.5 Certificates

As in subnet-operator's chart: `webhook.certificate.certManager: auto` uses cert-manager when
its API is installed (a self-signed Issuer of the chart's own, or `issuerRef`), and otherwise
the chart signs a CA and a serving certificate itself, reused on upgrades until it is within
`renewBefore` of expiry; `existingSecret` takes one you manage. The CA goes into the
ValidatingWebhookConfiguration's `caBundle` (or is injected by cert-manager's CA injector).

### 10.6 The CLI

`hsalert lint [flags] FILE...` reads PrometheusRule manifests (multi-document, other kinds
skipped) and plain rule files. Ownership, the Prometheus object and its namespace labels, and
optionally a Prometheus URL are flags. Output as text, JSON or GitHub annotations. Exit codes:
0 clean (warnings allowed), 1 a blocking problem (or any problem with `--strict`), 2 usage or
read errors. It is built for linux and darwin, amd64 and arm64, and attached to every GitHub
release with signed checksums.

### 10.7 Metrics

`hs_alert_webhook_reviews_total{mode,result}` (result: allowed, warned, denied) and
`hs_alert_webhook_review_duration_seconds`.
