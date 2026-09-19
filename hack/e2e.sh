#!/usr/bin/env bash
# hack/e2e.sh — end-to-end compatibility test for tsidp-operator + tsidp.
#
# Spins up a kind cluster, installs the charts/tsidp Helm chart (real tsidp
# image + the operator sidecar), applies the sample OIDCClient, and asserts
# the full reconcile lifecycle: credentials Secret created with all expected
# keys, CR status populated, and Secret garbage-collected on CR deletion.
#
# Environment:
#   TS_AUTHKEY    (required) Tailscale auth key for tsidp to join the tailnet.
#   TSIDP_IMAGE   tsidp image ref. Default: FROM line of hack/tsidp.Dockerfile.
#   OPERATOR_IMG  operator image ref. Default: ghcr.io/isvaldi-consulting/tsidp-operator:dev
#   KIND_CLUSTER  kind cluster name. Default: tsidp-e2e
#   SKIP_BUILD    "true" to skip `make docker-build`. Default: false
#   KEEP_CLUSTER  "true" to leave the kind cluster running. Default: false
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ -z "${TS_AUTHKEY:-}" ]]; then
  echo "E2E FAIL: TS_AUTHKEY is required (a Tailscale auth key; ephemeral+reusable recommended)." >&2
  exit 1
fi

for bin in kind kubectl helm docker; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "E2E FAIL: required tool '$bin' not found on PATH." >&2
    exit 1
  fi
done

default_tsidp_image() {
  grep -E '^FROM ' "$ROOT/hack/tsidp.Dockerfile" | awk '{print $2}'
}

TSIDP_IMAGE="${TSIDP_IMAGE:-$(default_tsidp_image)}"
OPERATOR_IMG="${OPERATOR_IMG:-ghcr.io/isvaldi-consulting/tsidp-operator:dev}"
KIND_CLUSTER="${KIND_CLUSTER:-tsidp-e2e}"
SKIP_BUILD="${SKIP_BUILD:-false}"
KEEP_CLUSTER="${KEEP_CLUSTER:-false}"

NAMESPACE="tsidp-e2e"
RELEASE="tsidp-e2e"
AUTHKEY_SECRET="tsidp-authkey"
SECRET_NAME="sample-oidc" # spec.secretName of config/samples/tsidp_v1alpha1_oidcclient.yaml
REQUIRED_KEYS=(client_id client_secret issuer authorization_endpoint token_endpoint userinfo_endpoint jwks_uri redirect_uris)

log() { echo "[e2e] $*"; }

fail() {
  echo "E2E ASSERTION FAILED: $*" >&2
  exit 1
}

diagnostics() {
  log "--- diagnostics ---"
  kubectl -n "$NAMESPACE" get pods -o wide || true
  kubectl -n "$NAMESPACE" get events --sort-by=.lastTimestamp | tail -n 30 || true
  kubectl -n "$NAMESPACE" get oidcclients.tsidp.isvaldi-consulting.github.io -o yaml || true
  kubectl -n "$NAMESPACE" logs --all-containers --tail=100 -l "app.kubernetes.io/instance=${RELEASE}" || true
  log "--- end diagnostics ---"
}

cleanup() {
  local status=$?
  set +e
  trap - EXIT
  if [[ $status -ne 0 ]]; then
    diagnostics
  fi
  helm uninstall "$RELEASE" -n "$NAMESPACE" >/dev/null 2>&1
  if [[ "$KEEP_CLUSTER" == "true" ]]; then
    log "KEEP_CLUSTER=true — leaving kind cluster '${KIND_CLUSTER}' running"
  else
    kind delete cluster --name "$KIND_CLUSTER" >/dev/null 2>&1
  fi
  if [[ $status -eq 0 ]]; then
    echo "E2E PASS: operator ${OPERATOR_IMG} is compatible with tsidp ${TSIDP_IMAGE}"
  else
    echo "E2E FAIL: operator ${OPERATOR_IMG} vs tsidp ${TSIDP_IMAGE} (exit ${status})" >&2
  fi
  exit "$status"
}
trap cleanup EXIT

