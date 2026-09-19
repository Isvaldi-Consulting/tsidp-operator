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
  mechanism, by tsidp's design. Restrict `pods/portforward` and `pods/exec`
  on the tsidp pod via RBAC; treat either as equivalent to IdP admin.
- **The chart isolates cluster credentials from the network-facing
  container**: `automountServiceAccountToken: false` at pod level, with a
  projected ServiceAccount token mounted only into the operator container.
- **tsidp stores client secrets and its signing key in cleartext** under
  its state dir: the PVC and any backups of it are credential material.
- **Client secrets exist in exactly two places**: tsidp's state file and
  the Kubernetes Secret this operator writes. The operator can never
  re-read a secret from tsidp; a lost Secret forces a credential rotation
  (by design — see README).
- **CRD validation is the registration gate.** Anyone who can create
  OIDCClient objects can mint OIDC clients; anyone holding a
  `tailscale.com/cap/tsidp` `allow_dcr` grant in the tailnet ACLs can
  bypass the CRD entirely. Grant `allow_dcr` to no one unless another DCR
  consumer exists.
