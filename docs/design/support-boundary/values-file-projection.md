# The values file: refused for where it sits, not for what it is

> **design + implementation record** — Move 1 (a referenced values file is read-only context)
> has shipped; Move 2 (projecting the file as an editable object) is still design.
> Captured: 2026-07-14. Move 1 shipped: 2026-07-16.
> Related:
> [README.md](README.md),
> [support-contract.md](support-contract.md),
> [expansion-boundary-and-corpus-organisation.md](expansion-boundary-and-corpus-organisation.md),
> [write-only-encrypted-secrets.md](write-only-encrypted-secrets.md),
> [orchestrator-knowledge-boundary.md](orchestrator-knowledge-boundary.md),
> [acceptance-precision.md](acceptance-precision.md),
> [finished/higher-level-krm-documents.md](finished/higher-level-krm-documents.md)

[expansion-boundary-and-corpus-organisation.md](expansion-boundary-and-corpus-organisation.md)
names the free-standing Helm values file as *"the single highest-leverage thing available
for the Helm story"* and says it *"deserves a design of its own rather than a decision
here."* This is that design.

The boundary it must not move: **we edit the intent layer, never the expansion layer.** We
never render `templates/`. We never learn what a value *means*. Everything below treats a
values file as **bytes with a home in Git**, and nothing more.

---

## 1. The finding: the refusal is location-dependent, not content-dependent

The same Helm values, expressed six ways in the corpus, get five different verdicts:

| Where the values live | Fixture | Verdict today |
|---|---|---|
| `Application.spec.source.helm.valuesObject` (structured map) | `argocd-external-helm/applications/external-dns.yaml` | **Editable** |
| `Application.spec.source.helm.values` (YAML **as a string**) | `argocd-external-helm/applications/cert-manager.yaml` | **Editable** — as an opaque blob |
| `Application.spec.source.helm.parameters` | `helm-environment-values/argocd/*.yaml` | **Editable** |
| `HelmRelease.spec.values` inline, `valuesFrom` → a hand-written `ConfigMap` | `flux-helmrelease/infrastructure/controllers/ingress-nginx/` | **Editable** |
| a `values.yaml` alone in a `values/` directory | `argocd-external-helm/values/**` | **Not seen at all** — no KRM in the dir, so never a candidate |
| a `values.yaml` **next to** KRM | `argocd-external-helm/platform/cert-manager/values.yaml` | **Refused** — and it takes the folder with it |
| a `values.yaml` wrapped by a `configMapGenerator` | `flux-helmrelease/apps/frontend/` | **Refused** — the generator is on the deny-list |

Read that column again. Values are editable when a human wraps them in a `ConfigMap`, and
refused when `configMapGenerator` does the identical wrapping — with
`disableNameSuffixHash: true`, so the result is deterministic. They are editable as a *string*
inside an Application, and refused as a *file* on disk. They are invisible when alone and
fatal when adjacent.

The worst case is worth spelling out. `platform/cert-manager/values.yaml` is:

- **referenced by name** by the Application in the same folder
  (`helm.valueFiles: [$values/platform/cert-manager/values.yaml]`),
- therefore **load-bearing desired state** — it is *the* configuration of cert-manager, and
- refused as `non-krm-yaml: "YAML is not a Kubernetes manifest"`, which **also takes down a
  perfectly valid `ClusterIssuer`** sitting beside it, because acceptance is all-or-nothing.

We refuse the folder on the grounds that we do not know what the file is. The repository is
telling us what it is, in a field we already parse.

---

## 2. Two moves, in order

### Move 1 (cheap, immediate): a referenced values file is context, not junk

