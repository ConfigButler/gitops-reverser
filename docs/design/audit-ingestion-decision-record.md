# Audit Ingestion Decision Record

This file replaces the older "current state" snapshot for the audit pipeline.

The durable conclusion is simple:

- the Kubernetes audit webhook is the only reliable source for author-attributed live mutations
- watch-based reconciliation is still useful, but it is not a complete substitute for audit
- a simpler watch-only mode is still possible in the future, but it should be framed as a reduced
  capability mode that does not know the real actor

## Why this matters

GitOps Reverser is trying to turn live Kubernetes activity into Git history that is both:

- operationally correct
- attributable to the real user when possible

Those goals are stricter than "notice that something changed eventually."

## Approaches we tried

### 1. Watch-only live routing

This is the simplest model mechanically:

- watch the resulting object state
- sanitize it
- write it to Git

That can work for a simpler product mode, but it has two hard limits:

- the watch path does not reliably know the real request user
- the watch path only sees resulting object state, not the exact write intent or request context

So watch-only is acceptable only if we are willing to give up real author attribution and accept a
bot-style committer identity.

### 2. Request-time webhook or correlation-based enrichment

We also tried preserving request-time identity through separate webhook-style enrichment and then
joining that information back to the later observed object state.

This looks attractive at first, but it is fundamentally awkward:

- request-time signals happen before you can be sure the final object state is what will persist
- retries, updates, defaulting, later mutations, and failed writes make the join fuzzy
- syncing "who asked for a change" with "what actually ended up stored" is much harder than it
  sounds

In practice this became a correlation problem with edge cases at every seam. It was hard to make
reliable and even harder to make obviously correct.

That is the key lesson: request-time webhook data is not a trustworthy standalone source for final
Git history.

### 3. Mixed watch and audit live sources

We also had periods where both watch and audit could route the same logical live mutation.

That created exactly the kind of race you would expect:

- watch saw the object and wrote first
- audit arrived later with the real user attribution
- the later audit event often became a no-op because the Git content already matched

That meant the lower-fidelity source could suppress the higher-fidelity source. The result was
wrong authorship, duplicate-source complexity, and a lot of fragile source-precedence logic.

## Why audit webhook is the authoritative live path

The audit webhook is the best fit for the high-fidelity mode because it gives us:

- the real Kubernetes user information
- a single source of truth for live mutating activity
- a path that is conceptually tied to actual API operations, not only later observed state

It is still not magic, but it is the least-wrong authority for "who changed what" in live mode.

That is why the accepted architecture is:

- audit for live mutation authority
- watch for snapshot and reconcile behavior

Not:

- watch and audit competing for the same live write path
- webhook/correlation data trying to patch over a watch-only model

## Why watch is still needed

Watch-based logic still has a clear role:

- initial snapshot
- rule changes
- discovery changes
- retry loops for unavailable GVRs
- any future simplified mode that intentionally gives up author attribution

So the conclusion is not "watch is bad."

The conclusion is:

- watch is good for state reconciliation
- watch is not enough for high-fidelity author-attributed live history

## The viable simpler mode

A simpler mode without kube-apiserver audit integration is still a reasonable future idea.

That mode would be:

- watch/reconcile based
- simpler to install
- able to write useful Git state
- unable to reliably name the real end-user who made each change

That trade-off can be acceptable, but it should be described honestly. It is not equivalent to the
audit-backed mode.

## Remaining architectural debt

Two durable concerns remain even with audit authority:

### 1. Queue payload sensitivity

The current audit queue persists raw `payload_json`, which can include Secret material before Git-side
SOPS encryption happens.

That is why the queue must be treated as a security boundary and why payload minimization or
redaction is still worth doing.

### 2. Audit and watch are still separate systems

The design is intentionally split:

- audit handles live mutations
- watch handles reconciliation and snapshots

That split is correct, but bugs tend to appear at the seams:

- deduplication
- stale state
- source precedence
- rule-change timing

## Bottom line

If we want correct end-user attribution for live changes, the audit webhook is the only reliable
mechanism we have found.

If we want a simpler installation path, we can still add a watch-based mode later, but it should be
treated as a reduced-fidelity mode rather than as an equivalent replacement.
