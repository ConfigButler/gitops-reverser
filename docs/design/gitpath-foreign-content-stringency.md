# GitTarget path stringency — refuse foreign content, own the subtree

> Status: PROPOSAL — 2026-06-27. Companion to
> [unsupported-folder-refusal-plan.md](unsupported-folder-refusal-plan.md). That doc settled *how* a
> refusal is mechanically wired and surfaced (the `GitPathAccepted` condition + kstatus trio). This doc
> asks the next question the author raised: **the gate is good at refusing the bad YAML it can see — but
> what should it refuse that it cannot currently see at all?** It argues for one new, structural class of
> refusal (foreign / non-managed content), frames it as an explicit decision about what a GitTarget path
> *is*, and designs the policy so the decision stays forward-compatible.

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
but make the *only* way for a non-managed file to be acceptable **membership in a small, explicit,
extensible allowlist** (stance B's mechanism). Concretely, every filesystem entry under `spec.path` is
accepted iff it is one of exactly four things; anything else is **foreign → refused**:

```
ACCEPTED under spec.path
  1. Managed KRM            a YAML document the operator materializes              (already modeled)
  2. Allowlisted directive  kustomization.yaml / .yml (soft only)                  (DefaultAllowlist)
  3. Bootstrap artifact     .sops.yaml, README.md — operator-authored, retained    (WriterAllowlist + README)
  4. (empty directories)    harmless; git does not track them                      (ignore)

REFUSED under spec.path  (NEW — the foreign class)
  •  any non-YAML file not in the allowlist        (secrets.txt, deploy.sh, blob.bin, Chart.yaml…)
  •  any YAML that is not managed KRM and not an allowlisted directive   (already refused as non-KRM)
  •  any symlink                                   (today: silently skipped)
  •  any gitlink / submodule                       (today: invisible)
```

Why C-via-B rather than pure C or pure B:

- **Pure C ("only managed KRM")** would refuse the operator's *own* `README.md` and `.sops.yaml`. Wrong.
  The exclusive subtree still contains operator-authored non-KRM; the allowlist is how we name it.
- **Pure B ("shared, but allowlist some basenames")** keeps the *shared* framing, which leaves the door
  open to "well, this other unlisted file is probably fine too." The exclusive *framing* is what makes the
  default unambiguous: **unknown ⇒ refused**, full stop. B is the implementation; C is the contract.

The single most important consequence: **make `README.md` acceptance intentional.** Today it is tolerated
only because non-YAML is invisible. Under this proposal it is tolerated because it is an allowlist entry.
The moment we start *seeing* foreign files, the operator's own README must be explicitly allowed or we
would refuse our own bootstrap output.

---

## 4. The escape hatch — designed so we never have to narrow

The ratchet only stays in our favour if widening is always additive. Encode the policy as a **mode enum**,
defaulting to the strict end, so every future option is a non-breaking addition:

```yaml
# GitTarget.spec (proposed; field name open — see D-foreign-2)
spec:
  pathPolicy:
    foreignContent: Strict        # DEFAULT. Refuse anything not in the accepted set (§3).
    # future, additive, non-breaking modes:
    #   Ignore         leave foreign content alone (today's accidental behaviour, now an explicit opt-in)
    #   WrapAsConfigMap  materialize matched foreign files as ConfigMap data — the author's idea
    # allow:                       # future: per-target allowlist additions, narrow and explicit
    #   - "*.md"
```

Because the **default is `Strict`**, adding `Ignore` or `WrapAsConfigMap` later cannot break anyone: a
folder that worked under `Strict` still works, and the new modes only ever *accept more*. This is the
literal mechanism behind "easier to add options than remove them" — we ship the removal (refuse foreign)
first, while it is still free to ship, and keep every addition for later.

Two design rules for the hatch:

1. **The allowlist is the one widening surface.** Build-directive basenames, bootstrap artifacts, and any
   future per-target `allow` globs all flow through the existing `Allowlist`
   ([acceptance.go:118](../../internal/manifestanalyzer/acceptance.go#L118)). One concept, one place to
   reason about "what non-managed content is tolerated."
2. **Never an implicit widening.** No "we'll just ignore files under a `docs/` subdir" special cases.
   Every tolerated foreign file is named (basename or glob) and visible in the policy. Implicit tolerance
   is how stance A leaked in the first place.

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

The one real change is upstream of `Accept`: the gate must **start seeing foreign entries**. Today they are
dropped at [analyzer.go:215](../../internal/manifestanalyzer/analyzer.go#L215). The store (or a sibling
input to `AcceptStructureOnly`) must begin carrying:

- the **non-YAML file list** that `collectFiles` already computes and currently throws away (the `_`);
- the **skipped-symlink set** (today only an info diagnostic);
- **gitlink** entries (submodules), if the walk can observe them.

Then a new `foreignContentRefusals(store, allowlist)` issues one refusal per foreign entry not covered by
the allowlist. Proposed `IssueKind`s, alongside the existing ones:

```go
IssueForeignFile     IssueKind = "foreign-file"      // non-YAML, non-allowlisted regular file
IssueForeignSymlink  IssueKind = "foreign-symlink"   // any symlink under the subtree
IssueForeignSubmodule IssueKind = "foreign-submodule" // gitlink under the subtree
```

(`IssueNonKRM` already covers foreign *YAML*; this adds the non-YAML / symlink / submodule cases.)

---

## 6. Edge cases and where the line sits

- **Nested directories are normal, not foreign.** Canonical placement is
  `{group}/{version}/{resource}/{ns}/{name}.yaml`, so depth is expected. The refusal is about foreign
  *content*, never about subdirectories. A nested `kustomization.yaml` is allowlisted; a nested
  `deploy.sh` is foreign.
- **Empty directories:** ignore. Git does not track them; refusing them buys nothing and annoys users.
- **The operator's own bootstrap output must self-accept.** `README.md` and `.sops.yaml` must be in the
  effective allowlist or we refuse our own writes. `.sops.yaml` already is
  ([acceptance.go:149](../../internal/manifestanalyzer/acceptance.go#L149) `WriterAllowlist`); `README.md`
  must be added. Audit the full bootstrap template
  ([bootstrapped_repo_template.go](../../internal/git/bootstrapped_repo_template.go)) for any other
  non-KRM file it stages and allowlist each one explicitly.
- **Symlinks: refuse, do not skip.** A writer that materializes into a folder containing a symlink can
  follow it out of the subtree; silently skipping it hides a real hazard. If a legitimate symlink use case
  appears, it becomes an opt-in mode — the ratchet again.
- **Gitlinks / submodules: refuse.** A submodule in the managed subtree is content the operator cannot
  own or reason about.
- **Case-insensitive checkouts.** Two files differing only in case can collide on macOS/Windows working
  trees. Out of scope for the foreign-content rule (identity duplicates are already refused), but worth a
  one-line note in the user docs.
- **A "common benign" starter allowlist.** To keep `Strict` from being needlessly hostile on day one,
  seed the allowlist with the genuinely-harmless, near-universal repo files and let users extend it:
  `README.md`, `LICENSE`, `.gitignore`, `.gitattributes`, plus the existing `kustomization.yaml` /
  `.sops.yaml`. Everything else is foreign until named. (Decision D-foreign-3.)

---

## 7. Migration and risk

- **Blast radius — this refuses folders accepted yesterday.** Any GitTarget path with a stray `README.md`
  a user added, a hand-written `.gitignore`, or a loose script will flip to `Stalled=True` on the next
  resync. This is the *intended* tightening, but it is real. Mitigations: (a) ship it on `v1alpha2` while
  the API is still pre-stable and the author is "updating wildly" — now is the cheapest this change will
  ever be; (b) the "common benign" starter allowlist (§6) absorbs the most frequent false alarms; (c) the
  refusal message already names the offending file, so the fix is obvious (`git rm` it, or wait for the
  `Ignore` opt-in).
- **An explicit opt-out exists by construction.** Users who genuinely want a shared folder get
  `foreignContent: Ignore` once that mode lands — so the strict default is not a dead end, it is the
  *safe* default with a documented escape. (Whether to ship `Ignore` in the same change or defer it:
  D-foreign-4.)
- **Do not over-reach into discovery-derived refusals.** This proposal is strictly structural. The
  mapping-aware refusals (unwatched / out-of-scope KRM) stay deferred for the same discovery-blink reason
  the unsupported-folder plan already records — do not let "be more stringent" pull them onto the live
  path.
- **Keep the diagnostic.** As with every refusal, the foreign file's path must survive into the
  `GitPathAccepted` message **and** a printer column; a bare count is useless for fixing the folder.

---

## 8. Decisions

### Settled (recommended, consistent with the ratchet)

1. **Foreign content is a refusal.** Non-YAML files, symlinks, and gitlinks under `spec.path` that are not
   allowlisted are refused, not ignored. Structural ⇒ runs in `AcceptStructureOnly`, gates live + resync,
   never false-refuses on discovery.
2. **The path is an exclusive subtree, expressed via the allowlist.** Accepted = managed KRM ∪ allowlisted
   directives ∪ bootstrap artifacts; everything else foreign. The allowlist is the single widening surface.
3. **`README.md` acceptance becomes intentional.** Add it (and any other bootstrap non-KRM) to the
   effective allowlist, since the gate will now *see* non-YAML.

### Open — for the team

- **D-foreign-1 — default mode.** Recommend `foreignContent: Strict` as the default. Confirm we want the
  tightening on by default rather than opt-in (the ratchet argues yes; the blast radius argues for a
  starter allowlist, not a looser default).
- **D-foreign-2 — policy shape & placement.** `GitTarget.spec.pathPolicy.foreignContent` enum vs. a
  `GitProvider`-level default with per-target override vs. reusing/extending the `Allowlist` type only.
  Recommend a `GitTarget`-level enum now (smallest additive surface), provider-level default later if
  needed.
- **D-foreign-3 — the starter allowlist contents.** `README.md`, `LICENSE`, `.gitignore`,
  `.gitattributes`, `kustomization.yaml`, `kustomization.yml`, `.sops.yaml`. Add/remove?
- **D-foreign-4 — ship `Ignore` now or defer.** Shipping only `Strict` first is the purest ratchet (refuse
  first, widen later). Shipping `Ignore` alongside gives users an immediate escape. Recommend: ship
  `Strict` + `Ignore` together (the escape hatch is cheap and de-risks the migration), defer
  `WrapAsConfigMap` to its own design.
- **D-foreign-5 — `WrapAsConfigMap` is its own doc.** The author's ConfigMap idea is a materialization
  feature with real questions (size limits, binary handling, name derivation, key collisions, round-trip
  editability). This doc only guarantees it *stays possible* by refusing foreign content first. Track it
  separately.

---

## 9. One-paragraph summary

The gate is strict about the YAML it can see and blind to everything else, so a GitTarget path silently
accepts arbitrary non-YAML files and symlinks today. Because acceptance is a one-way ratchet — refusing is
reversible, accepting is not — the future-proof move is to declare the path an **operator-exclusive
subtree**, refuse foreign content **now** as a purely structural check (reusing the existing
`AcceptStructureOnly` seam and `GitPathAccepted` surface), and expose widening only through an explicit,
default-`Strict` policy mode. That keeps every future option open — including the author's auto-ConfigMap
idea — while costing nothing but a starter allowlist and an `Ignore` escape hatch today.
