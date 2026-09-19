# CRDs

The `OIDCClient` CustomResourceDefinition (`oidcclients.tsidp.isvaldi-consulting.github.io`)
YAML in this directory is **generated — do not hand-edit or hand-write it**.

It is produced by `controller-gen` into `config/crd/bases/` at the repo root
and copied here by:

```sh
make sync-chart-crds
```

Helm installs everything in `crds/` before rendering templates, but does not
upgrade or delete CRDs on `helm upgrade`/`helm uninstall` — apply CRD updates
manually (`kubectl apply -f charts/tsidp/crds/`) when upgrading across
versions that change the schema.
