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
#    (create one in the admin console: pre-approved recommended; NOT
#    ephemeral — the IdP node identity should be durable)
kubectl create namespace tsidp
kubectl -n tsidp create secret generic tsidp-authkey --from-literal=authkey=tskey-auth-...

# 2. Install the chart from GHCR (CRDs included)
helm install tsidp oci://ghcr.io/isvaldi-consulting/charts/tsidp \
  --version 0.1.0 -n tsidp --set tsidp.authKey.existingSecret=tsidp-authkey

# 3. Register a client
kubectl apply -f config/samples/tsidp_v1alpha1_oidcclient.yaml
kubectl get oidcclient sample     # CLIENT ID, SECRET, READY
kubectl get secret sample-oidc    # client_id, client_secret, issuer, endpoints
```

(From a checkout, `helm install tsidp charts/tsidp ...` works identically.)

## The Helm chart

`oci://ghcr.io/isvaldi-consulting/charts/tsidp` deploys the whole stack in
one release: a **single-replica StatefulSet** running the official
`ghcr.io/tailscale/tsidp` image (version pinned to the last e2e-tested tag,
see `versions.yaml`) with the operator as a sidecar, a PVC for tsidp's
state, the `OIDCClient` CRD, and the operator's RBAC. The replica count is
deliberately not configurable — tsidp is single-instance by design.

Values you are most likely to set:

| Value | Default | Why |
|---|---|---|
| `tsidp.authKey.existingSecret` | `""` | Name of the Secret holding the Tailscale auth key (key `authkey`). Without it tsidp waits at an interactive login prompt. |
| `tsidp.hostname` | `idp` | The tailnet machine name → your issuer is `https://<hostname>.<tailnet>.ts.net`. |
| `tsidp.funnel` | `false` | Expose token/JWKS endpoints publicly via Funnel so relying parties outside the tailnet can redeem codes. |
| `tsidp.persistence.size` | `1Gi` | The PVC holds the tsnet node identity, signing key, and client registry — treat it (and backups of it) as credential material. |
| `operator.resyncInterval` | `10m` | How often registrations are re-verified against tsidp. |
| `operator.watchNamespaces` | `[]` | Empty: watch **all** namespaces (needs cluster-wide RBAC). Set a list to enable the hardened, least-privilege install — see below. |
| `networkPolicy.enabled` | `false` | Optional ingress restriction for the pod. |

### Hardened install (least privilege)

By default the operator watches **all namespaces**, which requires a
ClusterRole with read/write on every Secret in the cluster (Kubernetes
RBAC cannot be label-scoped). To harden, list the namespaces where your
`OIDCClient`s will live:

```sh
helm install tsidp oci://ghcr.io/isvaldi-consulting/charts/tsidp \
  --version 0.1.0 -n tsidp \
  --set tsidp.authKey.existingSecret=tsidp-authkey \
  --set 'operator.watchNamespaces={monitoring,myapp}'
```

or in a values file:

```yaml
operator:
  watchNamespaces:
    - monitoring
    - myapp
```

This swaps the ClusterRole for one namespaced Role/RoleBinding per listed
namespace and starts the operator with `--watch-namespaces` — cluster-wide
Secret access is gone entirely. The trade: an `OIDCClient` created in a
namespace **not** on the list is silently ignored until you add the
namespace and `helm upgrade`. Existing installs are unaffected by the
default; hardening is a pure values change.

Full reference: [`charts/tsidp/README.md`](charts/tsidp/README.md). If your
relying parties run **inside the same cluster**, remember they must be able
to reach the issuer URL — either enable `tsidp.funnel`, or (tailnet-only)
run the Tailscale Kubernetes operator and point an egress Service at the
issuer plus a CoreDNS rewrite of the issuer hostname onto that Service.

Secret keys written for every OIDCClient: `client_id`, `client_secret`,
`issuer`, `authorization_endpoint`, `token_endpoint`, `userinfo_endpoint`,
`jwks_uri`, `redirect_uris`.

## Writing an OIDCClient

Create one `OIDCClient` per application, in the **same namespace as the
workload that will consume the credentials** (the Secret is created next to
the CR). Minimal form — two fields:

```yaml
apiVersion: tsidp.isvaldi-consulting.github.io/v1alpha1
kind: OIDCClient
metadata:
  name: myapp
  namespace: myapp
spec:
  redirectUris:
    - https://myapp.example.com/oauth2/callback
```

Everything else has defaults. The full surface:

```yaml
apiVersion: tsidp.isvaldi-consulting.github.io/v1alpha1
kind: OIDCClient
metadata:
  name: grafana
  namespace: monitoring
spec:
  clientName: "Grafana"           # display name in tsidp (default: CR name)
  redirectUris:                   # required; matched EXACTLY (case, slashes)
    - https://grafana.example.com/login/generic_oauth
  scope: "openid profile email"
  tokenEndpointAuthMethod: client_secret_basic   # or client_secret_post|none
  secretName: grafana-oidc        # default: <name>-oidc
  secretTemplate:                 # optional labels/annotations on the Secret
    annotations:
      reloader.stakater.com/match: "true"
  updatePolicy: Recreate          # Recreate | InPlace | Block
  deletionPolicy: Deregister      # Deregister | Orphan
```

Watch it converge, then wire the app to the Secret — credentials **and**
endpoint URLs all come from the operator, so nothing OIDC is hand-typed:

```sh
kubectl -n monitoring get oidcclient grafana   # READY=True + client ID
```

```yaml
# example: Grafana generic_oauth, entirely from the minted Secret
env:
  - name: GF_AUTH_GENERIC_OAUTH_CLIENT_ID
    valueFrom: {secretKeyRef: {name: grafana-oidc, key: client_id}}
  - name: GF_AUTH_GENERIC_OAUTH_CLIENT_SECRET
    valueFrom: {secretKeyRef: {name: grafana-oidc, key: client_secret}}
  - name: GF_AUTH_GENERIC_OAUTH_AUTH_URL
    valueFrom: {secretKeyRef: {name: grafana-oidc, key: authorization_endpoint}}
  - name: GF_AUTH_GENERIC_OAUTH_TOKEN_URL
    valueFrom: {secretKeyRef: {name: grafana-oidc, key: token_endpoint}}
  - name: GF_AUTH_GENERIC_OAUTH_API_URL
    valueFrom: {secretKeyRef: {name: grafana-oidc, key: userinfo_endpoint}}
```

Behavior notes:

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
