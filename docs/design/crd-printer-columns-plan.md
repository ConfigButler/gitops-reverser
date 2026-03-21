# CRD Printer Columns Plan

## Summary

Restore and standardize CRD printer columns around the current API surface only, with no legacy naming and no
"gimmick" resource counter unless we discover a truly trivial implementation during execution. The main goal is to
make `kubectl get` useful again across `GitProvider`, `GitTarget`, `WatchRule`, and `ClusterWatchRule`, with `Ready`
shown consistently everywhere and richer columns on `GitProvider` and `GitTarget` for the fields operators actually
care about. In addition to the boolean `Ready` signal, each resource should expose a Flux-style `Status` column that
prints the current `Ready` condition message so operators can see the latest high-level reason directly in list views.

## Key Changes

-
- Add `+kubebuilder:printcolumn` annotations to `api/v1alpha1/gitprovider_types.go` for:
  - `URL` -> `.spec.url`
  - `Ready` -> `.status.conditions[?(@.type=="Ready")].status`
  - `Status` -> `.status.conditions[?(@.type=="Ready")].message`
  - `Age` -> `.metadata.creationTimestamp`
- Add `+kubebuilder:printcolumn` annotations to `api/v1alpha1/gittarget_types.go` for:
  - `Provider` -> `.spec.providerRef.name`
  - `Branch` -> `.spec.branch`
  - `Path` -> `.spec.path`
  - `Ready` -> `.status.conditions[?(@.type=="Ready")].status`
  - `Status` -> `.status.conditions[?(@.type=="Ready")].message`
  - `Age` -> `.metadata.creationTimestamp`
- Fix existing `WatchRule` printer columns in `api/v1alpha1/watchrule_types.go` so they use the current field names
  instead of stale `destinationRef`:
  - `Target` -> `.spec.targetRef.name`
  - `Ready` -> `.status.conditions[?(@.type=="Ready")].status`
  - `Status` -> `.status.conditions[?(@.type=="Ready")].message`
  - `Age` -> `.metadata.creationTimestamp`
- Fix existing `ClusterWatchRule` printer columns in `api/v1alpha1/clusterwatchrule_types.go` to use the current
  target reference:
  - `Target` -> single rendered target identity in the form `namespace/name`
  - `Ready` -> `.status.conditions[?(@.type=="Ready")].status`
  - `Status` -> `.status.conditions[?(@.type=="Ready")].message`
  - `Age` -> `.metadata.creationTimestamp`
- Keep status semantics unchanged: all four resources already expose `Ready` via `status.conditions`, so this is
  primarily a CRD/schema presentation fix rather than a controller-behavior redesign.
- Do not add a total-resource counter in v1 of this change unless, during implementation, there is an obviously
  low-risk existing field that already represents the intended meaning. Today the rule controllers do not own such a
  count, and the current `GitTarget` snapshot stats are diff-oriented, not a stable "current total" value.

## Controller And Status Analysis

- `GitProvider` already exposes conditions in status and is a natural fit for a compact operator-facing view with the
  repository URL and readiness.
- `GitTarget` already maintains richer status than the rule resources, including snapshot progress and diff stats; it
  is the right place for richer operational columns.
- `WatchRule` and `ClusterWatchRule` currently reconcile reference validity, store compiled rule state, and set
  `Ready`; they do not maintain per-rule resource totals.
- A `Status` printer column sourced from the `Ready` condition message is a good fit here and follows familiar Flux
  ergonomics. It gives operators immediate context without requiring a `describe`, as long as controllers keep their
  ready messages short, stable, and human-readable.
- The main tradeoff is noise: if `Ready` messages are overly verbose or churn frequently, the list view becomes harder
  to scan. Implementation should therefore prefer concise one-line messages and avoid embedding low-level error dumps
  when setting ready-condition messages.
- Because of that controller shape, the logical extra columns for rule resources are target identity and readiness,
  not synthetic counters.
- The generated CRDs in `config/crd/bases/configbutler.ai_watchrules.yaml` and
  `config/crd/bases/configbutler.ai_clusterwatchrules.yaml` are currently stale and still point at
  `.spec.destinationRef.name`; regeneration must be part of the change.
- The `ClusterWatchRule` target display should be treated as a UX requirement, not as two separate operator-facing
  fields. Implementation should therefore prefer a single printable value formatted as `targetns/target`.
  If kubebuilder/CRD printer columns cannot concatenate JSONPath values directly, the implementation should introduce
  a small status field owned by the controller for this rendered target string rather than splitting it into `Target`
  and `TargetNS`.

## Sample And Template Alignment

- Regenerate CRDs with `make manifests` so the base CRD YAML matches the corrected annotations.
- Review `test/e2e/templates/gitprovider.tmpl` for demo and e2e consistency with the new `GitProvider` operator view;
  no schema change is needed there, but it should remain a clear example of the `spec.url` field that will now be
  visible in `kubectl get`.
- Review `config/samples/clusterwatchrule.yaml` and keep it aligned with the current `targetRef` naming; no schema
  change is needed, but the sample should remain the canonical example for the fields shown in the new columns.
- Optionally, if the current samples/tests rely on older human wording like `destination`, normalize helper text and
  fixture naming toward `target` where it improves clarity without broad churn.

## Test Plan

- Add or update lightweight API/CRD assertions that verify generated CRDs contain the intended
  `additionalPrinterColumns` for `GitProvider`, `GitTarget`, `WatchRule`, and `ClusterWatchRule`.
- Add or update controller-focused tests only if needed to keep the `Ready` condition messages concise and predictable
  enough for the new `Status` column to be useful.
- Run `make manifests` and verify the resulting CRD YAML shows the corrected JSONPaths and column names.
- Run repo validation in the project-required order after implementation:
  - `make fmt`
  - `make generate` if the annotation/codegen path requires it
  - `make manifests`
  - `make vet`
  - `make lint`
  - `make test`
  - `docker info` before e2e
  - `make test-e2e`
  - `make test-e2e-quickstart-manifest`
  - `make test-e2e-quickstart-helm`

## Assumptions

- Legacy labels and legacy field names are intentionally not preserved.
- "Status on all resources" means a consistent `Ready` printer column on all relevant CRDs, not adding new condition
  types, plus a `Status` printer column sourced from the `Ready` condition message.
- The preferred user-facing labels are current-API labels: `URL`, `Provider`, `Branch`, `Path`, `Target`, `Ready`,
  `Status`, `Age`.
- `ClusterWatchRule` should expose one `Target` column whose displayed value is `namespace/name`.
- No new persisted resource-count field will be designed in this pass unless implementation uncovers a genuinely
  trivial, semantically correct option.
