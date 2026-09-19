# tsidp Helm chart

Deploys [tsidp](https://github.com/tailscale/tsidp) — Tailscale's OIDC
identity provider — together with
[tsidp-operator](https://github.com/isvaldi-consulting/tsidp-operator), a sidecar
controller that reconciles namespaced `OIDCClient` custom resources
(`tsidp.isvaldi-consulting.github.io/v1alpha1`) against tsidp's pod-local admin API and
materializes each registered client's credentials into a Kubernetes Secret.

## How it works

The chart creates a **single-replica StatefulSet** with two containers:

- **tsidp** (`ghcr.io/tailscale/tsidp`, the official image published by the
  upstream repo on every release tag): joins your tailnet via tsnet as
  `<hostname>.<tailnet>.ts.net`, keeps its state under `/data` (a PVC from a
  `volumeClaimTemplate`), and additionally listens on `127.0.0.1:<localPort>`
  — a loopback listener that grants admin + dynamic client registration to
  pod-local callers.
- **operator** (`ghcr.io/isvaldi-consulting/tsidp-operator`): watches `OIDCClient`
  resources cluster-wide, registers/updates/deletes clients through the
  loopback API, and writes credentials (`client_id`, `client_secret`,
  `issuer`, `authorization_endpoint`, `token_endpoint`, `userinfo_endpoint`,
  `jwks_uri`, `redirect_uris`) into the Secret named by `spec.secretName`.

## Why the replica count is hardcoded to 1

tsidp is single-replica by design and this chart deliberately does **not**
expose a `replicas` value:

- its OIDC client registry lives in a single JSON state file;
- issued tokens/sessions are held in memory;
- it owns exactly one tsnet node identity (one hostname on the tailnet).

A second replica would fork state, invalidate tokens randomly depending on
which replica answers, and fight over the node identity. The StatefulSet uses
`updateStrategy: RollingUpdate`, which at `replicas: 1` behaves like
Recreate: the old pod is deleted before the new one starts, so two instances
never run concurrently.

## Version pinning policy

`tsidp.image.tag` defaults to the tsidp version this operator release is
**e2e-tested against** (the operator drives tsidp's unversioned admin API, so
pairings matter). The pin's source of truth is `hack/tsidp.Dockerfile` in the
operator repo; the default tag in `values.yaml` is synced from it by
`make sync-tsidp-version` (maintainer-initiated; see `docs/COMPATIBILITY.md`).
You may override the tag, but combinations other than the default are not
tested.

Upstream image tags keep the `v` prefix (e.g. `v0.0.15`); `latest` also
exists but is not recommended for production.

## Prerequisites

- A tailnet with MagicDNS and HTTPS enabled.
- A Tailscale auth key (or OAuth client secret) in a pre-created Secret:

  ```sh
  kubectl -n <ns> create secret generic tsidp-authkey \
    --from-literal=authkey='tskey-auth-XXXXXXXXXXXX'
  ```

  If you use an OAuth client secret (`tskey-client-...`), you must also set
  `tsidp.advertiseTags` (e.g. `tag:tsidp`). If no Secret is configured, tsidp
  prints an interactive login URL in its container logs instead.
- The `OIDCClient` CRD (installed automatically from `crds/` on first
  `helm install`; see `crds/README.md` — the YAML there is synced from
  `config/crd/bases/` by `make sync-chart-crds`).
- Access to tsidp's admin UI / DCR endpoints from the tailnet requires an
  [application capability grant](https://tailscale.com/kb/1537/grants-app-capabilities)
  (`tailscale.com/cap/tsidp`); the operator's pod-local access does not.

## Installing

```sh
helm install tsidp charts/tsidp \
  --namespace tsidp --create-namespace \
  --set tsidp.authKey.existingSecret=tsidp-authkey
```

## Security note

The loopback listener on `127.0.0.1:<tsidp.localPort>` trusts every pod-local
caller with full admin. It is unreachable from other pods, but
`kubectl port-forward` and `kubectl exec` into the tsidp pod bypass that
boundary — restrict `pods/portforward` and `pods/exec` RBAC in the release
namespace to cluster administrators. The optional NetworkPolicy
(`networkPolicy.enabled=true`) additionally denies all pod ingress; tsidp
still serves fine because tailnet traffic arrives over its outbound tsnet
tunnel.

## Values

### tsidp

| Key | Default | Description |
| --- | --- | --- |
| `tsidp.image.repository` | `ghcr.io/tailscale/tsidp` | Official upstream image. |
| `tsidp.image.tag` | `"v0.0.15"` | tsidp version; pinned to the release e2e-tested against this operator (see pinning policy above). |
| `tsidp.image.pullPolicy` | `IfNotPresent` | |
| `tsidp.hostname` | `"idp"` | Tailnet hostname; node becomes `<hostname>.<tailnet>.ts.net`. |
| `tsidp.localPort` | `8080` | Pod-local admin/DCR loopback listener; the operator talks to it. |
| `tsidp.funnel` | `false` | Expose tsidp publicly via Tailscale Funnel (`-funnel`). |
| `tsidp.advertiseTags` | `""` | Comma-separated tags (`-advertise-tags`); required with OAuth client secrets. |
| `tsidp.port` | `443` | HTTPS port on the tailnet interface. |
| `tsidp.authKey.existingSecret` | `""` | Name of a pre-created Secret holding the Tailscale auth key. Empty = interactive login via logs. |
| `tsidp.authKey.key` | `"authkey"` | Key in that Secret. |
| `tsidp.persistence.size` | `1Gi` | Size of the `/data` PVC (tsnet identity, signing key, client registry). |
| `tsidp.persistence.storageClass` | `""` | `""` = cluster default; `"-"` = disable dynamic provisioning. |
| `tsidp.extraArgs` | `[]` | Extra CLI args (e.g. `["-enable-sts"]`). |
| `tsidp.extraEnv` | `[]` | Extra env vars (raw `EnvVar` list). |
| `tsidp.resources` | modest requests/limits | |

Note: the chart always passes `-dir=/data`. This flag is required — without
it tsidp writes to its working directory (`/app`, root-owned in the official
image) and returns 500s at runtime.

### operator

| Key | Default | Description |
| --- | --- | --- |
| `operator.image.repository` | `ghcr.io/isvaldi-consulting/tsidp-operator` | |
| `operator.image.tag` | `""` | Empty = chart `appVersion`. |
| `operator.image.pullPolicy` | `IfNotPresent` | |
| `operator.resyncInterval` | `"10m"` | Periodic drift-repair resync. |
| `operator.logLevel` | `""` | Optional `--zap-log-level`: `debug`, `info`, `error`, or a positive integer for more verbosity. `warn` is **not** valid (controller-runtime rejects it; the chart fails at template time). |
| `operator.resources` | modest requests/limits | |

### Other

| Key | Default | Description |
| --- | --- | --- |
| `serviceAccount.create` | `true` | |
| `serviceAccount.name` | `""` | Empty = chart fullname. |
| `serviceAccount.annotations` | `{}` | |
| `rbac.create` | `true` | ClusterRole/Binding: `oidcclients` (+`/status`, `/finalizers`), Secrets, Events. |
| `networkPolicy.enabled` | `false` | Deny pod ingress (see security note). |
| `networkPolicy.extraIngress` | `[]` | Extra `NetworkPolicyIngressRule`s (e.g. allow metrics scrapes on 8081). |
| `nameOverride` / `fullnameOverride` | `""` | |
| `imagePullSecrets`, `podAnnotations`, `podLabels`, `nodeSelector`, `tolerations`, `affinity` | empty | Standard passthroughs. |

The operator serves metrics on `:8081` and health probes (`/healthz`,
`/readyz`) on `:8082`. A headless Service (required by the StatefulSet)
exposes the metrics port.
