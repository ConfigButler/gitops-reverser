# 15-mixed-and-hostile

## What this is
A deliberately adversarial folder. Nothing here is invented: every trap is
something real repositories have shipped — a `kustomization.yaml` that is a Flux
CR, an `application.yaml` that is a Deployment, Helm values beside manifests, CI
YAML that is not KRM, a JSON manifest, a `.yml` manifest, Go templates with no
chart, a SOPS Secret, and controllers (Crossplane, kro) whose objects embed
other objects. Collected in one place, this fixture exists to break naive
assumptions about what a file "is" from its name, extension, or directory.

## Layout
```
15-mixed-and-hostile/
├── README.md
├── application.yaml                   # TRAP: apps/v1 Deployment, NOT an Argo Application
├── kustomization.yaml                 # TRAP: Flux Kustomization CR, NOT a kustomize file
├── values.yaml                        # TRAP: Helm values, NOT a K8s object
├── ci/
│   ├── .gitlab-ci.yml                 # YAML, not KRM (GitLab CI pipeline)
│   └── docker-compose.yml             # YAML, not KRM (Docker Compose)
├── mixed/
│   ├── bundle.yaml                    # 4 docs: Deployment, non-KRM, empty, Service
│   ├── deployment.json                # K8s Deployment in JSON, not YAML
│   └── service.yml                    # KRM with a .yml extension
├── templates/
│   └── deployment.yaml                # TRAP: Go template, no Chart.yaml — invalid YAML
├── secrets/
│   └── db.enc.yaml                    # SOPS-encrypted Secret (fake ciphertext)
├── crossplane/
│   └── composition.yaml               # KRM whose spec embeds full KRM as base
├── kro/
│   └── resourcegraphdefinition.yaml   # KRM whose spec embeds templated KRM
└── empty-dir/
    └── .gitkeep                       # placeholder for an otherwise-empty dir
```

## What makes it structurally distinct
Each trap and the assumption it breaks:

- **`application.yaml` — filename implies kind.** Named like an Argo CD
  Application; it is an `apps/v1` Deployment. The filename tells you nothing
  about `kind`.
- **`kustomization.yaml` — a reserved filename implies kustomize config.** It is
  a Flux `Kustomization` CR (`kustomize.toolkit.fluxcd.io/v1`), a real cluster
  object — not `kustomize.config.k8s.io/v1beta1` build config. Same name, two
  APIs.
- **`values.yaml` — YAML implies a Kubernetes object.** It has no
  `apiVersion`/`kind`; it is Helm input, applied by no cluster.
- **`ci/.gitlab-ci.yml`, `ci/docker-compose.yml` — a `.yml`/`.yaml` file implies
  KRM.** Both are YAML config for other systems entirely; their top-level keys
  are job names / Compose services, not Kubernetes fields.
- **`mixed/bundle.yaml` — every `---` chunk implies an object.** Four documents:
  a Deployment, a non-KRM config document, an *empty* document (null node), and
  a Service. A naive splitter that treats each chunk as KRM mishandles docs 2
  and 3.
- **`mixed/deployment.json` — extension implies format.** A valid Kubernetes
  Deployment in JSON. `kubectl` accepts it; a `*.yaml`-only scan never sees it.
  (JSON carries no `#` comments, so this trap is documented here, not in-file.)
- **`mixed/service.yml` — a `*.yaml` glob is complete.** A real Service that a
  `.yaml`-only matcher skips because it ends in `.yml`.
- **`templates/deployment.yaml` — `.yaml` implies parseable YAML, and
  `templates/` implies a Helm chart.** It contains `{{ .Values.replicaCount }}`
  and `{{- toYaml .Values.resources | nindent 12 }}`, so it does not parse as
  YAML, and there is no `Chart.yaml` anywhere to render it.
- **`secrets/db.enc.yaml` — a YAML file's values are readable.** A SOPS Secret:
  structure cleartext, `data` values opaque ciphertext (fake here).
- **`crossplane/composition.yaml` — KRM never nests KRM.** A Crossplane
  `Composition` whose `spec.resources[].base` holds full nested manifests.
- **`kro/resourcegraphdefinition.yaml` — KRM never nests KRM (templated).** A kro
  `ResourceGraphDefinition` whose `spec.resources[].template` embeds a Deployment
  and a Service carrying `${schema.spec.*}` expressions.
- **`empty-dir/` — a directory of YAML implies a manifest directory.** This
  directory holds only a `.gitkeep`; it contains no manifests at all.

Two further traps **cannot be committed as files** and are therefore absent
here, but a real reader must still expect them:

- **A symlink that escapes the folder.** A repo can contain a symbolic link
  pointing at `../../etc/passwd` or at another tree outside this fixture. Git
  stores it as a link, not the target's bytes; a tool that follows links can be
  walked out of the intended scope. This fixture stores no such link (adding one
  to the corpus would be a path-traversal hazard), but the case is real.
- **A genuinely empty directory.** Git cannot track an empty directory at all —
  hence the `.gitkeep` above. A directory that exists in someone's working tree
  but carries no committed entry is invisible in the repository, so "the set of
  directories in Git" is not the same as "the set of directories on disk."

## Open questions
- If a tool infers `kind` from filename, what does it do with `application.yaml`
  and `kustomization.yaml` here — and can it recover once it reads the content?
- When two files claim the reserved name `kustomization.yaml` with different
  APIs across the corpus, how does a tool decide which one a directory means?
- Should a scan key on file extension, on a parsed `apiVersion`/`kind`, or on
  both — and what happens to `deployment.json` and `service.yml` under each?
- In `bundle.yaml`, how should the non-KRM document and the empty document be
  represented: dropped, preserved, round-tripped? Does dropping them change the
  file on rewrite?
- `templates/deployment.yaml` does not parse as YAML. Is it in scope for a
  manifest tool at all, and how would the tool tell it apart from a corrupt file?
- For the Crossplane and kro files, is the "desired state" the outer object, the
  embedded objects, or both? If a tool wants to change the frontend image, does
  it edit the nested `base`/`template`, or is that owned by the controller?
- The embedded objects use `${schema.spec.image}` and patch paths, not literal
  values. Can a tool reason about the effective running image without evaluating
  the controller's templating?
- Given a symlink escaping the folder or a phantom empty directory, what is the
  boundary of "this repository" that a tool should refuse to cross?
