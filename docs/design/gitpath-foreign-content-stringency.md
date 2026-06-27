# GitTarget path stringency — refuse foreign content, own the subtree

> Status: PROPOSAL — 2026-06-27. Companion to
> [unsupported-folder-refusal-plan.md](unsupported-folder-refusal-plan.md). That doc settled *how* a
> refusal is mechanically wired and surfaced (the `GitPathAccepted` condition + kstatus trio). This doc
> asks the next question the author raised: **the gate is good at refusing the bad YAML it can see — but
> what should it refuse that it cannot currently see at all?** It argues for one new, structural class of
> refusal (foreign / non-managed content), frames it as an explicit decision about what a GitTarget path
> *is* (five roles, with `kustomization.yaml` recognized as an *active* build directive rather than a
> tolerated passenger), and puts the escape hatch in the repo as a `.gitignore`-style **`.gittargetignore`**
> file rather than a CRD field — so the policy stays forward-compatible while the API gains zero new config.

---

## 1. The principle: acceptance is a one-way ratchet

The acceptance contract of a GitTarget path is a **public API**. Two facts about it are asymmetric, and
that asymmetry should drive every call we make here:

- **Refusing content today is reversible.** If we later decide a refused shape is fine, we add an opt-in
  (a new allowlist entry, a new policy mode) and nobody who relied on the old behaviour breaks — they
  could not have had that content in an *accepted* folder, because we refused it.
- **Accepting content today is not reversible.** If we tolerate arbitrary content now and later want a
  feature that *owns* that content — sweeps it, wraps it, re-encrypts it, reorganizes it — we cannot ship
  it without changing the meaning of files users already have sitting in the folder. That is a breaking
  change with a migration, or a feature we simply cannot build.

> **The rule:** when unsure whether to accept a shape, **refuse it.** A refusal is a door we can open
> later; an acceptance is a door that locks behind us.

This is the author's own framing — *"it's easier to give more options than to remove them"* — stated as a
design invariant. The rest of this doc is mostly its consequences.

The concrete motivator is the author's example: *automatically wrapping "other" files into a ConfigMap so
they stay editable.* That feature is **only safe to add later if foreign files are refused today.** If the
operator already tolerates a `notes.txt` in the folder, a future "wrap loose files into a ConfigMap"
behaviour would retroactively claim and materialize a file the user never intended us to manage. Refuse it
now, and the wrap feature becomes a clean, opt-in *widening* — exactly the additive change the ratchet
permits.

---

## 2. What the gate refuses today, and the one thing it cannot see

