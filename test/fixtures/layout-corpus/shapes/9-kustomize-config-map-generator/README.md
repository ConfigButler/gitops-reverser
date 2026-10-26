# 9: A ConfigMap a `configMapGenerator` renders

> **This scenario exercises the write path, not placement.** No document holds the object: a
> `configMapGenerator` builds it from files beside the kustomization, and those files are where a
> live edit lands.

Generators are how kustomize users keep configuration as files: a `menu.yaml` or a `flags.env`
next to the manifests, wrapped by a generator. With the name hash disabled the generated object
keeps its name, so the edit can be written into the input and the re-render oracle can check it.

## Starting repository

```text
apps/coffee/                 # config/gittarget.yaml
  kustomization.yaml         # four generator entries, disableNameSuffixHash: true
  deployment.yaml
  menu.yaml                  # coffee-menu: files: [menu.yaml]
  flags.env                  # coffee-flags: envs: [flags.env]
  regions/eu.env, us.env     # coffee-regions: two envs: files
  branding/                  # coffee-branding: files: (one text, one binary) + literals:
apps/hashed/                 # config/gittarget-hashed.yaml: the name hash left on
apps/shop/
  base/                      # a generator in the base
  overlays/prod/             # config/gittarget-prod.yaml: reads the base
  overlays/staging/          # config/gittarget-staging.yaml: behavior: merge over the base
```

## What a write does

| Live change | Result |
|---|---|
| `menu.yaml` key changed | the file's bytes become the live value: [`expected-menu-price-changed.patch`](expected-menu-price-changed.patch) |
| `envs:` key changed | that line is edited in place: [`expected-flags-value-changed.patch`](expected-flags-value-changed.patch) |
| key added, one `envs:` file | `KEY=value` is appended: [`expected-flags-key-added.patch`](expected-flags-key-added.patch) |
| `envs:` key removed | the line is removed: [`expected-flags-key-removed.patch`](expected-flags-key-removed.patch) |
| `files:` key removed | refused: [`expected-menu-key-removed-status.yaml`](expected-menu-key-removed-status.yaml) |
| key added, two `envs:` files | refused: [`expected-regions-key-added-status.yaml`](expected-regions-key-added-status.yaml) |
| key added, no `envs:` file | refused: [`expected-branding-key-added-status.yaml`](expected-branding-key-added-status.yaml) |
| `literals:` key changed | refused: [`expected-branding-literal-changed-status.yaml`](expected-branding-literal-changed-status.yaml) |
| `binaryData` changed | refused: [`expected-branding-binary-changed-status.yaml`](expected-branding-binary-changed-status.yaml) |
| label from the entry's `options:` changed | refused: [`expected-branding-label-changed-status.yaml`](expected-branding-label-changed-status.yaml) |
| name hash not disabled | folder refused, naming the setting: [`expected-hashed-status.yaml`](expected-hashed-status.yaml) |
| generator in a read-only base | refused: [`expected-base-owned-status.yaml`](expected-base-owned-status.yaml) |
| `behavior: merge` over a base's generator | folder refused: [`expected-behavior-merge-status.yaml`](expected-behavior-merge-status.yaml) |

A changed key is edited on its own line, so comments, order and every untouched line keep their
bytes. Every write goes through the re-render oracle: the folder is built again with the planned
bytes, and the commit happens only when the generator then renders the live object.

A refusal has the same cause each time: the change has no single input file to land in. Writing a
ConfigMap document beside the generator instead would collide with the generated object in the
build, so nothing is written.