log "tsidp image:    ${TSIDP_IMAGE}"
log "operator image: ${OPERATOR_IMG}"

# 1. kind cluster (idempotent).
if kind get clusters 2>/dev/null | grep -qx "$KIND_CLUSTER"; then
  log "reusing existing kind cluster '${KIND_CLUSTER}'"
else
  log "creating kind cluster '${KIND_CLUSTER}'"
  kind create cluster --name "$KIND_CLUSTER" --wait 120s
fi
# Point kubectl/helm at THIS cluster. `kind create cluster` switches the
# current context itself, but the reuse path above would otherwise leave
# whatever kube context happened to be current (possibly a real cluster).
kind export kubeconfig --name "$KIND_CLUSTER"

# 2. Build the operator image unless told not to.
if [[ "$SKIP_BUILD" != "true" ]]; then
  log "building operator image via 'make docker-build IMG=${OPERATOR_IMG}'"
  make -C "$ROOT" docker-build IMG="$OPERATOR_IMG"
fi

# 3. Load the locally-built operator image into kind. The tsidp image is
# deliberately NOT pre-loaded: it is public on ghcr, so the kind node pulls
# it directly — `kind load docker-image` of a multi-arch image whose full
# manifest list is not in the local Docker store fails with
# "ctr: content digest ... not found".
kind load docker-image "$OPERATOR_IMG" --name "$KIND_CLUSTER"

# 4. Namespace + Tailscale authkey Secret (idempotent applies).
kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$NAMESPACE" create secret generic "$AUTHKEY_SECRET" \
  --from-literal=authkey="$TS_AUTHKEY" \
  --dry-run=client -o yaml | kubectl apply -f -

# 5. Install the chart with the images under test.
log "installing chart"
helm upgrade --install "$RELEASE" "$ROOT/charts/tsidp" \
  --namespace "$NAMESPACE" \
  --set tsidp.image.repository="${TSIDP_IMAGE%:*}" \
  --set tsidp.image.tag="${TSIDP_IMAGE##*:}" \
  --set tsidp.authKey.existingSecret="$AUTHKEY_SECRET" \
  --set operator.image.repository="${OPERATOR_IMG%:*}" \
  --set operator.image.tag="${OPERATOR_IMG##*:}"

# 6. Wait for the StatefulSet rollout (discover the name; single-sts chart).
sts=""
for _ in $(seq 1 30); do
  sts="$(kubectl -n "$NAMESPACE" get statefulset -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  [[ -n "$sts" ]] && break
  sleep 2
done
[[ -n "$sts" ]] || fail "no StatefulSet appeared in namespace ${NAMESPACE} within 60s"
log "waiting for rollout of statefulset/${sts}"
kubectl -n "$NAMESPACE" rollout status "statefulset/${sts}" --timeout=300s

# 7. Apply the sample OIDCClient.
log "applying sample OIDCClient"
kubectl -n "$NAMESPACE" apply -f "$ROOT/config/samples/tsidp_v1alpha1_oidcclient.yaml"

# 8. Poll up to 120s for the credentials Secret.
secret_ok=false
for _ in $(seq 1 60); do
  if kubectl -n "$NAMESPACE" get secret "$SECRET_NAME" >/dev/null 2>&1; then
    secret_ok=true
    break
  fi
  sleep 2
done
[[ "$secret_ok" == "true" ]] || fail "Secret ${SECRET_NAME} was not created within 120s"
log "Secret ${SECRET_NAME} exists"

# 9. Assert all expected keys are present, and client_id decodes non-empty.
for key in "${REQUIRED_KEYS[@]}"; do
  b64="$(kubectl -n "$NAMESPACE" get secret "$SECRET_NAME" -o jsonpath="{.data.${key}}")"
  [[ -n "$b64" ]] || fail "Secret ${SECRET_NAME} is missing key '${key}' (or it is empty)"
