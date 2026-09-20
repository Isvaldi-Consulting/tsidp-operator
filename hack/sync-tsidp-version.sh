#!/usr/bin/env bash
# hack/sync-tsidp-version.sh — propagate the pinned tsidp version.
#
# Reads the single source of truth (the FROM line in hack/tsidp.Dockerfile)
# and:
#   1. Updates the tsidp image tag in charts/tsidp/values.yaml. The target
#      line MUST carry the `# tsidp-version-pin` marker comment — the script
#      refuses to touch the file otherwise, so it can never mangle an
#      unrelated `tag:` line.
#   2. Upserts a row in versions.yaml mapping the operator version (the
#      appVersion in charts/tsidp/Chart.yaml, "v"-prefixed, e.g. "v0.1.0")
#      to the tsidp tag and a test date.
#
# The `tested:` field is only stamped by the e2e loop: for an EXISTING
# (operator, tsidp) row the current value is preserved unless
# FORCE_TESTED_DATE=1 is set (a maintainer sets it after a green e2e after a
# passing e2e run). New rows get $SYNC_DATE if set, else today's UTC date.
#
# Idempotent: running it twice produces no change (existing rows are left
# untouched without FORCE_TESTED_DATE=1).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DOCKERFILE="$ROOT/hack/tsidp.Dockerfile"
VALUES="$ROOT/charts/tsidp/values.yaml"
CHART="$ROOT/charts/tsidp/Chart.yaml"
VERSIONS="$ROOT/versions.yaml"
MARKER="# tsidp-version-pin"

err() {
  echo "sync-tsidp-version: ERROR: $*" >&2
  exit 1
}

[[ -f "$DOCKERFILE" ]] || err "$DOCKERFILE not found"
[[ -f "$VALUES" ]] || err "$VALUES not found"
[[ -f "$CHART" ]] || err "$CHART not found"
[[ -f "$VERSIONS" ]] || err "$VERSIONS not found"

# --- 1. Parse the pinned tsidp image tag. -----------------------------------
image="$(grep -E '^FROM ' "$DOCKERFILE" | awk '{print $2}')"
[[ -n "$image" ]] || err "no FROM line found in $DOCKERFILE"
tag="${image##*:}"
[[ -n "$tag" && "$tag" != "$image" ]] || err "FROM line in $DOCKERFILE has no tag: '$image'"
echo "sync-tsidp-version: pinned tsidp image is ${image} (tag ${tag})"

# --- 2. Update charts/tsidp/values.yaml on the marked line only. ------------
grep -qF "$MARKER" "$VALUES" || err "marker '${MARKER}' not found in ${VALUES}.
The tsidp image tag line in values.yaml must be annotated with '${MARKER}'
(e.g.:   tag: \"v0.0.15\" ${MARKER}) so this script can update it safely.
Refusing to guess which 'tag:' line to edit."

# sed -i.bak works on both GNU and BSD sed; only lines carrying the marker
# are eligible for replacement.
sed -i.bak -e "/${MARKER}/ s|tag:[[:space:]]*\"[^\"]*\"|tag: \"${tag}\"|" "$VALUES"
rm -f "${VALUES}.bak"
grep -F "$MARKER" "$VALUES" | grep -qF "\"${tag}\"" \
  || err "failed to update the marked tag line in ${VALUES} to \"${tag}\""
echo "sync-tsidp-version: ${VALUES} pin -> \"${tag}\""

# --- 3. Upsert the row in versions.yaml. -------------------------------------
operator_version="$(awk -F': *' '/^appVersion:/ {gsub(/"/, "", $2); print $2; exit}' "$CHART")"
[[ -n "$operator_version" ]] || err "could not read appVersion from ${CHART}"
sync_date="${SYNC_DATE:-$(date -u +%F)}"

grep -qE '^compatibility:' "$VERSIONS" || err "no 'compatibility:' list found in ${VERSIONS}"

pair_exists() {
  awk -v op="$operator_version" -v tag="$tag" '
    index($0, "- operator:") > 0 { inblock = index($0, "\"" op "\"") > 0 }
    index($0, "tsidp:") > 0 && inblock && index($0, "\"" tag "\"") > 0 { found = 1 }
    END { exit found ? 0 : 1 }
  ' "$VERSIONS"
}

tmp="$(mktemp)"
if pair_exists; then
  if [[ "${FORCE_TESTED_DATE:-}" == "1" ]]; then
    # Stamp the tested date of the existing (operator, tsidp) row. Only the
    # e2e loop should do this (a maintainer sets FORCE_TESTED_DATE=1 after
    # a passing run).
    awk -v op="$operator_version" -v tag="$tag" -v date="$sync_date" '
      index($0, "- operator:") > 0 { inblock = index($0, "\"" op "\"") > 0 }
      index($0, "tsidp:") > 0 { intarget = inblock && index($0, "\"" tag "\"") > 0 }
      index($0, "tested:") > 0 && intarget {
        sub(/tested:[[:space:]]*"[^"]*"/, "tested: \"" date "\"")
        intarget = 0
      }
      { print }
    ' "$VERSIONS" > "$tmp"
    mv "$tmp" "$VERSIONS"
    echo "sync-tsidp-version: updated existing row operator=${operator_version} tsidp=${tag} tested=${sync_date}"
  else
    # Preserve whatever `tested:` already says (e.g. pending-first-e2e, or a
    # real e2e date) — a plain sync run is not evidence the pair passed e2e.
    rm -f "$tmp"
    echo "sync-tsidp-version: row operator=${operator_version} tsidp=${tag} already exists; preserving its tested value (set FORCE_TESTED_DATE=1 to stamp it)"
  fi
else
  # Insert a new row directly under the compatibility: key (newest first).
  awk -v op="$operator_version" -v tag="$tag" -v date="$sync_date" '
    { print }
    /^compatibility:/ && !done {
      print "  - operator: \"" op "\""
      print "    tsidp: \"" tag "\""
      print "    tested: \"" date "\""
      done = 1
    }
  ' "$VERSIONS" > "$tmp"
  mv "$tmp" "$VERSIONS"
  echo "sync-tsidp-version: added row operator=${operator_version} tsidp=${tag} tested=${sync_date}"
fi

echo "sync-tsidp-version: done"
