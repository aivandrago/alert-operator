# Safety

For a security reviewer: what alert-operator can and cannot do, and how that is enforced.
Everything below is checked in the repository by the tests named.

## It only reads Prometheus and Alertmanager

- The clients (`internal/promapi`) send `GET` requests only. There is no method that writes;
  `TestAlertmanagerClientHasOnlyReadMethods` fails if one is added to either client, and the
  HTTP transport refuses any other method (`TestOnlyGETLeavesTheProcess`).
- Endpoints read: Prometheus `/api/v1/rules`, `/api/v1/query`, `/api/v1/query_range`,
  `/api/v1/status/runtimeinfo`, `/api/v1/status/tsdb`; Alertmanager `/api/v2/silences`,
  `/api/v2/status`.
- The load is bounded per scope (`spec.queryBudget`): a timeout per query, a concurrency limit,
  a rate limit, and a deadline per sync.
- Redirects are not followed, so credentials are never sent to another host.

## It writes one kind of object

The operator's ClusterRole (`charts/alert-operator/templates/rbac.yaml`, equal to the generated
`config/rbac/role.yaml`, checked by `TestRBACMatchesGeneratedRole`):

| API group | Resources | Verbs |
|---|---|---|
| `""` | `namespaces` | get, list, watch |
| `monitoring.coreos.com` | `prometheusrules`, `prometheuses` | get, list, watch |
| `alerts.hypersurgery.dev` | `alertscopes` | get, list, watch |
| `alerts.hypersurgery.dev` | `alertscopes/status`, `alertrules/status` | get, update, patch |
| `alerts.hypersurgery.dev` | `alertrules` | get, list, watch, create, update, patch, delete |
| `events.k8s.io` | `events` | create, patch |
| `authentication.k8s.io` / `authorization.k8s.io` | `tokenreviews` / `subjectaccessreviews` | create (authenticated metrics endpoint) |

In its own namespace only (a Role): `secrets` get (the auth Secrets named by `authSecretRef`),
`leases` (leader election), `events`. Secrets are read without a cache, so the operator never
lists or watches Secrets. `TestRBACIsLeastPrivilege` fails on a wildcard, on a write verb outside
the operator's own kinds, on any verb but get/list/watch on `monitoring.coreos.com`, and on
cluster-wide Secret access.

## It touches only what it created

- An `AlertRule` is updated or deleted only when it carries both
  `alerts.hypersurgery.dev/managed-by: alert-operator` and the scope's
  `alerts.hypersurgery.dev/scope` label. An object without them that has the wanted name is left
  alone and reported as a conflict (`TestScopeLifecycle`).
- Deletion happens only after a complete, successful read: with Prometheus unreachable or any
  list failing, nothing is deleted and the verdicts go stale (`TestScopeLifecycle`).
- Each `AlertRule` has an ownerReference to its `PrometheusRule` with `controller: false` and
  `blockOwnerDeletion: false`: it is garbage-collected with its source and never holds it back.
- Deleting an `AlertScope` deletes the `AlertRule` objects carrying its scope label, nothing else.
- The Kind e2e and the integration test compare every `PrometheusRule`'s resourceVersion and
  Alertmanager's silence list before and after.

## Nobody else writes AlertRule objects

A ValidatingAdmissionPolicy (CEL, no webhook) refuses create, update and delete of `AlertRule`
objects and their status by anyone but the operator's service account, the garbage collector
and the namespace controller (`TestOnlyTheOperatorWritesAlertRules` against a real API server).
A verdict cannot be edited away; fix the rule instead.

## The silence writer (0.3, opt-in)

The one write path outside the operator's own objects ([silences.md](silences.md)):

- Only with `--enable-writes` (chart `writes.enabled`, false by default) and only for scopes
  whose `silences.mode` is `Apply` (Off by default). Without either, AlertSilence objects are only
  proposed.
- A separate client, `promapi.SilenceWriter`, built only then. It has exactly two methods,
  `CreateOrUpdateSilence` and `ExpireSilence` (`TestSilenceWriterMethods`); its transport allows
  `POST /api/v2/silences` and `DELETE /api/v2/silence/{id}` besides reads and refuses anything
  else (`TestSilenceWriterTransport`).
- Every silence it writes carries `createdBy: alert-operator` and a comment starting with
  `[alert-operator <namespace>/<name> uid=<uid>]`. Before an update or an expiry it reads the
  silence and refuses unless both match the object (`ErrNotOurs`,
  `TestSilenceWriterLeavesOtherSilencesAlone`). The e2e checks a silence made by hand is
  unchanged after creating, expiring and deleting AlertSilences around it.
- Bounded: every silence has an end within the scope's `maxDuration`, and only alerts of the
  namespace's own rules can be silenced.
- RBAC: `alertsilences` get, list, watch, update, patch (the finalizer), their status and
  finalizers; never create or delete. The admission policy refuses writes to
  `alertsilences/status` by anyone else.
- Audit: an Event and a JSON line on the `audit` logger per write.

## The webhook (0.2, opt-in)

The validating webhook for `PrometheusRule` objects ([validation.md](validation.md)) answers
admission reviews and writes nothing: it needs no permission beyond the reads above. It is off
unless `webhook.enabled` is set, registered with `failurePolicy: Ignore` so an outage of the
operator never blocks a change, and a namespace labelled `alerts.hypersurgery.dev/validation=off`
is never sent to it. Its serving certificate is cert-manager's, the chart's, or yours; the
chart-signed key is stored in a Secret of the release namespace and in Helm's release Secrets.

## What it exposes

- `AlertRule` objects copy the expression, labels and annotations of each rule into the rule's
  namespace. Whoever may read `AlertRule` there may read the rules. With
  `rbac.aggregateToView: true` (default) that is everyone with `view` in the namespace.
- Metrics carry scope, namespace, team and alert names, never expressions or messages. The
  endpoint is HTTPS with TokenReview/SubjectAccessReview by default.
- No data leaves the cluster: no telemetry, no external service.

## The process

Distroless, non-root (65532), read-only root filesystem, no capabilities, `RuntimeDefault`
seccomp, no privilege escalation. Optional NetworkPolicy: egress to DNS, the API server and the
configured Prometheus/Alertmanager peers only.
