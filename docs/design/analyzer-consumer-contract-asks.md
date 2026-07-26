# Three asks from a downstream consumer of the analyzer

> Status: accepted as work, unscheduled. Written for an implementing party — every
> claim below was checked against the tree at `v0.39.1`, and each ask ends with the
> decisions only a maintainer can make.
> Date: 2026-07-26
> Companion to [config-surface-for-a-structured-repository.md](../future/config-surface-for-a-structured-repository.md),
> which makes the same argument one layer up: the engine learned something and the
> contract did not carry it.

The three asks come from a product that consumes GitOps Reverser two ways at once — it
links `pkg/manifestanalyzer` as a module, and it execs `manifest-analyzer --mode
scan-repo --format json` from a second tool that does not link us at all. It runs the
reverser itself as an image, pinned to the module by two constants with a build-breaking
test on their side (their `internal/cluster/cluster.go`). That makes it
the first consumer that feels every seam in our published contract, and all three asks
are the same complaint: **a fact our own doc comments assert is carried by prose that
nothing tests, so it drifts silently and a consumer encodes the stale version.**

One of them already shipped a wrong sentence to a real user because of it. That is the
bar these asks are answering, and it is why the fix in each case is *data plus a test*,
not better prose.

---

## Ask 1 — put the permanence of a refusal in the data

### What they asked for

A third field on `RefusalReason`, filled in by the check that raised it, saying whether
the refusal can ever stop being one. Plus the same field on `Issue`, which is the same
taxonomy one level down.

### Why — verified

The consumer's onboarding wizard shows a human every folder in their repository and why
each one cannot be picked. *"Not supported yet"* is a wait; *"cannot be synced"* is a
redesign; and the two sentences were split on the only signal the type offers, a match on
one code:

```go
if r.Code == manifestanalyzer.ReasonRefusedStructural {
    fc.Permanent = true          // everything else fell through to "not supported yet"
}
```

They took that from our doc comments, and both halves of what they read are now wrong.

**The axis went degenerate.** `ReasonOverlayFanOutUnsupported` is marked `// Deprecated:
no longer emitted` at [`pkg/manifestanalyzer/repo.go:42`](../../pkg/manifestanalyzer/repo.go)
— render-root scoping shipped and external-base overlays are adopted. That was the right
change, and it deleted one side of a two-valued distinction a consumer had already
encoded, with no signal that it had happened.

**The stale comment that caused it is still there.** `LayoutKustomizeOverlay` at
[`pkg/manifestanalyzer/repo.go:23`](../../pkg/manifestanalyzer/repo.go) says such folders
are "Refused today, with a forward-looking reason", four lines above the constant saying
the scanner now adopts them. That is the sentence their code was written from. **Fixed in
the same commit as this document** — but the fix is worth nothing on its own, because
nothing stops the next one.

**The code space was never two-valued.** `RefusalReason.Code` is populated from exactly
two places: the literal `ReasonRefusedStructural`, and `issuesToReasons`
([`internal/manifestanalyzer/scan_repo.go:409`](../../internal/manifestanalyzer/scan_repo.go)),
which sets `Code: string(iss.Kind)` for **every** acceptance issue. So the live value set
is the eighteen `IssueKind` constants plus `refused-structural` — and most of them are
neither permanent nor pending. They are **fixable today by the person looking at the
screen**. A folder refused for `invalid-yaml` is one broken document from working, and its
owner was told "not supported yet".

They are correct that no consumer can maintain this table outside our repo. We added
fifteen codes without any of them carrying permanence.

### The shape

Additive, and inert for anyone who ignores it:

