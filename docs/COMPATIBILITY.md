# tsidp / tsidp-operator compatibility

The operator runs as a sidecar next to [tsidp](https://github.com/tailscale/tsidp)
and talks to its HTTP API. tsidp is pre-1.0 and moves fast, so we do not claim
"works with any tsidp" — instead, every operator release is **pinned and
tested against a specific tsidp release**, and the machinery below keeps that
pin honest.

## Where the pin lives

`hack/tsidp.Dockerfile` is the single source of truth. It contains exactly one
line of substance:

```dockerfile
FROM ghcr.io/tailscale/tsidp:v0.0.15
```

Nothing is built from it — it exists so that standard Docker tooling
(Dependabot) can watch the upstream release. Every other place a tsidp version
appears is derived from this file:

- `charts/tsidp/values.yaml` — the chart's default `tsidp.image.tag`, on the
  line marked `# tsidp-version-pin` (that marker is what
  `hack/sync-tsidp-version.sh` keys on; do not remove it).
- `versions.yaml` — the compatibility matrix (see below).

## What Dependabot does

`.github/dependabot.yml` has a `docker` entry for directory `/hack`. When
Tailscale tags a new tsidp release (images are published to
`ghcr.io/tailscale/tsidp` on every tag), Dependabot opens a PR labeled
`tsidp-bump` that edits the `FROM` line in `hack/tsidp.Dockerfile`.

The compatibility workflow detects that change and refuses to let it merge
untested (make the `e2e` job a required status check on `main` to enforce
this).

## What the compatibility workflow verifies

`.github/workflows/tsidp-compat.yaml` runs on: **every** pull request, a
weekly schedule (canary against registry/bit-rot), and manual
`workflow_dispatch`.

It runs on every PR — not just PRs touching the pin — because a
path-filtered workflow that doesn't trigger reports *no* status check at
all, which would block every normal PR forever once `e2e` is marked
required (an untriggered check is **not** the same as a "skipped"
conclusion). Instead the `e2e` job diffs the PR against its base branch and
decides for itself:

- `hack/tsidp.Dockerfile` **unchanged** → the job succeeds immediately
  (seconds, no cluster), keeping the required check green;
- pin **changed** and `TS_AUTHKEY` is available → the full kind + tailnet
  e2e below runs;
- pin **changed** but `TS_AUTHKEY` is missing → the job **fails** with an
  explicit error. An untested tsidp bump must never become mergeable just
  because credentials were absent.

The `e2e` job runs `make e2e` → `hack/e2e.sh`, which:

1. creates a kind cluster and builds the operator image from the PR's code;
2. loads the pinned (or overridden) tsidp image and installs `charts/tsidp`
   with both images, joining a real tailnet via `TS_AUTHKEY`;
3. applies `config/samples/tsidp_v1alpha1_oidcclient.yaml` and asserts:
   - the credentials Secret `sample-oidc` appears within 120 s with all eight
     keys (`client_id`, `client_secret`, `issuer`, `authorization_endpoint`,
     `token_endpoint`, `userinfo_endpoint`, `jwks_uri`, `redirect_uris`) and a
     non-empty `client_id`;
   - the OIDCClient CR reports `.status.clientId` and a `Ready=True` condition;
   - deleting the CR garbage-collects the Secret (ownerReference cascade)
     within 60 s;
4. prints a clear `E2E PASS` / `E2E FAIL` line and tears everything down.

The `contract-test` job (`make test`, which includes a fake-tsidp contract
suite) always runs — it is the fallback for forks and for repos without
`TS_AUTHKEY`, but only the e2e job proves the *real* tsidp binary still
behaves.

## What versions.yaml promises

`versions.yaml` is the operator↔tsidp compatibility matrix. Each row maps an
operator version (the chart's `appVersion`, which carries the `v` prefix —
e.g. `v0.1.0` — matching the release git tag and image tag) to a tsidp tag
and the UTC date that pair last passed the e2e suite. The standing promise:

> The chart's default `tsidp.image.tag` always equals the tsidp tag of the
> newest e2e-tested row in `versions.yaml`.

Rows are written by `hack/sync-tsidp-version.sh` (also available as
`make sync-tsidp-version`), which reads the pin from `hack/tsidp.Dockerfile`,
updates the `# tsidp-version-pin` line in the chart values, and upserts the
matrix row. It accepts `SYNC_DATE=YYYY-MM-DD` to backdate a row.

The `tested:` field is only stamped by the e2e loop: for a row that already
exists, the script **preserves** the current `tested:` value (e.g.
`pending-first-e2e`) unless `FORCE_TESTED_DATE=1` is set — the
`record-compat` job sets it because it only runs after a passing e2e. A
plain maintainer sync is not evidence the pair passed e2e, so it never
refreshes dates.

### The merge flow for a tsidp bump

1. Dependabot opens the `tsidp-bump` PR editing `hack/tsidp.Dockerfile`.
2. The `e2e` required check must pass on that PR.
3. After merge, a maintainer runs `make sync-tsidp-version` and commits, **or**
   triggers the `tsidp compatibility` workflow via *Run workflow*
   (`workflow_dispatch`) — its `record-compat` job runs the sync script and
   opens the follow-up PR automatically.

Why not fully automatic? Workflows triggered by Dependabot run with a
read-only `GITHUB_TOKEN`, and PRs opened by `GITHUB_TOKEN` don't trigger other
workflows — so we keep the recording step an explicit maintainer action rather
than fighting those limits.

### COMPAT_SYNC_PAT — giving the sync PR CI

The `record-compat` job opens its follow-up PR with
`peter-evans/create-pull-request`. If that PR is created with the default
`GITHUB_TOKEN`, **no workflows run on it** — it shows up with zero checks.
To fix this, add a repository secret named `COMPAT_SYNC_PAT` containing a
[fine-grained personal access token](https://github.com/settings/personal-access-tokens)
scoped to this repository with **Contents: read & write** and
**Pull requests: read & write** permissions. The workflow uses it when
present (`token: ${{ secrets.COMPAT_SYNC_PAT || github.token }}`), so the
sync PR is authored by a real user token and gets normal CI. Without the
secret it falls back to `github.token`, and a maintainer must close and
reopen the PR (or push an empty commit) to trigger checks manually.

## Setting up TS_AUTHKEY

The e2e job needs a Tailscale auth key so the ephemeral kind-hosted tsidp can
join a tailnet:

1. In the [Tailscale admin console → Keys](https://login.tailscale.com/admin/settings/keys),
   generate an auth key that is **ephemeral** (nodes vanish when the test
   pod dies), **reusable** (many CI runs), and **tagged** (e.g. `tag:ci`) so
   it needs no interactive approval. See the
   [auth keys documentation](https://tailscale.com/kb/1085/auth-keys).
2. Add it to the repo as an **Actions** secret named `TS_AUTHKEY`
   (Settings → Secrets and variables → Actions).
3. Also add it as a **Dependabot** secret with the same name
   (Settings → Secrets and variables → Dependabot) — Dependabot-triggered
   workflow runs read from that separate store, and the tsidp-bump PRs are
   exactly the runs that need it.

Without the secret: PRs that leave the pin untouched still pass `e2e`
instantly (the in-job gate short-circuits), but any PR that **changes**
`hack/tsidp.Dockerfile` fails `e2e` outright, and scheduled/manual runs skip
it — `contract-test` remains the only signal. Fine for forks, not for
`main`.

## Manually testing a different tsidp version

Run the `tsidp compatibility` workflow via **Run workflow** and set the
optional `tsidp_tag` input (e.g. `v0.0.16`). The e2e job then tests
`ghcr.io/tailscale/tsidp:<tsidp_tag>` instead of the pin. If it passes, the
`record-compat` job updates `hack/tsidp.Dockerfile`,
`charts/tsidp/values.yaml`, and `versions.yaml`, and opens a PR with all
three — a one-click way to roll the pin forward (or back) without waiting for
Dependabot.

Locally, the same loop is:

```sh
TS_AUTHKEY=tskey-auth-... TSIDP_IMAGE=ghcr.io/tailscale/tsidp:v0.0.16 make e2e
```
