# Contributing

## Development

```sh
make build           # codegen + compile
make test            # unit + contract tests (fake tsidp, no cluster needed)
make lint            # go vet + gofmt check
make manifests sync-chart-crds   # after changing api/v1alpha1 types
make helm-lint       # chart lint + render (needs helm)
TS_AUTHKEY=tskey-... make e2e    # kind e2e against the pinned real tsidp
```

CI fails on codegen drift: if you touch `api/v1alpha1`, commit the
regenerated files from `make manifests generate sync-chart-crds`.

## The tsidp wire contract

The operator talks to tsidp's HTTP API; `internal/tsidp/tsidpfake` is a
contract-faithful double (snapshot: tsidp v0.0.15). If you change the
client, keep the fake honest — including the awkward parts, like `/edit`
reporting validation failures at HTTP 200. Registration must always use
`POST /register` (RFC 7591), never `/clients/new`.

## tsidp version bumps (the compat loop)

The pinned tsidp version lives in `hack/tsidp.Dockerfile` — a single FROM
line watched by Dependabot. PRs touching that file hard-fail CI unless the
`TS_AUTHKEY` secret is available to run the kind e2e; see
`docs/COMPATIBILITY.md` for the full loop, including recording tested
pairs in `versions.yaml`. Forks without the secret still run the contract
tests.

## Releases

Tag `vX.Y.Z` after bumping `charts/tsidp/Chart.yaml` (`version: X.Y.Z`,
`appVersion: "vX.Y.Z"`) — the release workflow verifies they match the tag
and publishes the multi-arch image and the chart to GHCR.