```go
// Permanence says whether a refusal can ever stop being one. It is set by the check
// that raised the refusal, because only that check knows. Consumers MUST treat an
// unrecognised or absent value as PermanenceUnknown and say nothing about the future.
type Permanence string

const (
    PermanenceUnknown   Permanence = ""                 // not classified; say nothing
    PermanenceFixable   Permanence = "fixable"          // change the repo or the GitTarget
    PermanencePending   Permanence = "pending-upstream" // a future release may accept it
    PermanencePermanent Permanence = "permanent"        // the support boundary; never a "not yet"
)

// Actor names who can act on a fixable refusal. Empty when unclassified or when
// nobody can act.
type Actor string

const (
    ActorUnknown  Actor = ""
    ActorAuthor   Actor = "repository-author" // the person who owns the files
    ActorPlatform Actor = "platform-operator" // the person who owns the GitTarget
)

type RefusalReason struct {
    Code   string `json:"code"`
    Detail string `json:"detail"`
    // Permanence is empty when the check did not classify itself.
    Permanence Permanence `json:"permanence,omitempty"`
    // Actor is empty unless Permanence is PermanenceFixable.
    Actor Actor `json:"actor,omitempty"`
}
```

`Issue` takes both fields for the same reason: a policy that treats an issue as blocking
is making this decision already, without the data to make it.

Ship the actor field. The consumer offered it as optional; it is the cheaper half of the
same constant, and two of our codes are fixable **only** by the platform operator, whom
their wizard does not have on the screen. Without it, `out-of-scope` renders as "fix your
repository" to someone who cannot.

### The classification — our answers

This is the part no consumer can write, so it is written here in full. Derived from each
kind's own doc comment and its raise site.

| Code | Permanence | Actor | Basis |
|---|---|---|---|
| `invalid-yaml` | fixable | author | the document does not parse |
| `duplicate-identity` | fixable | author | two documents claim one identity |
| `impure-managed-file` | fixable | author | split the file |
| `mixed-managed-allowlisted` | fixable | author | move the kustomization to its own file |
| `ignore-shadows-managed` | fixable | author | narrow the `.gittargetignore` pattern |
| `non-krm-yaml` | fixable | author | remove it, or ignore it |
| `foreign-file` | fixable | author | remove it, or ignore it |
| `foreign-symlink` | fixable | author | the *rule* is permanent, the *folder* is not — remove the link |
| `foreign-submodule` | fixable | author | as above; relocate the submodule |
| `out-of-scope` | fixable | **platform** | widen the GitTarget's scope |
| `write-escapes-scope` | fixable | **platform** | widen `spec.path`, or re-place the write |
| `render-does-not-match-live` | fixable | **platform** | a diverged live value is out-of-band substitution, not a render artifact |
| `write-fan-in` | **pending** | — | the doc comment says per-render-root scoping generalizes this |
| `unplaceable-edit` | **permanent** | — | its comment argues the alternative is measurably wrong, not merely risky |
| `refused-structural` | **permanent** | — | the support boundary by definition |

Note what the `foreign-symlink` row settles, because it is the rule for every future
check: **permanence classifies the folder's prospects, not the rule's.** A rule we will
never relax can still produce a refusal the author clears in one commit. Classify what the
reader can do, since that is the sentence the field exists to write.

### Three that need a decision, not a lookup

**`unsupported-kustomize` — must be classified per construct at the raise site.** This is
the case that proves the whole ask: one code, both answers. Its doc comment lists
generators, components, Helm inflation, replacements, transformers, name prefixes and
remote bases; `v0.37.0` shipped exactly the kind of loosening that moves one of those, and
`patches:` already moved. Suggested starting split, for the implementer to confirm
construct by construct:

- remote bases, Helm inflation → `pending-upstream`
- generators, replacements, transformers, name prefix/suffix → **decide**; they are the
  constructs whose output the writer cannot map back to source at all, which reads
  permanent, but so did `patches:` before it moved

A static per-code map cannot express this. The field can, and it is why the field goes on
the emitted reason rather than into a table beside the constants.

**`unresolved-krm` — splits at the raise site.** A kind absent because its CRD is not
installed is `fixable`/`platform`. A kind that is ambiguous, unserved, or missing a verb is
`pending` at best. One code, two answers, same argument as above.

**`kustomize-render-refused` — decide what it classifies.** It refuses a *write*, not a
folder, so "can this folder ever be picked" is the wrong question for it. Either classify
it `permanent` (the oracle will always refuse a write it cannot vouch for) or leave it
`Unknown` deliberately and say so in its comment.

### Implementation notes

- The constant goes at the **raise site**, not into a map beside the type. A map is the
  thing that drifted; a field on the emitted value cannot be raised without a decision.
- `issuesToReasons` becomes the projection for both new fields — it is already the single
  choke point through which every issue becomes a reason.
