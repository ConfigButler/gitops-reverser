# Acceptance precision: we refuse too much, and we explain too little

> **design** — direction-setting; ships no code. Nothing it describes is supported today.
> Captured: 2026-07-14
> Related:
> [README.md](README.md),
> [support-contract.md](support-contract.md),
> [kustomize-support-boundary.md](kustomize-support-boundary.md),
> [orchestrator-knowledge-boundary.md](orchestrator-knowledge-boundary.md),
> [render-root-scoping.md](render-root-scoping.md),
> [values-file-projection.md](values-file-projection.md)

Small, independent fixes to the acceptance gate, each cheap, each visible in
[support-today.md](../../../test/fixtures/gitops-layouts/support-today.md) the moment it
lands. Two of them are correctness bugs in folders we support **today**.

They share one theme: **a refusal must name the thing it refuses, and refuse only that.**

---

## 1. A stray file does not degrade the target. It stops it dead.

Acceptance is all-or-nothing — `Accepted = len(issues) == 0` — and `writeBatch.refusal()`
aborts the **entire flush before a byte is written**. There is no partial mode. So a single
unrecognised file in a `GitTarget` does not mean "that file is skipped". It means the
operator writes **nothing, ever**, for that target.

The corpus shows what that costs:

| Fixture | The stray | Collateral |
|---|---|---|
| `repo-per-environment` | three `.gitignore` files | **20 valid manifests**, across all three environment roots |
| `argocd-plain/apps/frontend` | `ci-metadata.yaml` | six valid manifests |
| `argocd-external-helm/platform/cert-manager` | `values.yaml` | an `Application` **and** a valid `ClusterIssuer` |

And the three strays form a clean spectrum, which is the whole design:

| File | Does the repo say anything about it? | What it actually is |
|---|---|---|
| `.gitignore` | No. Referenced by nothing. | Git plumbing. Genuinely not desired state. |
| `ci-metadata.yaml` | **Yes — the Application excludes it by name** (`spec.source.directory.exclude`) | CI output. The repo already declares it non-desired-state. |
| `values.yaml` | **Yes — the Application includes it by name** (`helm.valueFiles`) | Load-bearing desired state. Refusing it refuses real config. |

We refuse all three identically, as `foreign-file` or `non-krm-yaml`, on the grounds that we
cannot name them. Two of the three are named **in the repository, in fields we already
parse**.

### The escape hatch that is never there

Every one of these messages ends *"remove it or name it in `.gittargetignore`."* There is
**no `.gittargetignore` anywhere in the corpus** — and the shipped template at
[`internal/git/bootstrapped-repo-template/.gittargetignore`](../../../internal/git/bootstrapped-repo-template/.gittargetignore)
lists `.gitignore` as a commented example. The hatch was designed for exactly this case and
is never present, because it only exists in repos we bootstrapped. Onboarding an existing
repo means hand-writing one before anything works.

It also cannot rescue the values file: `*.yaml` is on the catastrophic-pattern deny-list, so
the only way to exempt `values.yaml` is to enumerate it by name — a file that is not a
passenger at all, but the configuration itself.

### The fixes

**1a. A default inert set.** The category already exists: `isRecognizedArtifact` treats
`README.md` and `.sops.yaml` as known-and-never-written. It is simply too small. Add the
files every repository has — `.gitignore`, `.gitattributes`, `.gitkeep`, `LICENSE`,
`CODEOWNERS`. Inert means: *not KRM, never written by us, and referenced by no build
directive.* The last clause is what keeps it honest — this is not a blanket "ignore what we
don't understand", it is "ignore what nothing in the repo depends on".

**1b. Honour the exclusions the repo already declares.** Argo's
`spec.source.directory.exclude` is, in effect, an in-repo `.gittargetignore` that we do not
read. It fits the closed claim vocabulary in
[orchestrator-knowledge-boundary.md](orchestrator-knowledge-boundary.md) — a claim about a
path, read from a document we already parse, needing no orchestrator code. Reading it turns
`ci-metadata.yaml` from a fatal stray into a declared non-target.

**1c. A referenced values file is context.** Handled in
[values-file-projection.md](values-file-projection.md) §2.

**1d. Consider partial acceptance.** The three fixes above shrink the blast radius but do
not remove it — the next unknown file still stops the target. Whether an unmanaged file
should *ever* be able to halt writes to the manifests around it is a bigger question than
this document, but the corpus is a strong argument that the answer is no.

---

## 2. `vars` is not refused, and it corrupts source files

`unsupportedKustomizeFeatureKeys()` lists 17 keys. **`vars` is not one of them.**

