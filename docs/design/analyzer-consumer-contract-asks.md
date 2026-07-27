# Three asks from a downstream consumer of the analyzer

> Status: accepted as work, unscheduled. Written for an implementing party — every
> claim below was checked against the tree at `v0.39.1`, and each ask ends with the
> decisions only a maintainer can make.
> Date: 2026-07-26
> Companion to [config-surface-for-a-structured-repository.md](../future/config-surface-for-a-structured-repository.md),
> which makes the same argument one layer up: the engine learned something and the
> contract did not carry it.

The asks come from **two independent consumer teams**, arriving separately and landing on
the same seams. One links `pkg/manifestanalyzer` as a module and execs `manifest-analyzer
--mode scan-repo --format json` from a second tool that does not link us; it runs the
reverser as an image pinned to the module by two constants with a build-breaking test on
its side. The other verified everything against `v0.39.1` by measurement — running the
binary over a corpus and reading the JSON — and filed four issues.

That convergence is the strongest evidence in this document. Two teams that never spoke to
each other hit the same three seams, and every one is the same complaint: **a fact our doc
comments assert, carried by prose nothing tests, so it drifts and a consumer encodes the
stale version.** One of them shipped a wrong sentence to a real user because of it.

The second team's four issues map onto this document as: their #1 → Ask 1, their #2 → the
`LayoutKustomizeOverlay` fix already made, their #3 → Ask 2, their #4 → Ask 3. Where they
asked for something narrower than what is written here, this document says so.

One thing they were explicit about, so it is recorded before someone "fixes" it: **the
minimal-version-selection diamond is not our bug.** It existed because their tool linked
our module; it stopped linking, and that is the end of it. Do not restructure the module
to solve it.

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
is the **seventeen** internal `IssueKind` constants plus `refused-structural` — and most of
them are neither permanent nor pending. They are **fixable today by the person looking at
the screen**. A folder refused for `invalid-yaml` is one broken document from working, and
its owner was told "not supported yet".

**A second gap surfaced while counting them, and it needs fixing in the same pass.**
`internal/manifestanalyzer` defines seventeen kinds; `pkg/manifestanalyzer` re-declares
**fourteen**. Because `issuesToReasons` copies the internal string through verbatim, three
codes can reach a consumer that has no exported constant to match on:

- `kustomize-render-refused`
- `render-does-not-match-live`
- `unplaceable-edit`

A consumer doing the correct thing — matching on our published constants — silently fails
to recognise all three. Either export them or state that they never reach `ScanRepo`; the
implementer should confirm which by checking whether these write-time kinds can appear in a
structure-only scan. Whatever the answer, the counts must stop disagreeing.

**And the block a consumer reads first is the one that misleads.** The second team measured
a real corpus and got back `unsupported-kustomize` and `non-krm-yaml` as refusal codes, then
went looking for them. `repo.go` has this:

```go
// Refusal reason codes a candidate may carry.
const (
    ReasonOverlayFanOutUnsupported = "overlay-fan-out-unsupported"  // Deprecated: no longer emitted
    ReasonRefusedStructural        = "refused-structural"
)
```

A block headed "Refusal reason codes a candidate may carry" that lists **two of eighteen**,
one of them retired. The codes they measured do exist as `IssueKind` constants — but nothing
says `RefusalReason.Code` draws from `IssueKind`, so a reader who finds this block reasonably
concludes it is the enumeration and treats everything else as unknown. That is a more direct
cause of their wrong sentence than the `Layout` comment was.

Two fixes, both cheap:

- **Type it.** `RefusalReason.Code` becomes `IssueKind` rather than `string`, which makes the
  relationship compile-checked instead of stated. If that is too strong for a JSON-facing
  field, document it: *"Code is an [IssueKind] value, or [ReasonRefusedStructural]."*
- **Fix the block's header** so it says what it holds: reason codes that are *not* issue
  kinds. As written it claims to be the enumeration, and it is not.

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

**Four values, where the second team proposed two** (`permanent | conditional`). Take the
four deliberately: a two-valued enum drops the *not-yet* axis, which is the exact
distinction that went degenerate when `ReasonOverlayFanOutUnsupported` was retired and the
exact one that made their table wrong. Collapsing `pending-upstream` into `conditional`
tells a user to go fix something no user can fix. And `PermanenceUnknown` has to exist as
the zero value regardless, or an unclassified path emits a confident wrong answer instead
of silence. Their `retryable` naming is worth considering for the `fixable` value; the axis
count is the part not to compromise on.

