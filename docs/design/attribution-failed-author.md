# Name the gap: an explicit author when attribution fails

> Design, 2026-07-20. **Not built.** Small change, no API/CRD impact.
> Motivated by [debug/attribution-loss.md](../debug/attribution-loss.md), which took hours
> longer to diagnose than it should have — precisely because this gap has no name.

## The problem

When author attribution is **enabled** but no audit fact matches, the commit is authored by
the configured committer:

~~~go
author := pendingWrite.AuthorUserInfo()
if author.Username == "" {
    return &git.CommitOptions{Author: committer, Committer: committer, Signer: signer}
}
~~~
[commit.go:188-194](../../internal/git/commit.go#L188-L194)

`committer` is `DefaultCommitterName` = `"GitOps Reverser"`
([types.go:22](../../internal/git/types.go#L22)) — which is **also** the identity every commit
carries in configured-author mode. So one string means two unrelated things:

| Mode | Fact found? | Author today |
|---|---|---|
| configured-author | n/a | `GitOps Reverser` ← correct and expected |
| attribution | yes | the real actor |
| attribution | **no** | `GitOps Reverser` ← **a silent failure, indistinguishable from row 1** |

Rows 1 and 3 are byte-identical in Git history. Nothing in the repository, the commit, or a
`git log` distinguishes "this operator does not do attribution" from "this operator does
attribution and just lost someone's name". That ambiguity is not theoretical: it is why the
first reading of the e2e failure was wrong, and why the only way to see the ~7–10% loss today
is a Prometheus counter nobody looks at.

## The proposal

When attribution is **enabled** and no fact matches, author the commit as an explicit
placeholder instead of the committer. Always — there is no flag (see
[below](#no-flag)).

~~~text
Author:    unknown (attribution failed) <attribution-failed@gitops-reverser.invalid>
Committer: GitOps Reverser <noreply@configbutler.ai>
~~~

Configured-author mode is untouched: it keeps authoring as the committer, because there the
committer genuinely *is* the author.

| Mode | Fact found? | Author after |
|---|---|---|
| configured-author | n/a | `GitOps Reverser` |
| attribution | yes | the real actor |
| attribution | **no** | **`unknown (attribution failed)`** ← now says what happened |

### The three slots, and why each string differs

`UserInfo` feeds three distinct places, which is easy to miss and is why one string will not
do:

| Field | Surfaces in | Value |
|---|---|---|
| `Username` | window grouping key ([open_window.go:57](../../internal/git/open_window.go#L57)), the **grouped commit message body** ([commit.go:96](../../internal/git/commit.go#L96)), and the author-name fallback | `attribution-failed` |
| `DisplayName` | the git author Name, via `authorName` | `unknown (attribution failed)` |
| `Email` | the git author Email, via `authorEmail` | `attribution-failed@gitops-reverser.invalid` |

`Username` lands **inside commit message bodies**, so it has to stand alone there — a bare
`unknown` in a commit message says nothing, whereas `attribution-failed` names the event.
`DisplayName` is what a human reads in `git log`, so it gets the fuller phrasing. The split
mirrors the existing OIDC-display-name-vs-Kubernetes-username pattern rather than inventing a
convention.

### Why this wording

**Say it FAILED, not that it is absent.** `unattributed` (the first draft) is too passive — it
reads as "this operator does not do attribution", which is exactly the confusion with
configured-author mode that the change exists to remove. The reader must be able to tell that
the operator *tried and did not manage it*. That is a defect they should investigate, not a
configuration they chose.

**`unknown` first, mechanism second.** `unknown (attribution failed)` leads with the fact a
human cares about — we do not know who — and follows with why. `unknown-attribution-failed`
as one token is slightly redundant (a failure implies unknown) and reads as a machine
identifier in the one slot that is meant for humans.

**Use Git's author/committer split for what it is for.** The operator really did make the
commit, so it stays the committer. The author slot answers "on whose behalf", and the honest
answer is "we do not know". Overwriting the committer would lose the operator's identity for
no gain.

**`.invalid` is reserved (RFC 2606).** `attribution-failed@gitops-reverser.invalid` can never
collide with a real address, never routes mail, and greps cleanly. `authorEmail` already
synthesises `ConstructSafeEmail(username, "cluster.local")` for users with no email claim, so
a synthetic address is established practice here — but `.invalid` is the stronger signal for a
value that is deliberately not a person.

**Parentheses are safe in a signature.** `isSafeSignatureField` rejects control characters and
the angle brackets that delimit the email; spaces and parens pass, so the display form is
valid in a commit object and in a signed payload.

`git shortlog -sn` then surfaces the loss as its own line with a count — the point being that
it becomes a number a human sees without querying anything.

## What it does not change

**Commit-window grouping is unaffected in shape.** `canAppend` matches on
`e.UserInfo.Username == w.Author` ([open_window.go:57](../../internal/git/open_window.go#L57)).
Today attribution-failed events all carry `Username == ""` and therefore group with each other and
not with attributed ones. With a sentinel they still all share one identity and still group
with each other and not with attributed ones. One equivalence class is renamed; none is split
or merged. This was the interaction most likely to make the change risky, and it does not.

**CommitRequest attribution is unaffected** — it sets a named author explicitly, so it never
reaches the placeholder path.

**No CRD or API change.** The value never appears in a spec or status.

## The cost, stated plainly

This changes what lands in users' Git history. Three real consequences:

1. **Tooling that keys on the author string.** Anyone matching `GitOps Reverser` to mean "the
   operator wrote this" will now miss the commits where attribution failed. That is arguably a *fix* — those
   commits were mislabelled — but it is still a behaviour change on shipped data.
2. **It makes an existing defect visible.** With ~7–10% loss unfixed, adopters will suddenly
   see `unknown (attribution failed)` in their history and reasonably file bugs. That is the
   intent, and with no opt-out flag it is unavoidable — so it MUST be release-noted, with a
   pointer to the metric and to docs/debug/attribution-loss.md, rather than discovered.
3. **Signed commits** carry the author in the signature payload. The placeholder must pass
   `isSafeSignatureField` — it does (ASCII, no angle brackets, no control characters).

### No flag

**Rejected: making this configurable.** An earlier draft proposed
`--author-attribution-placeholder`, defaulting on. Dropped, and the reasoning is worth
keeping: the only thing such a flag can do is restore *silently mislabelled authorship*. That
is not a preference to be preserved behind an opt-out — it is the bug. A knob whose "off"
position reinstates a correctness defect is a knob nobody should be given, and shipping one
would imply the old behaviour is a legitimate choice.

Attribution is already gated by `--author-attribution`. An operator who does not want actor
names in Git turns that off and gets configured-author mode, where the committer genuinely is
the author and no placeholder ever appears. That is the real, coherent choice; a second flag
would only add an incoherent third state — attribution on, but lie about it when it fails.

Also rejected: making the placeholder *string* configurable. It invites every deployment
inventing its own value, which destroys the one property that matters — that everyone
recognises it.

## Implementation sketch

1. `internal/git/types.go` — add the placeholder identity next to the committer constants, so
   all three identities (committer, real actor, failed) are readable in one place:
   `AttributionFailedUsername = "attribution-failed"`,
   `AttributionFailedDisplayName = "unknown (attribution failed)"`,
   `AttributionFailedEmail = "attribution-failed@gitops-reverser.invalid"`.
2. `internal/watch/target_watch.go` — in `attachAuthor`, when the resolver is non-nil (i.e.
   attribution is enabled) and `ResolveAuthor` returns `ok=false`, stamp the placeholder
   `UserInfo` instead of leaving it zero. The resolver stays untouched; it already reports
   `AttributionAbsent`.
3. `internal/git/commit.go` — the `author.Username == ""` branch then means only
   "configured-author mode", which is exactly what it should have meant all along. Tighten
   its comment accordingly.
No flag, so no `cmd/main.go` change.

The placeholder is stamped at the **watch** layer rather than the commit layer on purpose: the
watch layer is the only place that knows attribution was *attempted and failed*, as opposed to
never attempted.

## Tests

- **Unit:** attribution enabled + no fact → author is the placeholder, committer unchanged;
  attribution disabled + no fact → author is the committer (unchanged); fact found → real
  actor (unchanged).
- **Unit:** two attribution-failed events land in one commit window (the equivalence class
  survives the rename); an attribution-failed and an attributed event do not.
- **e2e:** the existing author assertions gain a third expectation. Today a lost attribution
  fails as `expected jane@acme.com, got GitOps Reverser` — ambiguous. After, it fails as
  `got unknown (attribution failed)`, which names the cause in the failure message itself.
- **Unit:** the placeholder passes `isSafeSignatureField` and produces a valid commit object,
  since the display form contains spaces and parentheses.

## Relationship to the open defect

This does **not** fix [attribution-loss.md](../debug/attribution-loss.md); it makes it
legible. Both are worth doing, and this one is worth doing *first*: it turns an invisible
correctness failure into one an operator can see in `git log` without a Prometheus query, and
it removes the ambiguity that made the underlying defect so slow to diagnose. It also gives
that investigation a sharper signal — an `attribution-failed` commit is a labelled instance of the
failure, tied to a specific object and time, rather than a counter increment.