done
client_id="$(kubectl -n "$NAMESPACE" get secret "$SECRET_NAME" -o jsonpath='{.data.client_id}' | base64 -d)"
[[ -n "$client_id" ]] || fail "Secret ${SECRET_NAME} key client_id decodes to an empty string"
log "all ${#REQUIRED_KEYS[@]} Secret keys present; client_id=${client_id}"

# 10. Assert CR status: .status.clientId set and Ready condition True.
cr_name="$(kubectl -n "$NAMESPACE" get oidcclients.tsidp.isvaldi-consulting.github.io -o jsonpath='{.items[0].metadata.name}')"
[[ -n "$cr_name" ]] || fail "no OIDCClient resource found in ${NAMESPACE}"
ready=""
for _ in $(seq 1 30); do
  ready="$(kubectl -n "$NAMESPACE" get oidcclients.tsidp.isvaldi-consulting.github.io "$cr_name" \
    -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)"
  [[ "$ready" == "True" ]] && break
  sleep 2
done
[[ "$ready" == "True" ]] || fail "OIDCClient ${cr_name} Ready condition is '${ready:-<unset>}', want True"
status_client_id="$(kubectl -n "$NAMESPACE" get oidcclients.tsidp.isvaldi-consulting.github.io "$cr_name" -o jsonpath='{.status.clientId}')"
[[ -n "$status_client_id" ]] || fail "OIDCClient ${cr_name} .status.clientId is empty"
log "OIDCClient ${cr_name} is Ready with clientId=${status_client_id}"

# 10b. Exercise the InPlace update path against REAL tsidp: this is the one
# integration that rides tsidp's HTML /edit form contract (not a stable
# JSON API), so it is exactly what a tsidp version bump can silently break.
# Assert the update converges AND preserves the credentials.
log "exercising InPlace update (tsidp /edit contract)"
kubectl -n "$NAMESPACE" patch oidcclients.tsidp.isvaldi-consulting.github.io "$cr_name" --type merge -p \
  '{"spec":{"updatePolicy":"InPlace","redirectUris":["https://sample.example.ts.net/oauth/callback","https://sample.example.ts.net/oauth/callback2"]}}'
inplace_ok=""
for _ in $(seq 1 24); do
  uris="$(kubectl -n "$NAMESPACE" get secret "$SECRET_NAME" -o jsonpath='{.data.redirect_uris}' 2>/dev/null | base64 -d 2>/dev/null || true)"
  ready_now="$(kubectl -n "$NAMESPACE" get oidcclients.tsidp.isvaldi-consulting.github.io "$cr_name" \
    -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)"
  if [[ "$uris" == *"callback2"* && "$ready_now" == "True" ]]; then
    inplace_ok=yes
    break
  fi
  sleep 5
done
[[ -n "$inplace_ok" ]] || fail "InPlace update did not converge (tsidp /edit contract may have changed): redirect_uris=${uris:-<unset>}"
client_id_after="$(kubectl -n "$NAMESPACE" get secret "$SECRET_NAME" -o jsonpath='{.data.client_id}' | base64 -d)"
[[ "$client_id_after" == "$status_client_id" ]] || fail "InPlace update rotated client_id (${status_client_id} -> ${client_id_after}); must preserve credentials"
log "InPlace update converged with credentials preserved"

# 11. Delete the CR and assert the Secret is garbage-collected (ownerRef cascade).
log "deleting OIDCClient ${cr_name}"
kubectl -n "$NAMESPACE" delete oidcclients.tsidp.isvaldi-consulting.github.io "$cr_name" --wait=true
gc_ok=false
for _ in $(seq 1 30); do
  if ! kubectl -n "$NAMESPACE" get secret "$SECRET_NAME" >/dev/null 2>&1; then
    gc_ok=true
    break
  fi
  sleep 2
done
[[ "$gc_ok" == "true" ]] || fail "Secret ${SECRET_NAME} was not garbage-collected within 60s of CR deletion"
log "Secret ${SECRET_NAME} garbage-collected after CR deletion"

log "all assertions passed"