- Add a test that **every** `IssueKind` constant is classified, failing on a new
  unclassified kind. That test is the ask. Without it this document is prose again in two
  releases, which is how we got here.
- `PermanenceUnknown` must stay the zero value so an unclassified path degrades to silence
  rather than to a confident wrong sentence.

### Meanwhile, downstream

They have flipped their default from "not supported yet" to unknown — an unrecognised code
now says only "this folder cannot be picked" — and deleted their `Permanent bool`. That is
worse for their user than the truth and is the honest maximum from `{Code, Detail}`.

---

## Ask 2 — a JSON document from a binary is a contract with no compile-time signal

### What they asked for

Four things, in their order of value:

1. The analyzer's own version **in the report**.
2. A `manifest-analyzer --version` flag.
3. One sentence on what a `SchemaVersion` bump asserts, and whether a reader should refuse
   a version it does not know.
4. The analyzer binary attached to the release we already sign.

### Why — verified

Our package doc invites this consumer by name: *"Exec the binary if Go is not your
language; import this package if it is."* When they link the package, a rename is a build
failure. When they exec the binary, the document is all they hold, and it does not say
what produced it. `RepoReport` and `FolderReport` carry `schemaVersion` and nothing else
about provenance; the flag set in `cmd/manifest-analyzer/main.go` is `--mode`, `--format`,
`--policy`, `--kubeconfig`, `--context`, with no `--version`.

Their exec'd binary is a **third** pin on the same release, alongside the module and the
image that their build already guards. It is installed by `go install` and recorded
nowhere. A report from one release consumed against a writer from another is a wrong
answer that looks entirely normal.

`--version` alone does not close that — it is a second exec and an assumption that it was
the same binary. **The field in the report is the fix.** The flag is the convenience.

### The shape, and why it is nearly free

The machinery exists. [`cmd/buildinfo.go`](../../cmd/buildinfo.go) holds
ldflags-injected `version`/`gitCommit`/`buildDate` and serves them on `/build-info`; the
`Dockerfile` passes `-X main.version=${VERSION}`.

For the `go install github.com/ConfigButler/gitops-reverser/cmd/manifest-analyzer@vX.Y.Z`
path our own package doc recommends, **ldflags do not apply at all** —
`runtime/debug.ReadBuildInfo()` returns the module version for free, with no build change
and no release-workflow change. Use ldflags when set, fall back to `ReadBuildInfo`, and
emit `"dev"` for a plain `go build`.

```go
type ReportProvenance struct {
    // AnalyzerVersion is the release that produced this report: "v0.39.1", or "dev"
    // for an unreleased build. Informational — do not gate on it.
    AnalyzerVersion string `json:"analyzerVersion,omitempty"`
}
```

Both reports take it. Adding a field does not bump `SchemaVersion`, which is exactly what
our own stability note tells consumers to expect.

### On `SchemaVersion` — the missing half

Our doc states the consumer's duties well: pin a version, ignore unknown fields, do not
switch on prose. It never states what a **bump asserts**. Write that sentence, and answer
these three:

- Does a bump mean fields were removed, that a field's meaning changed, or either?
- Should a reader that knows `v1` hard-fail on `v2`, or attempt a best-effort parse?
- Since adding a field never bumps it, what is left that *does*?

Until that exists every consumer's version handling is a guess, and they will not all
guess alike.

### On the release asset

Their devcontainer pins every tool from a release asset — `task`, `kubectl`, `kustomize`,
`helm`, `k3d`, `flux`, `flux-operator` with `sha256sum -c`. The analyzer is the only one
that cannot be, because we publish no binary; the tool that runs it does `go install` from
source. That is the single unverifiable link in an otherwise pinned toolchain, and it is
the tool deciding which folders they offer a tenant.

They explicitly do **not** want a `sha256sums` file. `v0.39.1` already ships `crds.yaml`,
`install.yaml` and `sbom.spdx.json`, each with a `.intoto.jsonl` provenance attestation
and a `.sigstore.json` bundle ([`.github/workflows/release.yml`](../../.github/workflows/release.yml)).
Adding the analyzer binary to that job makes it verifiable with `gh attestation verify` —
stronger than a checksum, and machinery we already run.