They also note that adding a field is additive and need not move `SchemaVersion`. Correct,
and it stays correct under the KRM envelope recommended in Ask 2 — that envelope is a
separate, deliberate breaking change, and this field should not wait for it.

Ship the actor field. The consumer offered it as optional; it is the cheaper half of the
same constant, and two of our codes are fixable **only** by the platform operator, whom
their wizard does not have on the screen. Without it, `out-of-scope` renders as "fix your
repository" to someone who cannot.

### The classification — our answers

This is the part no consumer can write, so it is written here in full. Derived from each
kind's own doc comment and its raise site.

The fourteen rows below plus the three deferred to the next section account for all
seventeen internal kinds; `refused-structural` is the eighteenth value and is not an
`IssueKind`. Check that arithmetic when adding a kind — it is the only thing keeping this
table honest until the test below exists.

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
- One test per constant is **not sufficient**, and the three decision cases above are why:
  `unsupported-kustomize`, `unresolved-krm` and `kustomize-render-refused` each classify
  differently depending on which branch raised them, so a per-constant test passes while a
  new branch emits `PermanenceUnknown`. Cover each emission path, not each constant. This
  is the one place the ask costs more than a constant per site, and skipping it reproduces
  the original bug one level down.
- Reconcile the internal and public kind sets in the same pass, so the classification test
  covers the codes a consumer can actually receive rather than the subset we export.
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
2. A `manifest-analyzer --version` flag — printing the release version **and the
   `SchemaVersion` it emits**, so one exec answers both questions.
3. One sentence on what a `SchemaVersion` bump asserts, and whether a reader should refuse
   a version it does not know.
4. The analyzer binary attached to the release we already sign.

The second team measured the current flag set to confirm the gap: `manifest-analyzer
--version` returns `flag provided but not defined: -version`, and the full set is
`-context`, `-format`, `-kubeconfig`, `-mode`, `-policy`.

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

A **top-level string on each report**, beside `schemaVersion` — not a nested object, and
not `omitempty`. The point of the ask is that a report always says what produced it, so a
shape that permits omission fails it:

```go
type RepoReport struct {
    SchemaVersion string `json:"schemaVersion"`
    // AnalyzerVersion is the release that produced this report: "v0.39.1", or "dev"
    // for a build that carries no version. Never empty. Informational — a consumer
    // records it to trace an answer back, and does not gate on it.
    AnalyzerVersion string `json:"analyzerVersion"`
    // … unchanged
}
```

The second team proposed a `generator: {name, version}` object instead, on the grounds that
it is "the version of this that survives being piped into another tool". **Prefer their
shape**, and put the tool's name in it: a bare `analyzerVersion` says which release without
saying which *tool*, and a report that has been piped somewhere is exactly where that
ambiguity bites. Under the KRM envelope below it becomes `status.generator: {name, version}`,
where `kind` names the document and `generator.name` names what produced it — two different
facts that a flat string conflates.

`FolderReport` takes the identical field in the same position. The fallback chain is
ldflags → `debug.ReadBuildInfo()` → the literal `"dev"`, so the field is non-empty on every
path including `go run`. A consumer may then treat an absent `analyzerVersion` as "produced
before this shipped" rather than having to distinguish that from "produced by a build that
did not know its own version".

Adding a field does not bump `SchemaVersion` — which is what our own stability note tells
consumers to expect, and a reason to answer the `SchemaVersion` question below in the same
release rather than after it.

### On `SchemaVersion` — the missing half

Our doc states the consumer's duties well: pin a version, ignore unknown fields, do not
switch on prose. It never states what a **bump asserts**. Write that sentence, and answer
these three:

- Does a bump mean fields were removed, that a field's meaning changed, or either?
- Should a reader that knows `v1` hard-fail on `v2`, or attempt a best-effort parse?
- Since adding a field never bumps it, what is left that *does*?

Until that exists every consumer's version handling is a guess, and they will not all
guess alike.

### Recommended: make the report a KRM document, and delete the question

The three questions above are ones the Kubernetes API conventions already answer, in a
document every consumer of a GitOps tool has read. So stop writing our own versioning
policy and adopt theirs — give the report an `apiVersion` and a `kind`:

```yaml
apiVersion: manifestanalyzer.configbutler.ai/v1alpha1
kind: RepoReport
spec:                       # what was asked for
  root: /repo
  mode: scan-repo
status:                     # what was found
  analyzerVersion: v0.39.1
  candidates: [...]
  summary: {...}
```

`apiVersion` replaces `schemaVersion` outright. "What does a bump assert" becomes the
published alpha/beta/GA contract; "should a reader hard-fail on a version it does not know"
becomes yes, by the same rule every Kubernetes client already follows. We answer three open
questions by citing a document instead of writing one.