A source document containing `$(SOME_VAR)` renders to a substituted value. The live object
carries the substituted value. Mirroring writes it back — **over the `$(VAR)` in the source
file**. The variable is gone, replaced by whatever it resolved to on the day we happened to
observe it.

This is silent corruption, in a folder we accept today, and it is a bug, not a boundary
question. Add `vars` to the deny-list. (`replacements` — the same hazard, the modern
spelling — *is* already on the list, which is what makes the omission look accidental.)

The general form of this hazard is the subject of
[render-root-scoping.md](render-root-scoping.md) §2: **any transformer we tolerate but do
not invert leaks its output into the source.** `vars` is the sharpest instance.

---

## 3. `labels` / `commonLabels` / `annotations` leak the same way

Classed as benign and explicitly supported. They inject metadata into every rendered object;
mirroring bakes that metadata into the source file as drift. F1 solved exactly this problem
for `images` and `replicas` — `SplitDesiredForOverrides` subtracts the override's effect
from the live object before the diff engine ever sees it — and the same subtraction is what
these transformers need.

Two ways out, and they should be chosen deliberately rather than left to drift:

- **subtract** them, F1-style (they are trivially invertible: a known key/value applied to
  every object), or
- **refuse** them, and admit that "benign" was a guess.

Note that this is *not* the same as the GitOps-tool label stripping in `internal/sanitize` —
`commonLabels` are the user's own labels (`app: frontend`), not a tool's stamp, and nothing
strips them.

---

## 4. We accept generated output and refuse its source

In `rendered-manifests`, the folder `rendered/production/` is **accepted** (`3/3/0`). Its
contents open with:

```
# Generated by `kustomize build src/frontend/overlays/production`. DO NOT EDIT.
# Source commit: <sha>. Regenerate with src/render.sh.
```

Anything we write there is clobbered by the next run of `src/render.sh`. Meanwhile the
authored overlay that produced it is **refused** (on `namePrefix`). The polarity is exactly
backwards: we accept the artifact and refuse the intent.

The `Generated{path}` claim already exists in the vocabulary
([orchestrator-knowledge-boundary.md](orchestrator-knowledge-boundary.md)), sourced from
`DO NOT EDIT` headers. Nothing emits it. Emitting it would make this a **Refused** folder
with a reason a user can act on, instead of a silent write that loses their edit at the next
CI run.

---

## 5. Name the feature we actually objected to

`IssueUnsupportedKustomize` says, for every refusal:

> *"kustomization … uses an unsupported feature
> (generators/patches/components/helm/replacements/transformers/namePrefix/nameSuffix/remote
> bases) or malformed images/replicas overrides…"*

The user must now diff that list against their file to work out which one we meant. The repo
scan already gets this right — `unsupportedKustomizeFeatures()` reports *"kustomization uses
unsupported feature(s): **patches**"* — and it reuses the same key list, so the two cannot
drift.

Have the operator's message name the feature it found. Same function, already written.

While there: give each refused feature a **reason and an action**, not just a name. The
reasons differ, and flattening them into one sentence is what makes the boundary feel
arbitrary.

| Feature | Why it is refused | What the user can do |
|---|---|---|
| `remote-base` | the base is fetched over the network; it has no home in this repo, and we never fetch | vendor the base, or target a folder that owns its resources |
| generators **with** a name hash | the hash couples content to name; the object you edited is replaced by a differently-named one on the next render | set `disableNameSuffixHash: true` |
| `secretGenerator` | policy, not invertibility — plaintext secrets in Git contradict the SOPS stance | use SOPS |
| `replacements` / `vars` | the value is derived from another field; writing it here is overwritten on the next render | edit the source field |
| plugins (`generators`, `transformers`) | arbitrary code — the render is unknowable | — |
| `patches` | we cannot author one, today | see [render-root-scoping.md](render-root-scoping.md) §6 |

---

## 6. Order

Ranked by value per hour, and all six are independent:

1. **`vars`** (§2) — a correctness bug; one line.
2. **The default inert set** (§1a) — unblocks three environment roots and 20 manifests in
   one fixture, and every real repo has a `.gitignore`.
3. **Per-feature messages** (§5) — the function exists; call it.
4. **Referenced values files as context** (§1c) — rescues a folder that holds real config.
5. **`Generated{path}`** (§4) — stops us writing into files a script will overwrite.
6. **The transformer leak** (§3) — needs the decision in §3 made first.

Each one changes `support-today.md`, which is the point: the baseline is descriptive, it
regenerates with `task gitops-layouts-baseline`, and *"when it disagrees with the support
contract, that disagreement is the backlog."*