The gate ([acceptance.go](../../internal/manifestanalyzer/acceptance.go)) is strict and well-tested about
**YAML it parses**. Structure-only refusals (the live + resync path via `AcceptStructureOnly`,
[acceptance.go:191](../../internal/manifestanalyzer/acceptance.go#L191)):

| Refusal | IssueKind |
|---|---|
| Duplicate manifest identity | `IssueDuplicate` |
| Impure managed file (managed file with an empty / non-KRM / invalid passenger) | `IssueImpureManagedFile` |
| Standalone non-KRM / invalid YAML | `IssueNonKRM` / `IssueInvalidYAML` |
| Managed resource hiding in an allowlisted build directive | `IssueMixedFile` |
| `kustomization.yaml` using an unsupported feature | `IssueUnsupportedKustomize` |

**The blind spot: the gate never sees anything that is not YAML.** The acceptance gate is a pure function
of a `ManifestStore`, and the store is built **only from YAML files**:

- [analyzer.go:215](../../internal/manifestanalyzer/analyzer.go#L215) — `buildStoreFS` does
  `yamlFiles, _, scanDiags := collectFiles(fsys)`. The `_` is the **non-YAML file list, discarded.**
- [analyzer.go:312](../../internal/manifestanalyzer/analyzer.go#L312) — the walk classifies any path that
  is not `.yaml`/`.yml` as non-YAML and moves on.
- [analyzer.go:98](../../internal/manifestanalyzer/analyzer.go#L98) — `ClassNonYAML` is documented as
  *"Always ignored."*
- [analyzer.go:299-308](../../internal/manifestanalyzer/analyzer.go#L299-L308) — **symlinks are skipped**
  with an info diagnostic, never refused.

So the following all live in a GitTarget path with the operator reporting `GitPathAccepted=True`:

- arbitrary non-YAML files — `secrets.txt`, `deploy.sh`, `Dockerfile`, `values.json`, a `.png`, a binary
  blob, a tarball;
- a `README.md` — note the operator *writes its own* README at bootstrap, so today its acceptance is an
  **accident of the non-YAML blind spot**, not a policy;
- a symlink, including one escaping the subtree (`link -> /etc/passwd`);
- a git submodule (gitlink) nested in the subtree.

This is the "weird files inside the path" the author flagged. It is also a latent correctness hole: the
encryption guarantee is *"sensitive resources must never touch disk in plaintext"* — but a user-dropped
`db-password.txt` is plaintext we silently tolerate.

---

## 3. The real question: what *is* a GitTarget path?

Refusing foreign content is downstream of one decision we have never written down: **does a GitTarget
own its whole subtree, or does it share the subtree with content it leaves alone?** Three coherent
stances, laid side by side.

| | **A. Shared (status quo)** | **B. Allowlisted-shared** | **C. Exclusive subtree** |
|---|---|---|---|
| Operator owns | its managed KRM only | managed KRM + a named set of tolerated files | the whole subtree |
| Foreign file (`notes.txt`, `deploy.sh`) | accepted, left alone | refused unless its basename is allowlisted | refused unless allowlisted/bootstrap |
| Symlink / gitlink | skipped silently | refused | refused |
| Future "wrap loose files → ConfigMap" | **impossible** without a breaking change | opt-in widening | opt-in widening |
| Future "faithful-mirror sweep of the whole tree" | **impossible** (would delete user files) | possible | possible |
| Plaintext-secret-on-disk hole | open | closed | closed |
| Matches "Git is a *materialized mirror* of API state" | weakly (mirror with passengers) | yes | yes |
| Cost to users today | none | must not drop unlisted files in the folder | same as B |
| Reversibility (the ratchet) | locked toward permissive | open | open |

Stance A is where the code accidentally sits (via the blind spot, not by decision). It is the **only** stance
that forecloses future features, because every future feature that wants to *own* the subtree collides with
content A already promised to leave alone.

### Recommendation: **C, implemented as B's mechanism.**

Declare the subtree **operator-exclusive** (stance C — the mirror is faithful, the operator is the owner),
but make the *only* way for a non-managed file to be acceptable **membership in a small, fixed set of
recognized roles** (stance B's mechanism). Concretely, every filesystem entry under `spec.path` is
classified into exactly one of five roles; the last is **foreign → refused**:

```
ACCEPTED under spec.path
  1. Managed KRM            a YAML document the operator materializes              (already modeled)
  2. Active build directive kustomization.yaml / .yml — READ and ACTED ON          (DefaultAllowlist)
                              (namespace context, resources:); soft only,
                              hard-kustomize features still refused
  3. Operator artifact      .sops.yaml, README.md (basename) +                     (WriterAllowlist + …)
                              <spec.path>/.gittargetignore (ROOT only) —
                              operator-authored, retained, passive
  4. User-ignored           anything matching the root .gittargetignore —          (§4, NEW)
                              NEVER READ
  5. (empty directories)    harmless; git does not track them                      (ignore)

REFUSED under spec.path  (NEW — the foreign class)
  •  any non-YAML file in none of the roles above   (secrets.txt, deploy.sh, blob.bin, Chart.yaml…)
  •  any YAML that is not managed KRM / build directive   (already refused as non-KRM)
  •  a NESTED .gittargetignore (not at the path root) — not honoured, refused as foreign (D2)
  •  any symlink                                    (today: silently skipped)
  •  any gitlink / submodule                        (today: invisible)
```

**Roles 1 and 2 are not the same kind of "accepted," and conflating them is a real modeling error.** A
build directive is not a *tolerated passenger* — it is **load-bearing**: `kustomization.yaml` is parsed and
its `namespace:` / `resources:` change how the operator interprets and places the surrounding documents
([store.go](../../internal/manifestanalyzer/store.go) `resolveNamespaceContext` /
`kustomizeNamespaceAssignments`). So it sits in a distinct role: read and acted upon, never just left
alone, and **deliberately not user-configurable** — how the operator reads a folder is part of the
operator's contract, not a knob. The hard-kustomize refusal already lives here. (This is the author's
observation: "the `kustomization.yml` is not really an ignored file, it's accepted and it's certainly doing
something.")

Why C-via-B rather than pure C or pure B:

- **Pure C ("only managed KRM")** would refuse the operator's *own* `README.md`, `.sops.yaml`, and
  `.gittargetignore`. Wrong. The exclusive subtree still contains operator-authored non-KRM; role 3 names it.
- **Pure B ("shared, but allowlist some basenames")** keeps the *shared* framing, which leaves the door
  open to "well, this other unlisted file is probably fine too." The exclusive *framing* is what makes the
  default unambiguous: **unknown ⇒ refused**, full stop. B is the implementation; C is the contract.

The single most important consequence: **make `README.md` acceptance intentional.** Today it is tolerated
only because non-YAML is invisible. Under this proposal it is tolerated because it is an operator artifact
(role 3). The moment we start *seeing* foreign files, the operator's own README must be explicitly accepted
or we would refuse our own bootstrap output.

---

## 4. The escape hatch is in the repo, not the CRD — `.gittargetignore`

The escape hatch is **not** a CRD field. It is an in-repo `.gitignore`-style file, `.gittargetignore`, that
lives at the GitTarget path root and lists patterns the operator must **never read** — even if they are
`.yaml`/`.yml`. A file matching `.gittargetignore` is dropped at scan time (role 4 in §3): it does not become
managed KRM, it is not classified, and it cannot trigger a foreign-content refusal. It is, from the
operator's point of view, simply not there.

```gitignore
# <spec.path>/.gittargetignore  — same syntax as .gitignore
# Files matching these patterns are NEVER read by the operator, even if they are YAML.
docs/                 # a hand-maintained docs subtree the operator should leave alone
*.md                  # loose markdown notes
legacy/old-config.yaml  # a YAML file kept in the repo but not operator-managed
```

**Why this is better than a CRD `pathPolicy` mode enum — and it is what shrinks the config surface:**

- **Zero CRD surface.** No new `spec` field, no enum, no per-target `allow` list to validate, default, and
  document. The author's goal — "keep config surface as small as possible" — is served by adding *nothing*
  to the API. The widening surface moves from the cluster object into the repo, where the content lives.
- **GitOps-native and versioned.** The ignore rules are committed next to the content they govern, reviewed
  in the same PR, and travel with the branch. A reverse-GitOps tool keeping its *own* policy in Git is the
  consistent story.
- **Surgical, not blanket.** A `foreignContent: Ignore` mode would have said "tolerate *all* foreign
  content" — exactly the permissive stance A we are trying to leave. `.gittargetignore` instead names
  *specific* intentional passengers, pattern by pattern. The default stays strict for everything not named;
  toleration is always explicit and auditable. This is strictly safer than a mode toggle.
- **Familiar.** `.gitignore` semantics are universally understood; users need no new mental model.
- **It still obeys the ratchet.** Shipping `.gittargetignore` is purely additive — a folder with no
  `.gittargetignore` behaves exactly as strict §3 describes. And it does not foreclose a future
  `WrapAsConfigMap` materialization mode, which, when designed, brings its own narrow field (D-foreign-5).

So the behaviour is **always Strict**; `.gittargetignore` is the only escape, and it lives in the repo. There
is no `pathPolicy` enum in v1.

### 4.1 `.gittargetignore` semantics (v1) — positional, exactly one honoured file

The honoured ignore file lives at **exactly one path: `<spec.path>/.gittargetignore`** — the GitTarget
path root. This is deliberately *positional*, not a basename rule, and it has three consequences (D2):

- **A `.gittargetignore` above the path (e.g. at the repo root) is never seen.** The operator only ever
  scans `<spec.path>` and below, so a file outside that subtree is simply outside its world — irrelevant,
  not consulted.
- **A `.gittargetignore` deeper in the subtree is NOT honoured — and is refused.** Nested ignore files are
  not a feature in v1. Crucially, a nested `.gittargetignore` is not silently tolerated either: it is just
  another file under the exclusive subtree, so it falls through to the **foreign** role and is **refused**
  (`IssueForeignFile`) unless the *root* `.gittargetignore` ignores it. This is intentional — a misplaced
  ignore file is almost always a user error ("I thought nesting worked"), and refusing it loudly with a
  named path is far better than honouring nothing while the user believes their nested rules took effect.
  If a user genuinely wants to keep a nested `.gittargetignore` around as inert content, they add a pattern
  for it (e.g. `**/.gittargetignore`) to the root file — explicit, like everything else.
- **The root `.gittargetignore` is its own special case.** The one file at `<spec.path>/.gittargetignore`
  is recognized at that exact path: its *contents* are read to build the matcher, and it is itself never
  materialized and never refused. It is the only `.gittargetignore` the operator treats as an ignore file.

Other rules:

- **Exact `.gitignore` syntax** — comments (`#`), blanks, globs, directory matches (`foo/`), `**`, and
  negation (`!`). We reuse the parser, so behaviour matches git precisely (see §5).
- **Patterns may match nested paths** (`docs/`, `**/scratch/`), so the single root file still covers the
  whole subtree — depth-of-*match* is unlimited even though there is only one ignore *file*.
- **Precedence (highest first):** operator artifacts (`README.md`, `.sops.yaml`, basename-matched) and the
  root build directives (role 2) → the root `.gittargetignore` filter (role 4) → managed KRM (role 1) /
  foreign (refused). The operator's own artifacts and the hard-kustomize refusal are matched *before* the
  ignore filter, so a user cannot use `.gittargetignore` to hide the operator's own `.sops.yaml`/`README.md`
  or to silence a hard-kustomize refusal — those are the operator's contract, not user-suppressible.

### 4.2 Bootstrap stages a commented `.gittargetignore`

The bootstrap template ([bootstrapped_repo_template.go](../../internal/git/bootstrapped_repo_template.go))
already stages `README.md` and, when encryption is configured, `.sops.yaml`. Add `.gittargetignore` to that
set, written **fully commented** — every example pattern behind `#` — so a freshly bootstrapped target
ignores nothing until a human deliberately edits it. That gives users a discoverable, in-place template
(with the syntax reminder) without changing behaviour on day one.

---

## 5. Where this fits the existing code

This is a **structural** refusal — it depends only on the bytes/entries in the folder, never on cluster
discovery or followability. That places it in exactly the same safe class as the existing Tier-1 refusals,
so it inherits their guarantees and their seam:

- It runs in `AcceptStructureOnly` ([acceptance.go:191](../../internal/manifestanalyzer/acceptance.go#L191)),
  so it gates **both** the resync apply and the live write path, and it **cannot false-refuse on a
  discovery wobble** (the property the unsupported-folder plan guards so carefully). A foreign file is a
  fact about the disk, not about the API.
- On refusal it produces an `AcceptanceIssue` that names the offending path, flows out as the existing
  `AcceptanceRefusedError`
  ([acceptance_refusal.go](../../internal/manifestanalyzer/acceptance_refusal.go)), and surfaces as
  `GitPathAccepted=False` / `Stalled=True` with the file name in the message — **no new surface, no new
  status plumbing.** It reuses everything `unsupported-folder-refusal-plan.md` already built.

There are two real changes, both upstream of `Accept`, both in `collectFiles`
([analyzer.go:281](../../internal/manifestanalyzer/analyzer.go#L281)) — the single walk that already
classifies every entry:

1. **Apply the root `.gittargetignore` first (the filter).** Read **exactly** `<spec.path>/.gittargetignore`
   (the walk root — never a nested or repo-root copy) at the start of the walk, parse it into a matcher, and
   **skip any entry it matches** — before the YAML/non-YAML split, so ignored files never reach the store,
   the gate, the planner, or the writer. This is the "never read" semantic. A `.gittargetignore` found at any
   *other* depth is not consulted; it is left in the walk and therefore lands in the foreign set below
   (refused, per D2) unless the root file ignores it.
2. **Stop discarding foreign entries (the new view).** Today they are dropped at
   [analyzer.go:215](../../internal/manifestanalyzer/analyzer.go#L215) (the `_`). The store (or a sibling
   input to `AcceptStructureOnly`) must begin carrying the surviving:
   - **non-YAML file list** `collectFiles` already computes and throws away;
   - **skipped-symlink set** (today only an info diagnostic, [analyzer.go:299](../../internal/manifestanalyzer/analyzer.go#L299));
   - **gitlink** entries (submodules), if the walk can observe them.

Then a new `foreignContentRefusals(store)` issues one refusal per foreign entry that survived the ignore
filter and matched none of roles 1–3. Proposed `IssueKind`s, alongside the existing ones:

```go
IssueForeignFile      IssueKind = "foreign-file"      // non-YAML regular file in no recognized role
IssueForeignSymlink   IssueKind = "foreign-symlink"   // any symlink under the subtree
IssueForeignSubmodule IssueKind = "foreign-submodule" // gitlink under the subtree
```

(`IssueNonKRM` already covers foreign *YAML*; this adds the non-YAML / symlink / submodule cases.)

**`.gittargetignore` is cheap to implement — no new dependency.** `github.com/go-git/go-git/v5` is already a
direct dependency used throughout `internal/git`, and it ships
[`plumbing/format/gitignore`](https://pkg.go.dev/github.com/go-git/go-git/v5/plumbing/format/gitignore):
`ParsePattern(line, domain)` → `Pattern`, `NewMatcher([]Pattern)` → `Matcher`, and
`Matcher.Match(path []string, isDir bool) bool`. The implementation is: read the file, parse non-comment
lines, build a matcher once per scan, and call `Match` on each walked path. We reuse git's own matching
semantics rather than reinventing glob handling.

---

## 6. Edge cases and where the line sits

- **Nested directories are normal, not foreign.** Canonical placement is
  `{group}/{version}/{resource}/{ns}/{name}.yaml`, so depth is expected. The refusal is about foreign
  *content*, never about subdirectories. A nested `kustomization.yaml` is allowlisted; a nested
  `deploy.sh` is foreign.
- **Empty directories:** ignore. Git does not track them; refusing them buys nothing and annoys users.
- **The operator's own bootstrap output must self-accept.** `README.md`, `.sops.yaml`, and `.gittargetignore`
  must be in the effective allowlist or we refuse our own writes. `.sops.yaml` already is
  ([acceptance.go:149](../../internal/manifestanalyzer/acceptance.go#L149) `WriterAllowlist`); `README.md`
  and `.gittargetignore` must be added. Audit the full bootstrap template
  ([bootstrapped_repo_template.go](../../internal/git/bootstrapped_repo_template.go)) for any other
  non-KRM file it stages and allowlist each one explicitly.
- **Keep the hardcoded accepted set minimal; push "common benign" files into the bootstrapped
  `.gittargetignore`.** Rather than hardcoding `LICENSE` / `.gitignore` / `.gitattributes` into the operator's
  allowlist (they are *user* content, not operator artifacts, so they would muddy role 3), the bootstrapped
  `.gittargetignore` (§4.2) ships them as **commented example patterns**. A user who adds a `LICENSE`
  uncomments one line — an in-repo, discoverable, versioned fix — instead of relying on operator-baked API
  behaviour. The hardcoded set stays exactly the operator's own artifacts + build directives. (Supersedes
  the old "starter allowlist" idea; see D-foreign-3.)
- **Symlinks: refuse, do not skip.** A writer that materializes into a folder containing a symlink can
  follow it out of the subtree; silently skipping it hides a real hazard. A user who genuinely wants a
  symlink left alone can `.gittargetignore` it — explicit and versioned — rather than us tolerating it
  implicitly.
- **Gitlinks / submodules: refuse.** A submodule in the managed subtree is content the operator cannot
  own or reason about. `.gittargetignore` is again the explicit escape.
- **`.gittargetignore` must not shadow managed-resource paths.** It is for *foreign* passengers. If a user
  ignores a path the operator materializes into (under the canonical
  `{group}/{version}/{resource}/{ns}/{name}.yaml` placement), the operator stops seeing its own file and
  may re-create or churn it. v1 documents this as unsupported; a future guard could warn when an ignore
  pattern overlaps a managed write path. Likewise, ignoring an active `kustomization.yaml` changes namespace
  resolution — allowed, but a semantic change the user owns.
- **Case-insensitive checkouts.** Two files differing only in case can collide on macOS/Windows working
  trees. Out of scope for the foreign-content rule (identity duplicates are already refused), but worth a
  one-line note in the user docs.

---

## 7. Migration and risk

- **Blast radius — this refuses folders accepted yesterday.** Any GitTarget path with a stray `LICENSE` a
  user added, a hand-written `deploy.sh`, or loose notes will flip to `Stalled=True` on the next resync.
  This is the *intended* tightening, but it is real. Mitigations: (a) ship it on `v1alpha2` while the API is
  still pre-stable and the author is "updating wildly" — now is the cheapest this change will ever be;
  (b) the refusal message already names the offending file, so the fix is obvious — `git rm` it, or add one
  line to `.gittargetignore`; (c) the bootstrapped `.gittargetignore` ships the common-benign patterns as
  commented examples, so the fix is discoverable in-place.
- **The escape hatch is `.gittargetignore`, and it is explicit by construction.** Users who want to keep a
  foreign file name it in the root `.gittargetignore` — versioned, reviewed, and scoped to that pattern.
  There is **no** blanket "tolerate everything" mode in v1 (D-foreign-4: keep it simple); the strict default
  is never a dead end, but every exception is spelled out in the repo.
- **`.gittargetignore` footgun: shadowing managed paths (§6).** Ignoring a path the operator materializes into
  causes churn. Document loudly; consider a future overlap guard. This is the one place `.gittargetignore` can
  bite, and it is a misuse, not the intended use.
- **Do not over-reach into discovery-derived refusals.** This proposal is strictly structural. The
  mapping-aware refusals (unwatched / out-of-scope KRM) stay deferred for the same discovery-blink reason
  the unsupported-folder plan already records — do not let "be more stringent" pull them onto the live
  path.
- **Keep the diagnostic.** As with every refusal, the foreign file's path must survive into the
  `GitPathAccepted` message **and** a printer column; a bare count is useless for fixing the folder.

---

## 8. Decisions

### Settled (recommended, consistent with the ratchet)

1. **Foreign content is a refusal.** Non-YAML files, symlinks, and gitlinks under `spec.path` in no
   recognized role are refused, not ignored. Structural ⇒ runs in `AcceptStructureOnly`, gates live +
   resync, never false-refuses on discovery.
2. **The path is an exclusive subtree, modeled as five roles (§3).** Accepted = managed KRM ∪ active build
   directives ∪ operator artifacts ∪ `.gittargetignore`-ignored; everything else foreign. Build directives are
   a distinct *active* role (read and acted on), not a tolerated passenger.
3. **The escape hatch is `.gittargetignore`, in the repo — no CRD `pathPolicy` field in v1.** Behaviour is
   always Strict; `.gittargetignore` (`.gitignore` syntax, root-only, never-read semantics) is the only
   exception surface. This *shrinks* config surface to zero new API fields.
4. **`README.md` and `.gittargetignore` acceptance becomes intentional.** Add both (and any other bootstrap
   non-KRM) to the operator allowlist, since the gate will now *see* non-YAML. Bootstrap stages a
   fully-commented `.gittargetignore`.

### Decided this round (2026-06-27)

- **D-foreign-1 — strict-by-default — DECIDED.** Behaviour is always Strict; there is no mode field.
  Foreign content under `spec.path` is refused unless the root `.gittargetignore` ignores it. The tightening
  is on by default; blast radius is mitigated by the bootstrapped `.gittargetignore` examples + named
  diagnostics.
- **D-foreign-2 — `.gittargetignore` is positional, exactly one honoured file — DECIDED.** Honoured at
  **only** `<spec.path>/.gittargetignore`. A copy at the repo root (above the path) is never seen ("ignored"
  — outside the operator's scope). A copy deeper in the subtree is **not** honoured **and is refused as
  foreign** (`IssueForeignFile`) unless the root file ignores it — a misplaced ignore file is a likely user
  error and must surface, not silently do nothing. `.gitignore` syntax via go-git's `gitignore` package
  (already vendored); never-read semantics. (§4.1.)
- **D-foreign-3 — add `.gittargetignore` to the bootstrap files — DECIDED.** Bootstrap stages a
  fully-commented `.gittargetignore` (example patterns + syntax reminder, all behind `#`, a no-op until
  edited) right alongside `README.md` / `.sops.yaml`. Nice and clear, discoverable in-place.
- **D-foreign-4 — keep it simple: no blanket mode — DECIDED.** `.gittargetignore` is the whole escape-hatch
  story for v1. We do **not** add a `foreignContent: Ignore` (or any other) mode now — no extra surface, no
  extra states. If a real need ever appears the ratchet still permits adding one later, but it is explicitly
  out of scope here.
- **D-foreign-5 — `WrapAsConfigMap` deferred — NOTED for later.** Out of scope for this doc. The only thing
  this design owes it is to *keep the route open*, which refusing foreign content now does: a future
  opt-in could wrap matched foreign files into a ConfigMap without a breaking change. It gets its own design
  (and its own narrow field) when the time comes.

---

## 9. One-paragraph summary

The gate is strict about the YAML it can see and blind to everything else, so a GitTarget path silently
accepts arbitrary non-YAML files and symlinks today. Because acceptance is a one-way ratchet — refusing is
reversible, accepting is not — the future-proof move is to declare the path an **operator-exclusive
subtree** of five roles (managed KRM, active build directives, operator artifacts, `.gittargetignore`-ignored,
foreign), and refuse the foreign role **now** as a purely structural check (reusing the existing
`AcceptStructureOnly` seam and `GitPathAccepted` surface). The escape hatch is **not** a CRD field but an
in-repo `.gittargetignore` (`.gitignore` syntax, never-read semantics), so the widening surface lives in the
repo and the API gains *zero* new config. That keeps every future option open — including the author's
auto-ConfigMap idea — at the cost of a single new structural refusal and a ~50-line `.gittargetignore` filter
built on the go-git parser the project already vendors.
