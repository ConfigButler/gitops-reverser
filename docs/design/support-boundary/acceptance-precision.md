# Acceptance precision: we refuse too much, and we explain too little

> **design + implementation record** — some corrections described here have shipped;
> the remaining sections are proposals until their own status says otherwise.
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
lands. One of them is a correctness bug in folders we support **today**.

They share one theme: **a refusal must name the thing it refuses, and refuse only that.**

> **Everything below is still open.** One item this document originally carried has since
> landed and is gone from it: **`vars` was not refused**, so a source document containing
> `$(SOME_VAR)` had the substituted value mirrored back over the variable — silent corruption
> in an accepted folder. #229 closed it, and closed it *structurally*: the unsupported set is
> now derived by **reflecting over kustomize's own `Kustomization` struct**, so a field we
> have never heard of refuses the folder instead of being silently tolerated. The
> hand-maintained 17-key deny-list this document was written against no longer exists. That is
> the shape the remaining items should be fixed in: not "add the case we forgot", but "stop
> being able to forget".

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

**1a. A default inert set.** The category already exists:
[`isRecognizedArtifact`](../../../internal/manifestanalyzer/gittargetignore.go) treats
`README.md`, `.sops.yaml` and `kustomization.yaml` as known-and-never-written. It is simply
too small — three entries, and none of them the file every repository actually has. Add the
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

**1c. A referenced values file is context.** **Shipped (2026-07-16, Argo CD `Application`s and
Flux `HelmRelease`s).** A `values.yaml` a release names through `helm.valueFiles` or
`spec.chart.spec.valuesFiles` is now read-only context, so `platform/cert-manager` and its
`ClusterIssuer` are accepted instead of refused as `non-krm-yaml`. Designed and recorded in
[values-file-projection.md](values-file-projection.md) §2 (Move 1).

**1d. Consider partial acceptance.** The three fixes above shrink the blast radius but do
not remove it — the next unknown file still stops the target. Whether an unmanaged file
should *ever* be able to halt writes to the manifests around it is a bigger question than
this document, but the corpus is a strong argument that the answer is no.

---

## 2. `labels` / `commonLabels` / `annotations` leak into source files

**Resolved by source-form projection.** The old implementation treated transform-supplied
metadata as user drift and copied it into source files. `sourceForm` now preserves source bytes
where the source render already agrees with the live object, so `labels`, `commonLabels`, and
`commonAnnotations` no longer leak into the source. This section remains as the rationale for
the fix, not as a currently open correctness bug.

`vars` was refused because it has no safe source inverse. Metadata transformers are different:
the writer need not invert them when source form proves the rendered value already came from the
repository. Neither outcome makes transformer declarations a general field-authoring surface.

Two ways out, and they should be chosen deliberately rather than left to drift:

- **subtract** them, F1-style (they are trivially invertible: a known key/value applied to
  every object), or
- **refuse** them, and admit that "benign" was a guess.

Note that this is *not* the same as the GitOps-tool label stripping in `internal/sanitize` —
`commonLabels` are the user's own labels (`app: frontend`), not a tool's stamp, and nothing
strips them.

---

## 3. We accept generated output and refuse its source

In `rendered-manifests`, the folder `rendered/production/` is **accepted** (`3/3/0`). Its
contents open with:

```text
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

## 4. Name the feature we actually objected to

`IssueUnsupportedKustomize` says, for every refusal:

> *"kustomization … uses an unsupported feature
> (generators/patches/components/helm/replacements/transformers/namePrefix/nameSuffix/remote
> bases) or malformed images/replicas overrides…"*

The user must now diff that list against their file to work out which one we meant — and
since #229 the list in the message is not even the list we check against, because the check is
now a reflection over kustomize's struct and the sentence is a hand-written string. It can
name a feature we no longer refuse, or miss one we do.

Meanwhile **the feature is already computed, per file.** `parseKustomization` returns the exact
offending keys, and the repo scan already prints them — *"kustomization uses unsupported
feature(s): **patches**"* ([`scan_repo.go`](../../../internal/manifestanalyzer/scan_repo.go)).
The acceptance gate throws that away and substitutes the static sentence.

Have the operator's message name the features it found. The data is already in hand; this is
a plumbing job, not a design one.

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
| `patches` | we cannot author one, today | see [patch-authoring.md](patch-authoring.md) |

---

## 5. Order

Ranked by value per hour, and all five are independent:

1. **The default inert set** (§1a) — unblocks three environment roots and 20 manifests in
   one fixture, and every real repo has a `.gitignore`.
2. **Per-feature messages** (§4) — the data is already computed; pass it through.
3. **Referenced values files as context** (§1c) — rescues a folder that holds real config.
   **Shipped** for Argo CD `Application`s and Flux `HelmRelease`s.
4. **`Generated{path}`** (§3) — stops us writing into files a script will overwrite.
5. **The transformer leak** (§2) — the live correctness bug, but last, because it needs the
   subtract-or-refuse decision made first and neither answer is one line.

Each one changes `support-today.md`, which is the point: the baseline is descriptive, it
regenerates with `task gitops-layouts-baseline`, and *"when it disagrees with the support
contract, that disagreement is the backlog."*
