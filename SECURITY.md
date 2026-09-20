# Security Policy

## Reporting a vulnerability

Please report vulnerabilities privately via GitHub Security Advisories
("Report a vulnerability" on the repository's Security tab). Do not open a
public issue for security reports. You should receive a response within a
week.

## Supported versions

Only the latest released minor version receives security fixes. The pinned
tsidp version (see `hack/tsidp.Dockerfile` and `versions.yaml`) is the only
combination the project tests; tsidp's own vulnerabilities should be
reported to [Tailscale](https://tailscale.com/security).

## Security model (what deployers must know)

- **The tsidp loopback listener is full-admin plain HTTP** for anything
  inside the pod network namespace — that is the sidecar's authorization
  mechanism, by tsidp's design.
- **Access to the operator pod is worse than IdP admin in the default
  install**: the operator's ServiceAccount holds **cluster-wide Secrets
  read/write** (RBAC cannot be label-scoped; the operator's label selector
  narrows only its cache). `pods/exec`, `pods/ephemeralcontainers`
  (`kubectl debug`), `pods/portforward`, **and plain pod-create in the
  release namespace** (any pod may name the operator's ServiceAccount)
  all yield that token. Restrict all four verbs to cluster admins, run the
  release in a dedicated namespace with no other workloads and no
  delegated pod-create — or better, set `operator.watchNamespaces` in the
  chart, which switches to namespaced Roles and removes cluster-wide
  Secret access entirely.
- **The Tailscale auth key sits in the tsidp container's environment**
  (`TS_AUTHKEY`; the pinned tsidp release has no file-based alternative)
  — an arbitrary-file-read or RCE in tsidp (tailnet-reachable; internet-
  reachable under Funnel) can read `/proc/self/environ`. Prefer a
  **single-use, short-expiry auth key over an OAuth client secret**
  (`tskey-client-...` can mint tailnet keys via the API); after first join
  the node identity persists in `/data`, so you can delete the Secret and
  upgrade the release with `tsidp.authKey.existingSecret=""` to remove the
  env var altogether.
- **The chart isolates cluster credentials from the network-facing
  container**: `automountServiceAccountToken: false` at pod level, with a
  projected ServiceAccount token mounted only into the operator container.
- **tsidp stores client secrets and its signing key in cleartext** under
  its state dir: the PVC and any backups of it are credential material.
- **Client secrets exist in exactly two places**: tsidp's state file and
  the Kubernetes Secret this operator writes. The operator can never
  re-read a secret from tsidp; a lost Secret forces a credential rotation
  (by design — see README).
- **Shared namespaces imply shared fate.** Anyone with OIDCClient write
  access in a namespace can churn spec fields to keep tsidp rewriting its
  whole-file store (latency degradation for all clients), and anyone who
  can create Secrets there can pre-squat a predictable `<name>-oidc`
  Secret name, blocking a future registration (it fails safe with reason
  `SecretConflict`; set an explicit `spec.secretName` to sidestep).
- **CRD validation is the registration gate.** Anyone who can create
  OIDCClient objects can mint OIDC clients; anyone holding a
  `tailscale.com/cap/tsidp` `allow_dcr` grant in the tailnet ACLs can
  bypass the CRD entirely. Grant `allow_dcr` to no one unless another DCR
  consumer exists.
