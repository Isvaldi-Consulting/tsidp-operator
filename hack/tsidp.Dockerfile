# hack/tsidp.Dockerfile
#
# SINGLE SOURCE OF TRUTH for the tsidp version this operator is pinned to.
#
# This is not a real build — nothing is produced from this file beyond a retag.
# It exists so that standard Docker tooling can track the upstream tsidp
# container release:
#
#   1. Dependabot (docker ecosystem, directory "/hack" in .github/dependabot.yml)
#      watches the FROM line below and opens a PR ("tsidp-bump") when
#      ghcr.io/tailscale/tsidp publishes a new release tag.
#   2. That PR touches this file, which triggers the e2e testing loop in
#      .github/workflows/tsidp-compat.yaml (kind cluster + real tsidp +
#      operator sidecar + sample OIDCClient assertions).
#   3. After the bump merges, hack/sync-tsidp-version.sh propagates the tag
#      into charts/tsidp/values.yaml (the `# tsidp-version-pin` line) and
#      records the tested pair in versions.yaml.
#
# Keep this file to exactly one FROM line. Do not add build steps.
FROM ghcr.io/tailscale/tsidp:v0.0.15