Decision for the implementer: **which platforms.** `linux/amd64` and `linux/arm64` covers
their devcontainer; `darwin/arm64` is the obvious third. Each is another matrix leg on a
job that already signs.

---

## Ask 3 — `ResourceIdentifier.Key()` is a cross-product identity contract

### What they asked for

Document the **string format** of `ResourceIdentifier.Key()` where a consumer can find it,
and put a golden test on the exact strings **in our repo**.

**Explicitly not: export the type.** Worth recording why, because the generous answer is
the wrong one. Importing anything from our module puts a `require` line in *their* go.mod,
and minimal version selection then raises *their* build to whatever that tool pins —
defeating the exact-release pin their build test guards. A mirrored struct plus a format
test is their design, not a stopgap. **A stable documented format is worth more to them
than an importable type, and costs us less.**

### Why — verified

Two products must agree on what "the same resource" is or every join between them is
silently wrong: one keys a row on our identity, the other reports a verdict about it.
Today that agreement is enforced by a byte-for-byte test **on their side**, against a type
in **our** `internal/`. It breaks for them, late, after a version bump they chose, and
never for us.

And they are right that we have nothing.
[`internal/types/identifier_test.go`](../../internal/types/identifier_test.go) covers
`ToGitPath`, `IsClusterScoped` and `String`. **`Key()` has no test at all.** We can change
its format today and every gate stays green.

### Be specific about which `Key()`

There are two, and "the key format" is ambiguous:

```go
// internal/types/identifier.go:40 — the one this ask is about
func (r ResourceIdentifier) Key() string   // "{group}/{version}/{resource}/{namespace}/{name}"
                                           // cluster-scoped: the namespace segment is dropped
                                           // core group: empty, so the key leads with "/v1/secrets/…"

// internal/types/reference.go:40 — not this one
func (r ResourceReference) Key() string    // "namespace/name"
```

The golden test pins all three shapes, because those are the cases a reimplementation gets
wrong:

| case | expected |
|---|---|
| namespaced, grouped | `apps/v1/deployments/prod/api` |
| cluster-scoped, grouped | `rbac.authorization.k8s.io/v1/clusterroles/admin` |
| namespaced, core group | `/v1/secrets/prod/db` |

The four-segment cluster-scoped form is the sharp edge: `Key()` **drops** the namespace
segment rather than emitting an empty one, so a naive reimplementation that always joins
five parts produces `…/clusterroles//admin` and never joins.

### The trap worth naming

`Key()` includes `Version`; `ToGitPath()` deliberately excludes it, for a documented and
correct reason — the operator writes one version per object, so a version segment would
churn the path on a preferred-version bump.

Both are right for their own job. Together they mean **our two identity functions disagree
about whether a preferred-version bump is the same resource.** A cross-product join keyed
on `Key()` splits in two when a CRD's storage version moves, while the Git path correctly
does not move at all.

Decide which of the two **is** the identity and say so where both are defined. The
consumer would rather have a versionless key beside the current one than build their own —
which is the cheap answer if the decision goes that way, and it also removes the last
reason for anyone to reimplement either function.

### Scope

Small, and none of it constrains a pre-1.0 module:

- Godoc on `Key()` naming the format as public, with the three shapes.
- A table-driven golden test on the exact strings, in `internal/types`.
- One line in the package doc of `pkg/manifestanalyzer` pointing at the format, since that
  is where a consumer looks.
- The version-identity decision, recorded beside both methods.

A golden test in our repo turns their late breakage into a red CI run at the moment
someone makes the change, which is where the decision is being taken. It forbids nothing.
It makes the change deliberate.

---

## Why these three belong together

Each is the same failure at a different layer: a distinction our code knows, published as
prose no test defends.

- Ask 1 — the check knows whether a refusal is forever; the type carries a code.
- Ask 2 — the binary knows its version; the document carries a schema marker.
- Ask 3 — `Key()` is an identity contract; the format lives in a comment.

Each fix is a value plus a test that fails when the value stops being true. The cost is a
constant per site. What it buys is the end of a class of bug where we are correct, the
consumer is careful, and the user still gets a wrong sentence.
