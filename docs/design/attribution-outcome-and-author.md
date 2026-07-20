# Make attribution outcome explicit, and name the gap in Git history

> Design, 2026-07-20, **revised after review**. Not built. No CRD/API change.
> Motivated by [debug/attribution-loss.md](../debug/attribution-loss.md).
>
> **The revision is structural, not cosmetic.** The first draft stamped a sentinel author
> string and claimed nothing else was affected. Review showed that string is load-bearing in
> three places that would silently break. The fix is to stop encoding attribution outcome in
> the author string at all: carry it as an **explicit value**, and let the author string be a
> *rendering* of it.

## The problem

When attribution is enabled but no fact matches, the commit is authored by the configured
committer — the same `"GitOps Reverser"` identity used in configured-author mode
([commit.go:188-194](../../internal/git/commit.go#L188-L194),
[types.go:22](../../internal/git/types.go#L22)).

| Mode | Fact matched? | Author today |
|---|---|---|
| configured-author | n/a | `GitOps Reverser` ← correct |
| attribution | yes | the real actor |
| attribution | **no** | `GitOps Reverser` ← **silent, indistinguishable from row 1** |

Rows 1 and 3 are byte-identical in Git history. That ambiguity is why the ~7–10% loss took so
long to diagnose, and why the only way to see it today is a counter nobody reads.

## The core change: an explicit outcome

**Add an attribution outcome to the event, and derive everything from it.** This is the part
the first draft got wrong by trying to infer outcome from `UserInfo.Username`.

~~~go
type AttributionOutcome string

const (
    AttributionNotAttempted AttributionOutcome = ""           // configured-author mode
    AttributionResolved     AttributionOutcome = "resolved"    // a fact named the actor
    AttributionUnresolved   AttributionOutcome = "unresolved"  // attempted, no usable fact
)
~~~

`AttributionNotAttempted` is the **empty string on purpose**, so that it is also the type's zero
value. Most producers of an `Event` never assign the field — reconcile, resync, bootstrap, and
configured-author mode's early return in `attachAuthor` — and all of them mean "no actor was
sought". A name-shaped constant makes the zero value a silent fourth state that equals none of
the three outcomes; the first implementation did exactly that, and it stopped every
CommitRequest attaching in the default deployment (see the matching section below). Nothing
serializes the value — it reaches no CRD field and no metric label — so the empty string is free.

Carried on `git.Event` beside `UserInfo`, and on `PendingWrite`. Three consumers then read the
outcome instead of sniffing a string:

1. **Git author rendering** — `unresolved` renders the sentinel identity (below).
2. **`author_kind` metric** — gets its own value, so a failure never counts as a success.
3. **CommitRequest window matching** — matches on outcome, not on an author string that now
   varies.

This also removes the need to guess "is attribution configured?" from a non-nil resolver,
which the first draft did and which is not a sound proxy.

### Why "unresolved", not "failed"

The first draft said `attribution failed`. That overclaims. `AttributionAbsent` collapses
several genuinely different situations: no fact was ever produced (correct — not every change
has an audited human actor), a cancelled wait, a Redis read error, and a malformed value. All
of them return the same "not found" from `matchFactKey`
([attribution_index.go:404-412](../../internal/queue/attribution_index.go#L404-L412)), which
discards the error deliberately.

So the honest word is **unresolved**: we attempted attribution and did not arrive at an actor.
It still tells a reader something is worth investigating without asserting a fault we cannot
prove. If typed outcomes are added to the index later (distinguishing "no fact existed" from
"lookup errored"), a genuine `failed` state can be split out then — and the outcome enum is
the right place for it.

## The sentinel identity

The sentinel is **derived at the write path** (`commitOptionsFor`) from the carried outcome. It
is never stamped onto an `Event`, so it is scoped to the git author header and reaches nothing
else:

| Field | Surfaces in | Value |
|---|---|---|
| `Username` | machine token inside the sentinel identity; greppable by tooling that parses the header | `attribution-unresolved` |
| `DisplayName` | git author Name via `authorName` | `unknown (attribution unresolved)` |
| `Email` | git author Email via `authorEmail` | `attribution-unresolved@gitops-reverser.invalid` |

~~~text
Author:    unknown (attribution unresolved) <attribution-unresolved@gitops-reverser.invalid>
Committer: GitOps Reverser <noreply@configbutler.ai>
~~~

The operator really did make the commit, so it stays the **committer**; the **author** slot
answers "on whose behalf", and the honest answer is "we do not know". `.invalid` is reserved
(RFC 2606), so the address can never collide with a real one. Parentheses and spaces pass
`isSafeSignatureField`, so the display form is valid in a commit object and a signed payload.

**The sentinel deliberately does NOT reach message bodies or user templates.** An earlier draft
proposed stamping it onto `event.UserInfo`, which would have pushed it into window grouping, the
grouped commit-message body ([commit.go:96](../../internal/git/commit.go#L96)) and per-event
`{{.Username}}` templates ([commit.go:21-33](../../internal/git/commit.go#L21-L33)). Rejected:
the outcome is the fact and the sentinel is one *rendering* of it, so the rendering belongs at
the surface that needs it. Doing otherwise would change the commit text of every existing
deployment that has attribution misses, and force user-authored templates to special-case a
magic token they never had to handle. An unresolved event therefore renders `{{.Username}}`
exactly as configured-author mode always has — empty — while `git log` names the gap. No
release-note change to user-configured output.

## HIGH — CommitRequest attachment must not silently stop matching

A CommitRequest that could not attribute its own author falls back to `Author == ""`
([commitrequest_controller.go:147-151](../../internal/controller/commitrequest_controller.go#L147-L151)),
and attachment matches the open window by **exact author string**
([commit_request_attach.go:106-114](../../internal/git/commit_request_attach.go#L106-L114)):

~~~go
return p.author == w.Author && p.gitTargetName == w.GitTarget && ...
~~~

**Today** an unresolved live window also has `Author == ""`, so the two coincide and the
request attaches. **After a naive sentinel change** the window carries
`attribution-unresolved` while the request still carries `""` — they stop matching, and the
CommitRequest silently lands as a separate commit instead of naming the window it was meant
to. That is a real regression, and the first draft asserting "CommitRequest is unaffected" was
simply wrong.

Because the sentinel is confined to the git author header (above), the window's `Author` stays
`""` and this naive-sentinel regression never arises. The outcome still participates in matching,
but **only via `NamesActor`, and never as enum equality**.

**Decision: match on `(author, NamesActor(outcome), gitTarget, gitTargetNamespace)`.** The two
outcomes being compared are produced by *different, independently configured* subsystems — the
window's by mirrored-resource attribution (`--author-attribution`), the request's by command
authorship (`--admission-webhook`), which [cmd/main.go:311-316](../../cmd/main.go#L311-L316)
documents as independent. Requiring the enums to be equal silently couples the two flags:

| watch attribution | admission webhook | window | CommitRequest | enum equality | `NamesActor` |
|---|---|---|---|---|---|
| off | off | `not_attempted` | `not_attempted` | match | match |
| off | on, miss | `not_attempted` | `unresolved` | **no match** | match |
| on, unresolved | off | `unresolved` | `not_attempted` | **no match** | match |
| on, unresolved | on, miss | `unresolved` | `unresolved` | match | match |

The two middle rows are real deployments that attach correctly today; under enum equality the
user's commit message is dropped into a separate default-message commit with no error anywhere.

The author comparison stays **unconditional** — an additional guard, never a replacement.
Skipping it when neither side names an actor looks equivalent (both authors are `""`, so they
compare equal anyway) but makes cross-author attachment depend on the outcome fields being
right: any path that left an outcome unset while the author was set would let one author's
request finalize another's window. Comparing both costs nothing.

Note this is the opposite call from `openWindow.canAppend`, which *does* compare the enums
directly and should: it compares two events from the same watch pipeline, so they are directly
comparable. The distinction is intra-subsystem versus cross-subsystem.

**Requires a test that sets each side from its real producer**, not two literals: the gap that
let the enum-equality bug ship was that every existing case left both sides at the zero value,
so `"" == ""` passed and the production combination was never exercised.

## HIGH — `author_kind` must not report a failure as a success

`authorKind()` classifies by string
([pending_writes.go:251-260](../../internal/git/pending_writes.go#L251-L260)): empty →
`committer`, `system:serviceaccount:` prefix → `serviceaccount`, otherwise → **`user`**.

A sentinel username is none of those, so it would be classified **`user`** — and
[interpreting-metrics.md](../interpreting-metrics.md) defines `user` as a real named actor and
says "unattributed events use `committer`". **Failed attribution would appear in dashboards as
improved user attribution**, which is worse than the silence it replaces: it would actively
mask the defect, and could show attribution *improving* as it degrades.

**Add `unresolved` as a fourth `author_kind`**, driven by the outcome rather than by string
sniffing. Requires, together, in one change:

- `authorKind()` reads `AttributionOutcome`.
- `docs/interpreting-metrics.md` documents the new value and corrects the "unattributed events
  use `committer`" sentence.
- Any dashboard or alert keying on `author_kind` — a `user`-vs-`committer` ratio panel would
  otherwise misread the new value.
- Tests asserting the classification for all four kinds.

## The resolver contract changes

`ResolveAuthor` documents `ok=false` as **"commit as the configured committer"**
([author_resolver.go:67-82](../../internal/watch/author_resolver.go#L67-L82)). Returning a
placeholder instead silently overturns that contract while leaving the documentation intact —
the exact class of mismatch this whole investigation was caused by.

Change the signature to return the outcome explicitly rather than a bool whose meaning is
documented one way and implemented another:

~~~go
ResolveAuthor(...) (git.UserInfo, AttributionOutcome)
~~~

`attachAuthor` then stamps `UserInfo` and outcome together. A nil resolver yields
`not_attempted`; a resolver that ran and found nothing yields `unresolved`. This is also what
removes the unsound "non-nil resolver means attribution is configured" inference.

## Documentation to update in the same change

The committer-fallback promise is stated in several places; leaving any of them would make the
docs lie in the way this design exists to prevent:

- [architecture.md](../architecture.md) — "The author identity is never invented". A sentinel
  **is** an invented identity, so this must be rewritten to say the author is never
  *fabricated as a person*: an unresolved attribution is labelled unresolved, not attributed
  to someone.
- [configuration.md](../configuration.md) — the `--author-attribution` description.
- [attribution-setup-guide.md](../attribution-setup-guide.md) — the fallback behaviour users
  are told to expect.
- [interpreting-metrics.md](../interpreting-metrics.md) — `author_kind`, as above.
- [UPGRADING.md](../UPGRADING.md) — mandatory release note (below).

## What genuinely does not change

- **Configured-author mode.** No resolver runs, outcome is `not_attempted`, author remains the
  committer.
- **Resolved attribution.** Unchanged in every respect.
- **Window grouping shape.** Unresolved events share one identity and group with each other,
  not with attributed ones — exactly as they do today via `""`. One equivalence class is
  renamed, none split or merged.
- **No CRD or API change.** The outcome is internal; it never appears in a spec or status.

## Cost

1. **Git history changes.** Tooling matching `GitOps Reverser` to mean "the operator wrote
   this" will no longer see unresolved commits. Arguably a fix — those commits were
   mislabelled — but a behaviour change on shipped data.
2. **User templates and message bodies are unchanged.** The sentinel is confined to the git
   author header, so `{{.Username}}` in a custom `EventTemplate` still renders empty for an
   unresolved event, exactly as it does in configured-author mode.
3. **The defect becomes visible with no opt-out.** Adopters will see these commits appear and
   reasonably file bugs. Intended — but it makes the release note **mandatory**, pointing at
   `gitopsreverser_attribution_resolutions_total` and at
   [debug/attribution-loss.md](../debug/attribution-loss.md).

### No flag

The only thing a flag could do is restore silently mislabelled authorship. That is not a
preference to preserve behind an opt-out — it is the bug. Attribution is already gated by
`--author-attribution`: an operator who does not want actor names turns that off and gets
configured-author mode, where the committer genuinely is the author. A second flag would add
an incoherent third state — attribution on, but lie when it fails. Making the placeholder
*string* configurable is likewise rejected: it destroys the one property that matters, that
everyone recognises it.

## Implementation order

1. `AttributionOutcome` type + the three sentinel constants in `internal/git`.
2. `ResolveAuthor` returns the outcome; `attachAuthor` stamps both.
3. `Event` / `PendingWrite` carry the outcome.
4. `authorKind()` reads the outcome; metrics docs updated together.
5. `matchesWindow` matches on `NamesActor`, never on enum equality; the regression test must
   drive each side from its real producer, and cover the `not_attempted`/`unresolved` boundary.
6. Commit author rendering reads the outcome.
7. Documentation sweep (list above) + release note.

Steps 4 and 5 are the ones that must not be deferred: each is a silent behavioural regression
on its own.

## Tests

- **Unit:** all three outcomes → correct author and committer; `author_kind` for all four
  kinds; sentinel passes `isSafeSignatureField` and yields a valid commit object.
- **Unit:** two unresolved events coalesce into one window; an unresolved and a resolved event
  do not.
- **Unit:** the zero value of `AttributionOutcome` **is** `AttributionNotAttempted`. Everything
  below rests on it, and the paths that rely on it never assign the field, so nothing else
  would catch a regression.
- **Regression:** the full cross-subsystem matrix for `matchesWindow`, with each side driven by
  its real producer — the controller's `gitOutcome()` projection and the watch package's actual
  resolver — including the default deployment (webhook off against attribution off) and the
  `not_attempted`/`unresolved` boundary. Two literals set to the same value prove nothing here.
- **Regression:** a request whose author differs from the window's never attaches, *whatever*
  the two outcomes are — so the author check cannot be made conditional on them.
- **Compatibility:** a custom `EventTemplate` rendering `{{.Username}}` still produces an empty
  string for an unresolved event; the sentinel appears only in the git author header.
- **e2e:** author assertions fail as `got unknown (attribution unresolved)`, naming the cause
  in the failure message.

## Relationship to the open defect

This does **not** fix [debug/attribution-loss.md](../debug/attribution-loss.md); it makes it
legible and gives the investigation a labelled instance tied to an object and time rather than
a counter increment. Worth doing first for that reason — and the explicit outcome is what the
eventual fix will be measured against.