Four further things fall out, none of which the bespoke shape gives us:

- **The `spec`/`status` split is honest here**, which was the surprise. `spec` is the scan
  request (root, mode, policy) and `status` is the observation. Our own CRDs are built that
  way, so the report reads like the rest of the product. Today's `root` field floats at the
  top level marked "Informational" precisely because there is nowhere for a request to go.
- **YAML becomes the obvious serialization**, which makes `--format yaml` a natural third
  option: diffable, reviewable, and committable with the tooling the user already runs.
- **Existing tooling works** — `yq`, `jq`, `kubectl --dry-run` shape assumptions, and
  apimachinery's `TypeMeta` for anyone who does link us.
- **It is on-message.** A product whose thesis is that cluster state belongs in Git as KRM
  should not emit a bespoke JSON envelope to describe it.

### Two things to carve out, and one cost

**No `metadata`.** A KRM document invites `metadata.name`, and a report has no identity — it
observes a path at an instant. A synthesized name is noise, and worse, it suggests the
document can be applied. Kpt's own `kind: ResourceList` carries `apiVersion`, `kind`, `items`
and `results` with no `metadata`, so the envelope-without-metadata shape has precedent.

**Never served, never registered.** No CRD, no group registration, not applyable. State that
in the type's doc comment, because the shape will make someone try. The related failure is
mild but worth knowing: a report saved into a watched folder is refused either way — today
as `foreign-file`, and as a KRM document as `unresolved-krm`, which reads as "we tried to
manage this and could not" rather than "this is not ours". A marginally worse message, not a
blocker.

**The cost is one breaking change to the JSON contract**, and our only consumer holds golden
fixtures generated from it. That argues for doing it **now** and inside Ask 2 rather than
after: they are asking us for version clarity in this same document, there is exactly one
consumer to coordinate with, and every later release makes it dearer. It does not disturb
their mirrored-struct design — if anything `TypeMeta` is one more thing they can mirror.

**It does not replace Ask 2.** `apiVersion` versions the *contract*; `analyzerVersion`
records the *build*. Items 1, 2 and 4 stand unchanged; only the `SchemaVersion` question
above dissolves.

### On the release asset

Their devcontainer pins every tool from a release asset — `task`, `kubectl`, `kustomize`,
`helm`, `k3d`, `flux`, `flux-operator` with `sha256sum -c`. The analyzer is the only one
that cannot be, because we publish no binary; the tool that runs it does `go install` from
source. That is the single unverifiable link in an otherwise pinned toolchain, and it is
the tool deciding which folders they offer a tenant.

**The two teams differ here, and the resolution is to do both.** The first explicitly does
*not* want a `sha256sums` file, because attestation is stronger. The second asks for
"publishing the binary with checksums, if that is cheap alongside the existing release job"
— its devcontainer verifies every other tool that way, so checksums are the mechanism its
existing tooling already speaks.

`v0.39.1` already ships `crds.yaml`, `install.yaml` and `sbom.spdx.json`, each with a
`.intoto.jsonl` provenance attestation and a `.sigstore.json` bundle
([`.github/workflows/release.yml`](../../.github/workflows/release.yml)). Attach the binary
to that job — verifiable with `gh attestation verify` — **and** emit a `sha256sums` file
beside it. The attestation is the stronger claim; the checksum is the one a `curl | sha256sum
-c` line in a Dockerfile can consume without a GitHub token. Neither team is served by
choosing.

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

The golden test pins **four** shapes — the second team named the fourth, and it is the one
this document first missed. All four were confirmed by running them:

| case | expected |
|---|---|
| namespaced, grouped | `apps/v1/deployments/prod/api` |
| cluster-scoped, grouped | `rbac.authorization.k8s.io/v1/clusterroles/admin` |
| namespaced, core group | `/v1/secrets/prod/db` |
| **cluster-scoped, core group** | `/v1/nodes/node-1` |

The four-segment cluster-scoped form is the sharp edge: `Key()` **drops** the namespace
segment rather than emitting an empty one, so a naive reimplementation that always joins
five parts produces `…/clusterroles//admin` and never joins. The core cluster-scoped case
is the sharpest of the four, since both the group and the namespace are empty — one
degenerates to a leading `/` and the other vanishes, and a reimplementation has to get two
opposite rules right in one string.

Their ask includes wording, and it is worth taking verbatim: the test carries a comment
saying the format is **depended on across product boundaries, so changing it is a breaking
change rather than a refactor**. That sentence is what turns a red test from an obstacle
into a decision point.

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