> **Shipped (2026-07-16).** A values file named by an `Application`'s `helm.valueFiles` is
> read-only context in the acceptance gate: understood, never written, and never a refusal for
> the folder it sits in. The claim is read in
> [`internal/manifestanalyzer/valuefiles.go`](../../../internal/manifestanalyzer/valuefiles.go)
> and suppresses the `non-krm-yaml` refusal in `acceptance.go`; `platform/cert-manager` flips
> refused → accepted in [support-today.md](../../../test/fixtures/gitops-layouts/support-today.md).
> Both path-valued spellings ship: an Argo CD `Application`'s `helm.valueFiles` and a Flux
> `HelmRelease`'s `spec.chart.spec.valuesFiles`, resolved through one candidate set (repo-root
> `$values/…`, whole-repo, and co-located subtree).

A values file named by an `Application`'s **`helm.valueFiles`**, or by a `HelmRelease`'s
**`spec.chart.spec.valuesFiles`**, is **Read-only context** — a file we understand, never
write, and never refuse the folder over.

**Only the path-valued fields, and this distinction is load-bearing**, because the three
spellings are three different surfaces and only one of them is a file at all (both verified
against upstream, not inferred from the names):

| Field | What it holds | Surface |
|---|---|---|
| Argo `helm.valueFiles`, Flux `spec.chart.spec.valuesFiles` | a **path** — `$values/platform/cert-manager/values.yaml` | **a file in the repo.** This document's subject. |
| Argo `helm.valuesObject` (a `runtime.RawExtension`), Flux `spec.values` | **inline YAML**, embedded in the Application/HelmRelease itself | not a file. It is a *field of a KRM document we already parse*, so it is editable exactly as any other field of that document — no projection, no new claim. |
| Flux `spec.valuesFrom` | a **KRM object reference** — `Secret/my-secret-values`, with a `valuesKey` | not a file either. It names a ConfigMap or Secret, which is *already* a document the store handles (and a Secret drags in [write-only-encrypted-secrets.md](write-only-encrypted-secrets.md), a different problem with a different answer). |

Collapsing the three would claim a file-projection capability over content that lives inside
another document, or inside a Secret. The read-only-context rule below is scoped to the first
row.

This needs no new API and no renderer. It is one more claim in the closed vocabulary that
[orchestrator-knowledge-boundary.md](orchestrator-knowledge-boundary.md) already defines —
knowledge arriving *"as ordinary KRM documents in the repository that Tier 0 has already
parsed"*, requiring only a group, a kind, and a handful of field paths. Never Argo's or
Flux's code.

It rescues `platform/cert-manager/` and its `ClusterIssuer` on its own. It is filed with the
other classification fixes in [acceptance-precision.md](acceptance-precision.md), because it
is the same bug: **we refuse files we cannot name, in a repo that names them.**

### Move 2: project the file as an object

Read-only context is honest but unsatisfying — the user still cannot change a value. The
precedent for going further is already in this folder.

[write-only-encrypted-secrets.md](write-only-encrypted-secrets.md) faced the same shape: a
document we cannot store as itself. Its answer was to **project it into a kind we can**, and
its punchline is the one to reuse — *"a refusal becomes a capability."*

> **Project a free-standing values file as a synthetic KRM object. The user edits the
> object; the operator writes the bytes straight back to the file.**

Why this is not chart inflation, and not a widening of the boundary:

- **No renderer stands between the object and Git.** The file is plain YAML we own end to
  end. Round-tripping it is the same comment-preserving `yaml.Node` edit the operator
  already performs on every manifest — the pipeline is kind-agnostic, which is precisely
  what [finished/higher-level-krm-documents.md](finished/higher-level-krm-documents.md)
  proved when a `HelmRelease` needed no new code.
- **Fan-in = 1 gates it for free.** The analyzer can already see which
  `Application`/`HelmRelease` references which file. A values file referenced by exactly one
  is editable. `values/ingress-nginx/common.yaml` — referenced by several — has fan-in > 1
  and **is refused by the existing rule with no new machinery**. The corpus contains both
  cases, plus two orphan values files referenced by nobody.
