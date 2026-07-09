# A CommitRequest that can assert an author

> Status: implemented
> Related: [README.md](README.md),
> [../commitrequest-admission-authorship.md](../commitrequest-admission-authorship.md),
> [../../attribution-setup-guide.md](../../attribution-setup-guide.md)

## Problem

Commit attribution is derived from an apiserver **audit** fact. That is the right
default — it names the actor who really caused a change, and it cannot be forged
by anyone who can create a `CommitRequest`. But it means attribution is only
available where an audit webhook can be configured, which excludes every hosted
control plane whose apiserver flags the operator does not own.

An authenticated control plane in front of the API already knows who the human
is: it verified their token before impersonating them. Making it re-derive that
identity through the apiserver's audit stream is a long way around.

`CommitRequestSpec` was `targetRef` + `message` + `closeDelaySeconds`. It
*finalizes* an open commit window; it could not *open* one with an identity.

## Shape

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: CommitRequest
spec:
  targetRef: { name: tenants }
  author:                                  # NEW
    name: "Ada Lovelace"
    email: "ada@example.com"
```

Asserting an author is a **privilege**, not a field anyone may set. It is
authorized by an RBAC verb on the target, in the style of `bind`, `escalate`, and
`impersonate`:

```yaml
rules:
  - apiGroups: ["configbutler.ai"]
    resources: ["gittargets"]
    resourceNames: ["tenants"]
    verbs: ["assert-author"]
```

## The guard, and why it does not depend on `failurePolicy`

The `/validate-operator-types` admission webhook already captures the submitter
of a `CommitRequest` into Redis, keyed by the object's UID. It now also, when
`spec.author` is set:

1. issues a `SubjectAccessReview` for the requester against
   `{verb: assert-author, group: configbutler.ai, resource: gittargets,
   namespace: <cr.namespace>, name: <spec.targetRef.name>}`;
2. **denies** the create when the review says no — so an unauthorized caller
   gets an immediate, legible error;
3. records the verdict on the admission record when it says yes.

The webhook is `failurePolicy: Ignore` by design (a down webhook must not wedge
the API). A guard that lived only there would therefore be bypassable by taking
the webhook down. So the **controller** is the real gate: it honors `spec.author`
**only** when an admission record exists for the object's UID *and* that record
carries the authorized verdict. No record — the webhook was off, bypassed, or
Redis is not configured — means the assertion is ignored, the commit is authored
by the configured committer, and the request reports:

```
AuthorAttributed=False  reason=AuthorAssertionUnverified
```

Fail-closed, and independent of `failurePolicy`.

## Effect on the commit

Before this change, `AttachCommitRequest.Author` was a *window-matching key*: it
selected which open commit window to finalize by comparing against the window's
author (which came from the mirrored resource's audit events), and it never
reached the Git signature.

An asserted author is different in both respects:

- it **matches any open window** for the target, because the assertion is a
  statement about the commit being made, not a claim to be the actor the audit
  stream happened to record;
- it **becomes the commit's author signature** (`name <email>`), with the
  configured committer still the committer, exactly as an audit-attributed commit
  is signed.

`AuthorAttributed=True` with reason `AuthorAsserted` distinguishes it from
`AttributedFromAdmission`.

## Ordering against audit attribution

When both are available, the assertion wins for the commit this `CommitRequest`
finalizes. It is the more specific, more recent statement, made by a caller who
had to hold an RBAC verb to make it. Mirrored-resource attribution
(`--author-attribution`) is unaffected for every commit *not* finalized by an
asserting `CommitRequest`.

## Threat model

- The verb is checked against the **named `GitTarget`**, so a tenant granted
  `assert-author` on their own target cannot author commits into someone else's.
- The asserted `name`/`email` are free text and are **not** verified to
  correspond to a real identity. They are what the trusted control plane says
  they are. Granting `assert-author` is granting the ability to write any author
  into the repository's history — treat it exactly like granting `impersonate`.
- The commit's *committer* remains the operator's configured identity, and
  commit signing (when configured) still signs as the committer. A reader can
  always tell that a commit was made by the reverser on someone's behalf.
