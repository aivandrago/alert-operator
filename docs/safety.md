# Safety

For a security reviewer: what alert-operator 0.1 can and cannot do, and how that is enforced.
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
