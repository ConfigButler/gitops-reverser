# Contextual-namespace example folders

Small, real folder layouts that pin the supported/unsupported boundary for
contextual (kustomize-inherited) namespace inference. Each folder is one scenario;
`contextual_namespace_corpus_test.go` builds the manifest store over each and
asserts the outcome.

- `supported/*` — the store inherits the namespace from the kustomization that
  references the document through its `resources` graph (`NamespaceSource =
  Kustomize`), or keeps an explicit `metadata.namespace` as-is (`Explicit`).
- `unsupported/*` — the store refuses to infer a namespace (`NamespaceSource =
  None`); the ambiguous case also emits an `ambiguous-namespace` diagnostic. These
  are the inputs the pending `RepositoryValid` refusal will fail the GitTarget on.

The `images-overlay`, `replicas-overlay`, and `ambiguous-images` folders pin the
F1 override-chain attribution the same way (`overrides_test.go`); see
`docs/design/gitops-api/f1-images-replicas-edit-through.md`.

See `docs/design/manifest/contextual-namespace-and-kustomize-folder-editing.md`
(the "Supported and unsupported example folders" matrix). Add a new folder here
whenever a new "can we support X?" question comes up.
