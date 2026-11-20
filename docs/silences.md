# Silences as code

From 0.3 a team declares an Alertmanager silence as a Kubernetes object, `AlertSilence`, with an
owner, a reason and a mandatory end. The operator creates the silence, keeps it in step with the
object and expires it when the object is deleted. It is the first thing the operator writes
outside its own objects, so it is off until you turn it on twice, and it only ever changes
silences it created.

## Turning it on

1. Let the manager write: chart value `writes.enabled: true` (`--enable-writes`).
2. Pick a mode for the scope:

```yaml
apiVersion: alerts.hypersurgery.dev/v1alpha1
kind: AlertScope
metadata:
  name: main
spec:
  alertmanager:
    url: http://alertmanager-operated.monitoring.svc:9093
  silences:
    mode: Propose          # Off (default), Propose, Apply
    maxDuration: 7d        # the longest silence an AlertSilence may ask for (at most 90d)
    restrictToNamespaceRules: true
```

Start with `Propose`: every AlertSilence shows the silence it would create and the alerts it
would match, and nothing is written. Switch to `Apply` when the proposals look right.

## Declaring a silence

```yaml
apiVersion: alerts.hypersurgery.dev/v1alpha1
kind: AlertSilence
metadata:
  name: payments-db-migration
  namespace: team-payments
spec:
  scopeRef: main
  matchers:
    - { name: alertname, value: PaymentsHighLatency }
    - { name: env, value: staging }
  duration: 4h            # or expiresAt: "2026-11-21T18:00:00Z"; startsAt is optional
  owner: payments
  reason: "Database migration CHG-1234, latency expected to rise for the window"
```

```console
$ kubectl get alertsilences -n team-payments
NAME                    SCOPE   STATE    ENDS   MATCHED   OWNER      AGE
payments-db-migration   main    Active   3h     2         payments   58m
```

| State | Meaning |
|---|---|
| `Disabled` | The scope's `silences.mode` is Off. |
| `Proposed` | Validated; `status.proposed` shows the silence. Nothing written: Propose mode, `dryRun: true`, or the manager runs without `--enable-writes`. |
| `Rejected` | Against the policy; `status.message` says why. Nothing written. |
| `Pending` | Created, starts at `startsAt`. |
| `Active` | The silence is in Alertmanager (`status.silenceID`). |
| `Expired` | It reached its end. The object stays as the record; delete it when you like. |
| `ExpiredExternally` | Someone ended the silence in Alertmanager. It is not recreated; change the spec to ask for a new one. |

Changing the spec updates the silence (Alertmanager may give it a new ID). Deleting the object
expires the silence.

## The policy

- **The end is mandatory** (`expiresAt` or `duration`, exactly one) and at most the scope's
  `maxDuration` after the start.
- **Owner and reason are required**; they go into the silence's comment, so whoever finds the
  silence in the Alertmanager UI knows whose it is and why.
- **Only your own alerts**, with `restrictToNamespaceRules` (default): the matchers need an
  `alertname` matcher (a name, or a regex listing names: `A|B|C`), and every name must be an
  alerting rule of the AlertSilence's own namespace and of no other namespace.
- **Never everything**: at least one positive matcher must select something; `.*` alone is
  refused.

## What the operator never does

- It never changes or expires a silence it did not create. Every silence it writes has
  `createdBy: alert-operator` and a comment starting with
  `[alert-operator <namespace>/<name> uid=<uid>]`, and it reads a silence back and checks both
  before every update or expiry.
- It never lists silences to act on them, and never recreates a silence a person ended.
- It never writes without `--enable-writes` and a scope in Apply mode.
- It never creates or deletes AlertSilence objects; only teams do (the chart gives namespace
  editors that right through the aggregated `edit` role).

If Alertmanager is unreachable when an AlertSilence is deleted, the operator retries for ten
minutes, then lets the object go with a Warning Event: the silence still ends at its expiry.

## Audit

Every write is a Kubernetes Event on the AlertSilence (`SilenceCreated`, `SilenceUpdated`,
`SilenceReleased`, plus `SilenceRejected` and `SilenceExpiredExternally`) and one JSON line on the
operator's `audit` logger:

```json
{"level":"info","logger":"audit","msg":"Alertmanager silence write","action":"create","alertsilence":"team-payments/payments-db-migration","uid":"…","scope":"main","silenceID":"6d4f…","matchers":[…],"endsAt":"2026-11-20T14:00:00Z","owner":"payments","result":"ok"}
```

Metrics: `hs_alert_silence_writes_total{scope,action,result}` and `hs_alert_silences{scope,state}`.
