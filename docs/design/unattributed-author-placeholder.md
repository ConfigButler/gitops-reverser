# Name the gap: an explicit author for unattributed commits

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
placeholder instead of the committer.

~~~text
Author:    unattributed <unattributed@gitops-reverser.invalid>
Committer: GitOps Reverser <noreply@configbutler.ai>
~~~

Configured-author mode is untouched: it keeps authoring as the committer, because there the
committer genuinely *is* the author.

| Mode | Fact found? | Author after |
|---|---|---|
| configured-author | n/a | `GitOps Reverser` |
| attribution | yes | the real actor |
| attribution | **no** | **`unattributed`** ← now says what happened |

### Why this shape

**Use Git's author/committer split for what it is for.** The operator really did make the
commit, so it stays the committer. The author slot answers "on whose behalf", and the honest
answer is "we do not know". Overwriting the committer would lose the operator's identity for
no gain.

**`.invalid` is reserved (RFC 2606).** `unattributed@gitops-reverser.invalid` can never
collide with a real address, never routes mail, and is recognisable at a glance. Note
`authorEmail` already synthesises `ConstructSafeEmail(username, "cluster.local")` for users
with no email claim, so a synthetic address is established practice here — but `.invalid` is
the stronger signal for a value that is deliberately not a person.

**Lowercase, no display name.** Real actors get an OIDC display name; the placeholder should
not look like one. `unattributed` sorts and greps distinctly, and `git shortlog -sn` surfaces
it as its own line — which is the point: the loss becomes a number a human sees without
querying anything.

## What it does not change

**Commit-window grouping is unaffected in shape.** `canAppend` matches on
`e.UserInfo.Username == w.Author` ([open_window.go:57](../../internal/git/open_window.go#L57)).
Today unattributed events all carry `Username == ""` and therefore group with each other and
not with attributed ones. With a sentinel they still all share one identity and still group
with each other and not with attributed ones. One equivalence class is renamed; none is split
or merged. This was the interaction most likely to make the change risky, and it does not.

**CommitRequest attribution is unaffected** — it sets a named author explicitly, so it never
reaches the placeholder path.

**No CRD or API change.** The value never appears in a spec or status.

## The cost, stated plainly

This changes what lands in users' Git history. Three real consequences:

1. **Tooling that keys on the author string.** Anyone matching `GitOps Reverser` to mean "the
   operator wrote this" will now miss unattributed commits. That is arguably a *fix* — those
   commits were mislabelled — but it is still a behaviour change on shipped data.
2. **It makes an existing defect visible.** With ~7–10% loss unfixed, adopters will suddenly
   see `unattributed` in their history and reasonably file bugs. That is the intent, and it
   should be release-noted rather than discovered.
3. **Signed commits** carry the author in the signature payload. The placeholder must pass
   `isSafeSignatureField` — it does (ASCII, no angle brackets, no control characters).

### Should it be configurable?

**Recommendation: yes, one boolean, defaulting to the new behaviour.**

~~~text
--author-attribution-placeholder=true   # default: author unattributed commits as `unattributed`
--author-attribution-placeholder=false  # legacy: author them as the committer
~~~

Default-on because silently mislabelling authorship is the bug being fixed, and a flag that
defaults to the broken behaviour fixes nothing. The escape hatch exists for an operator whose
downstream tooling cannot absorb the change mid-stream; it is a migration aid, not a
long-term choice, and should say so in its flag help.

Rejected alternative: make the placeholder *string* configurable. It invites every deployment
inventing its own value, which destroys the one property that matters — that everyone
recognises it.

## Implementation sketch

1. `internal/git/types.go` — add `UnattributedAuthorName = "unattributed"` and
   `UnattributedAuthorEmail = "unattributed@gitops-reverser.invalid"` next to the committer
   constants, so the three identities are readable in one place.
2. `internal/watch/target_watch.go` — in `attachAuthor`, when the resolver is non-nil (i.e.
   attribution is enabled) and `ResolveAuthor` returns `ok=false`, stamp the placeholder
   `UserInfo` instead of leaving it zero. The resolver stays untouched; it already reports
   `AttributionAbsent`.
3. `internal/git/commit.go` — the `author.Username == ""` branch then means only
   "configured-author mode", which is exactly what it should have meant all along. Tighten
   its comment accordingly.
4. Flag plumbing in `cmd/main.go` alongside `--author-attribution`.

The placeholder is stamped at the **watch** layer rather than the commit layer on purpose: the
watch layer is the only place that knows attribution was *attempted and failed*, as opposed to
never attempted.

## Tests

- **Unit:** attribution enabled + no fact → author is the placeholder, committer unchanged;
  attribution disabled + no fact → author is the committer (unchanged); fact found → real
  actor (unchanged).
- **Unit:** two unattributed events land in one commit window (the equivalence class survives
  the rename); an unattributed and an attributed event do not.
- **e2e:** the existing author assertions gain a third expectation. Today a lost attribution
  fails as `expected jane@acme.com, got GitOps Reverser` — ambiguous. After, it fails as
  `got unattributed`, which names the cause in the failure message itself.

## Relationship to the open defect

This does **not** fix [attribution-loss.md](../debug/attribution-loss.md); it makes it
legible. Both are worth doing, and this one is worth doing *first*: it turns an invisible
correctness failure into one an operator can see in `git log` without a Prometheus query, and
it removes the ambiguity that made the underlying defect so slow to diagnose. It also gives
that investigation a sharper signal — an `unattributed` commit is a labelled instance of the
failure, tied to a specific object and time, rather than a counter increment.
