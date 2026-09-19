# tsidp-operator

A Kubernetes companion operator for [tsidp](https://github.com/tailscale/tsidp),
Tailscale's OIDC identity provider. Applications self-register OIDC clients by
creating an `OIDCClient` custom resource; the operator registers the client
with tsidp over its HTTP API and materializes the credentials into a
Kubernetes Secret the workload can mount or env-reference.

**tsidp is never forked or patched** — the operator is a pure API client of
the stock tsidp binary/image. Non-Kubernetes tsidp deployments are unaffected
by anything in this repo.

## How it works

```
┌─ Pod (StatefulSet, replicas: 1) ──────────────────────┐
│  ┌────────────┐  localhost:8080   ┌────────────────┐  │      tailnet
│  │   tsidp    │◄──────────────────│ tsidp-operator │  │   ┌───────────┐
│  │ -local-port│   POST /register  │    (sidecar)   │  │   │ your apps │
│  │   -dir     │                   └───────┬────────┘  │   └─────▲─────┘
│  └─────▲──────┘                           │           │         │ OIDC
└────────┼──────────────────────────────────┼───────────┘         │
     PVC (state)                   watches OIDCClient CRs,        │
                                   writes credential Secrets ─────┘
```

- tsidp's `-local-port` loopback listener grants admin + dynamic client
  registration to pod-local callers, so the sidecar needs no tailnet
  identity and no ACL grants. Sidecar-in-the-same-pod is the only deployment
  mode: the operator manages the tsidp instance it ships with, and the
  binary carries no Tailscale dependency at all. (If you ever need to drive
  an out-of-cluster tsidp, the API surface it would use — a tagged node with
  a `tailscale.com/cap/tsidp` grant — exists in tsidp today, but this
  operator deliberately doesn't implement it.)
- Registration uses tsidp's RFC 7591 `POST /register`. The plaintext
  `client_secret` exists only in that one response; the operator writes it to
  the Secret *before* recording status, and recovers from crashes in between
  by adopting (or burning) registrations via a name fingerprint
  `[k8s:<namespace>/<name>/<uid8>]`.

## Quick start

```sh
# 1. A Tailscale auth key for tsidp's tailnet node
kubectl create namespace tsidp
kubectl -n tsidp create secret generic tsidp-authkey --from-literal=authkey=tskey-auth-...

# 2. Install (CRDs included)
helm install tsidp charts/tsidp -n tsidp --set tsidp.authKey.existingSecret=tsidp-authkey

# 3. Register a client
kubectl apply -f config/samples/tsidp_v1alpha1_oidcclient.yaml
kubectl get oidcclient sample     # CLIENT ID, SECRET, READY
kubectl get secret sample-oidc    # client_id, client_secret, issuer, endpoints
```

Secret keys written: `client_id`, `client_secret`, `issuer`,
`authorization_endpoint`, `token_endpoint`, `userinfo_endpoint`, `jwks_uri`,
`redirect_uris`.

## OIDCClient

```yaml
apiVersion: tsidp.isvaldi-consulting.github.io/v1alpha1
kind: OIDCClient
metadata:
  name: grafana
spec:
  clientName: "Grafana"
  redirectUris:
    - https://grafana.example.ts.net/login/generic_oauth
  scope: "openid profile email"
  secretName: grafana-oidc        # default: <name>-oidc
  updatePolicy: Recreate          # Recreate | InPlace | Block
  deletionPolicy: Deregister      # Deregister | Orphan
```

- `updatePolicy: Recreate` (default) — drift deletes and re-registers the
  client with **new credentials**; pair Secrets with a reloader so workloads
  pick up changes.
- `updatePolicy: InPlace` — updates name/redirect URIs via tsidp's admin form
  endpoint, preserving credentials. This rides tsidp's HTML UI contract (no
  stable JSON update API exists yet); it may break across tsidp versions —
  the compat e2e (below) exercises exactly this path against the pinned
  real tsidp to catch that.
- Deleting the CR deregisters the client (tsidp also revokes its outstanding
  tokens) and cascade-deletes the Secret. Deregistration is guarded by a
  finalizer; add the `tsidp.isvaldi-consulting.github.io/force-remove-finalizer` annotation (any value)
  if tsidp is permanently gone.
- Only name and redirect-URI drift is detectable (tsidp's read API returns
  nothing else); other spec changes converge only via `Recreate`.

## Version pinning & the compat loop

The supported tsidp version is pinned in `hack/tsidp.Dockerfile` (single
`FROM` line). Dependabot watches that file and PRs new tsidp releases; the
`tsidp-compat` workflow runs a kind-based e2e against the bumped version
(requires a `TS_AUTHKEY` repo secret — add it as both an Actions **and** a
Dependabot secret); passing pairs are recorded in `versions.yaml` and the
chart default follows the newest tested pair. Details: `docs/COMPATIBILITY.md`.

## Development

```sh
make build           # generate + compile
make test            # unit + contract tests (fake tsidp)
make manifests       # regenerate the CRD after API changes
make sync-chart-crds # copy CRDs into the chart
make docker-build    # distroless image (IMG=... to override the tag)
TS_AUTHKEY=tskey-... make e2e   # kind e2e against the pinned tsidp
```

Module layout: `api/v1alpha1` (CRD types), `internal/tsidp` (tsidp API
client; `tsidpfake` is the contract-faithful test double),
`internal/controller` (reconciler), `charts/tsidp` (chart deploying tsidp
+ sidecar). The operator binary has no Tailscale dependency — it is a plain
HTTP client of the pod-local tsidp listener.

## Security notes

- The tsidp loopback listener is full-admin plain HTTP for anything inside
  the pod network namespace — restrict `pods/portforward` on the tsidp pod
  via RBAC, and treat pod exec access as equivalent to IdP admin.
- tsidp stores client secrets and its signing key in cleartext under its
  state dir: PVC **backups are credential material**.
- tsidp is single-replica by design (in-memory tokens, whole-file state,
  single tsnet identity); the chart pins `replicas: 1`. Restarts invalidate
  live tokens; PVC loss additionally changes the node identity (issuer) —
  keep the PVC durable and backed up.
- Anyone holding an `allow_dcr` grant in your tailnet ACLs can register
  clients directly, bypassing CRD validation — the operator itself needs no
  grant (loopback), so grant `allow_dcr` to no one unless you have another
  DCR consumer.
