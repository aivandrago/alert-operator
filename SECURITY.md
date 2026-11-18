# Security

## Reporting a vulnerability

Please report privately rather than in a public issue: **security@hypersurgery.dev**.

Include what the operator did, what an attacker could reach, and how to reproduce it. A redacted
`AlertScope` and the RBAC in use help more than a stack trace. We aim to acknowledge within three
working days and to agree a disclosure date with you once the fix is understood; credit is yours
unless you ask otherwise.

## What the operator can do, by design

- **It only reads Prometheus and Alertmanager.** The Prometheus client calls `GET` endpoints of
  the HTTP API; the Alertmanager client has no write methods compiled in, and a test enforces it.
- **It writes one kind of object**, `AlertRule`, plus the status of `AlertScope` and Events. It
  has no write access to `PrometheusRule`, `Prometheus`, `AlertmanagerConfig` or anything else.
- **It never deletes on uncertainty**: a failed read never leads to an `AlertRule` being deleted.
- **No data leaves the cluster**: no telemetry, no SaaS.

## Things that are not vulnerabilities

- `AlertRule` objects copy the expression, labels and annotations of alerting rules. Anyone who
  can read `AlertRule` objects in a namespace can read the rules of that namespace; scope RBAC
  accordingly.
- Metrics carry namespace, team and alert names by design.

## Supported versions

Releases before 1.0 get no fixes: upgrade.