- **We still never learn what a value means.** `replicaCount: 4` is a scalar at a path. The
  operator edits the field a human pointed it at. It does not know that the chart turns it
  into a `Deployment`.

What the chart *produces* remains untouchable: chart-rendered objects are expansion output,
carry no `ownerReference`, and are **Not mirrored**. That line does not move.

---

## 3. The open questions this design must answer

The projection is the right shape. The details are genuinely undecided, and pretending
otherwise would be the same mistake this document is correcting.

**What is the object?** A cluster-scoped or namespaced CR (`ValuesFile`? `HelmValues`?) whose
identity is a path. That is uncomfortable: every other object the operator mirrors is *live
state observed in a cluster*, and this one is a file lifted into the API so it can be
edited. The `EncryptedSecret` precedent says the discomfort is acceptable when the
alternative is a permanent refusal. It should still be stated as the cost it is.

**Who creates it?** Nothing in the cluster reconciles a values file. The object exists only
as an editing surface, which means the operator would be *serving* an object rather than
*mirroring* one — a real departure, and the crux of the design.

**Does it fit the aggregated API?** The read side is a projection of Git; the write side is a
write-back. This is closer to the existing aggregated-API write path than to the watch path,
and that is probably where it belongs.

**Layered values.** `helm-environment-values` layers `chart/values.yaml` →
`values/common.yaml` → `values/<env>.yaml` → the Application's `parameters` → and then a
hidden `chart/.argocd-source.yaml` that **overrides all of them**. Editing "the value" is
ambiguous when five files can supply it. Fan-in = 1 refuses the shared layers; the hidden
dotfile is an `OverriddenBy` claim and must refuse the folder or the edit, loudly. **This
document should not try to resolve the layering. It should refuse it and say so.**

**The `helm.values` string blob.** `Application.spec.source.helm.values` is a YAML document
embedded in a string field. It is "editable" today only in the sense that you can replace
the whole blob. Editing a scalar *inside* it means parsing a nested document, and the
comment-preservation guarantee does not survive the round trip. The honest recommendation is
to **name `valuesObject` as the supported surface** — structured, ordinary YAML, editable
with no special case — and to document the string form as a blob we do not reach into.
`valuesObject` is un-adjudicated in the docs today and this is the moment to adjudicate it.

**What makes a chart a chart?** Move 1 needs to tell a values file apart from a chart's own
`values.yaml`. `Chart.yaml` + `templates/` is the signal, and the whole chart folder is
skipped as a unit. `mixed-and-hostile` plants a `templates/` directory with no `Chart.yaml`,
so the detector must not be fooled by either half alone. This residual is already logged in
[expansion-boundary-and-corpus-organisation.md](expansion-boundary-and-corpus-organisation.md).

---

## 4. Where this leaves the Helm story

| What a user wants to do | Verdict |
|---|---|
| Bump a chart version | **Editable** today |
| Change a value on a `HelmRelease` (`spec.values`) | **Editable** today |
| Change a value via `Application` `parameters` / `valuesObject` | **Editable** today |
| Change a value held in a hand-written `ConfigMap` (`valuesFrom`) | **Editable** today |
| Change a value in a values file **referenced by exactly one release** | **Planned: Editable** — §2, Move 2 |
| Keep a values file in a folder without killing the folder | **Read-only context** — §2, Move 1 (shipped) |
| Change a value in a **shared** values file (`common.yaml`) | **Refused** — fan-in > 1, and correctly so |
| Change a value that a hidden `.argocd-source.yaml` overrides | **Refused** — say why, loudly |
| Edit the chart's `templates/` | **Refused**, permanently |
| Edit a chart-rendered object | **Not mirrored** — expansion layer |

The first four rows already work, and row six (Move 1) now ships — a referenced values file is
read-only context, so the folder is no longer refused for holding it. What the product is still
missing is row five: projecting the file as an editable object (§2, Move 2).
