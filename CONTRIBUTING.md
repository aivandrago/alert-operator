# Contributing

Thank you for looking. Issues and pull requests are welcome.

## Before you start

- Read [docs/design.md](docs/design.md): the scope of each version, the API and the safety rules.
  A change that makes v0.x write to anything other than `AlertRule` objects is out of scope.
- Everything in the repository is in English: code, comments, documentation, commit messages.

## Development

```sh
make build        # generate, then build bin/manager
make test         # unit tests and envtest (downloads kube-apiserver and etcd once)
make lint         # golangci-lint
make test-e2e     # Kind; needs Docker
```

After changing `api/` types or `+kubebuilder` markers run `make manifests generate` and commit
the result: CI fails when generated files are out of date.

## Pull requests

- One coherent change per pull request, with tests.
- CI must be green before a merge. If a check is red, fix it, even when it looks unrelated.
- Commit messages: a short summary line, then what changed and why.

## Reporting security issues

Not in a public issue: see [SECURITY.md](SECURITY.md).
