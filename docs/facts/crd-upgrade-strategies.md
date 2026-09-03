# Changing a CRD field: what the API server actually does

> **facts** — durable reference. Index: [`../INDEX.md`](../INDEX.md)
>
> Written while shipping the `GitTarget` API wave
> ([`../design/gittarget-api-wave.md`](../design/gittarget-api-wave.md)), which removed or renamed
> five fields across three kinds and tried both of the strategies below on the way. Every claim
> marked **measured** was run, not read: the envtests are named, and two were run against a live
> k3d cluster. Claims marked *inferred* are reasoning from documented behaviour and are labelled so
> you know which is which.
>
> This page exists because CRDs are the product's configuration surface. The cost of an API change
> is not the code — it is what happens to objects that already exist, and almost none of that is
> visible from the Go types.

## The one-line version

There are exactly two honest strategies for removing or renaming a CRD field, and the choice is
governed by how many consumers you have, not by which is technically nicer:

- **Retain and refuse** — keep the field in the schema, reject it, for one release. Costs code,
  buys a signal.
- **Delete outright** — remove it, accept that stored values are pruned, put the migration in a
  document. Costs nothing, buys nothing, and is correct when the population is small enough to
  migrate by hand.

A **conversion webhook is not a third option** for most changes. See the matrix.

### How silent "pruned silently" actually is — measured

Pruning is not uniformly quiet, and which half of it a user meets is decided by their client rather
than by the schema. Measured against this project's own CRDs on 1.31, applying a `GitTarget` that
still carries the removed `spec.allowedSourceNamespaces`:

| Client | Result |
|---|---|
| `kubectl apply` (strict field validation, the default since 1.25) | **rejected**: `strict decoding error: unknown field "spec.allowedSourceNamespaces"` |
| `kubectl apply --validate=ignore` | accepted, field absent from the stored object |
| A controller-runtime client, and any apply with `fieldValidation: Ignore` | accepted, field absent from the stored object |

So "delete outright" buys a signal after all for anyone applying by hand — the loud half of
retain-and-refuse, without the schema residue, though with a message that names the field rather
than its replacement. It buys nothing for a GitOps controller reconciling the manifest on the user's
behalf, which is the population that matters here and the reason the migration still belongs in a
document.

## Decision matrix

Pick the row that matches the change, then read across.

| The change | Auto-convertible? | Prune fails | Strategy that fits |
|---|---|---|---|
| Rename a field, same shape and semantics | yes, in principle | **closed** if the field is deny-by-default; **open** if it is an allow-flag | either; delete is fine with few consumers |
| Delete a field whose absence is *more* permissive | no | **open** — the quiet, dangerous case | retain and refuse, or a loud pre-upgrade inventory |
| Delete a field whose absence is *less* permissive | no | **closed** — an outage, not a hazard | delete; the stall is its own signal |
| Move a field to a **different kind** | **never** — conversion is per-object | open or closed, depending | delete + document; nothing else can work |
| Keep the spelling, change the meaning | **never** — nothing to translate | n/a, no field changes | document; there is no code-level signal available |
| Narrow an enum | n/a | value refused at admission | retain and narrow (cheapest of all) |
| Drop a capability with no replacement | **never** | n/a | document the loss explicitly |

**"Prune fails open"** is the column that matters most and the one that gets skipped. When a field
is pruned, ask: *does the object now do more, or less?* Less is an outage — loud, diagnosable, and
recoverable. More is a silent widening, and for anything that bounds data flow it is the failure
mode you cannot afford to discover from a user.

Worked example from the wave. Of five removed fields, four pruned fail-closed (a missing
deny-by-default policy admits nothing; a dropped delegation flag revokes a grant) and exactly one
failed open: `GitTarget.spec.allowedSourceNamespaces` bounded which source namespaces reach a Git
folder, so pruning it widened a `sourceNamespace: "*"` rule to every namespace its credential could
read. One row out of five carried nearly all the risk.

## Measured: the API server's behaviour

### A status update does not re-validate spec

**Measured** —
[`internal/controller/stored_superseded_value_status_test.go`](../../internal/controller/stored_superseded_value_status_test.go).

A controller **can** write a status update onto an object whose stored spec no longer validates
against its own CRD. The status subresource does not re-validate the spec, so the object that most
needs to explain why it was refused is able to.

Run three ways, all the same answer: Kubernetes **1.36**, and **1.31** with `CRDValidationRatcheting`
explicitly **on** and explicitly **off**. The gate is not what makes this work, which was the open
question — the concern was that ratcheting was doing the job and would vanish on an older cluster.

Without this, retain-and-refuse would be unusable: you could reject the value but never report the
rejection on the object carrying it.

### The ratcheting feature gate cannot be disabled from 1.33

**Measured** — kube-apiserver refuses to start with
`--feature-gates=CRDValidationRatcheting=false` on 1.36. `CRDValidationRatcheting` is beta and
default-on from 1.30 and GA in 1.33, and a GA gate is locked. Test the "gate off" case on ≤1.32 or
not at all.

### A field removed from the schema is invisible immediately, not after the next write

**Measured** — an envtest that stores a value, deletes the field from the CRD's structural schema,
and reads the object back with no write in between: the field is **absent from the response**.

This is the single most operationally important fact on this page, and it is the opposite of the
common assumption ("pruning happens on write, so I can still read the old value until something
touches the object"). Pruning *does* happen on write, but the field is not **served** the moment it
leaves the schema.

The consequence is an ordering constraint on every migration guide you will ever write: **any
inventory of soon-to-be-removed values must be taken before the new CRDs are applied.** Afterwards
the API cannot tell you what was there. *Inferred*: the bytes are presumably still in etcd until the
object is rewritten, so an etcd backup is a recovery path — but that is reasoning from documented
pruning behaviour, not something measured here, and the user's own manifests in Git are the
reliable record.

### A server-defaulted field cannot be removed by `kubectl apply`

**Measured against a live k3d cluster.** `ClusterProvider.spec.allowSourceNamespaceOverride`
carried `+kubebuilder:default=false`, so the API server had written it into **every stored object** —
including the chart-owned `default` provider, on installs that never used the feature. Re-applying
the clean manifest (which does not mention the field) left the value in place, because a
server-defaulted field was never in the user's manifest to be removed.

This has a sharp consequence for retain-and-refuse: **refusing every non-nil value of a previously
defaulted field refuses every object that has ever existed, and the operator cannot fix it.** That
is an upgrade nobody can complete. It was caught by an e2e run against a cluster carrying real
upgrade residue; neither lint nor the unit suite could see it, because both build their fixtures
from the current schema.

The rule that follows: **a defaulted field is refused only at its meaningful value.** A stored
`false` on a boolean whose replacement also defaults false carries no intent and must be tolerated.
This repo's `ClusterWatchRuleSpec.DeclaresNamespacedScope` already applied exactly this rule to its
own retained enum, refusing a stored `Namespaced` while letting the default `Cluster` through.

## Why a conversion webhook is usually not the answer

A conversion webhook converts **one object between versions of itself**. That single sentence
disqualifies it from most interesting changes:

- **It cannot move a field to another kind.** `GitProvider.spec.push.commitWindow` becoming
  `GitTarget.spec.commit.window` is not expressible: conversion cannot reach a different object, and
  one provider serves many targets, so there is no single target to write to.
- **It cannot invent intent.** Redefining a value's meaning (`"*"` from "the set this object admits"
  to "everything the credential can read") has no behaviour-preserving translation. Preserving the
  old reach would mean rewriting *other* objects.
- **It cannot restore a deleted capability.** Source-side label selectors had no replacement; there
  is nothing to convert to.

Of the five field changes in this wave, **two** were mechanically convertible and both were the
trivial renames.

### `strategy: None` does not do renames

`None` conversion relabels `apiVersion` and prunes fields absent from the target schema. A rename is
a drop plus an unset field, so `None` loses the value. It is only honest when the two versions are
structurally identical, which in practice means using a version bump to *sweep* fields already
refused and unused.

### Serving two versions reintroduces the silent prune

*Inferred, and the reasoning is worth keeping.* If `v1alpha3` still serves a field and `v1alpha4`
drops it, a user can apply a `v1alpha3` manifest carrying that field: it is **valid in that version**,
so admission accepts it, and conversion to the storage version drops it. That is precisely the silent
pruning retain-and-refuse exists to prevent, handed back by the mechanism meant to help.

Retain-and-refuse works *because* there is one served version and the field is refused in it. The
escape — refusing the field in the old version too — leaves the version bump buying nothing.

## The test that tells you whether a version bump is worth it

**Read the migration guide you would have to write either way. If it does not get shorter, the
version bump is not paying for anything.**

A CRD version bump buys exactly one thing: the API server translates old to new for you. So if every
step of the guide is a human decision — *which* target should this value move to, *what* did you
mean by `"*"`, *which* namespaces did that selector cover — there is nothing for conversion to
automate, and you have added a serving dependency and a cert lifecycle to change a string.

The cost side, for calibration: a conversion webhook is one of the few components whose
unavailability makes the API server **unable to serve those CRs at all**, including to your own
controller, including mid-upgrade.

## Mechanics worth remembering

**Refusing a value at admission** is a field-level CEL rule that always fails:

```go
// +optional
// +kubebuilder:validation:XValidation:rule="false",message="spec.oldField is renamed spec.newField. Same shape, same semantics: rename the key."
OldField *SomeType `json:"oldField,omitempty"`
```

On an optional field the rule is evaluated **only when the field is present**, so the object is
otherwise unaffected. Put the replacement's name in the message: the message is the entire migration
guide for the person who hits it.

**Admission covers the write path and nothing else.** An object written by an earlier release keeps
its value and is never re-admitted, so admission alone misses exactly the population that needs
telling. If a stored value matters, the reconciler must refuse it too — and if that refusal is meant
to *stop* something, it has to run before the data plane is declared, not merely be published as a
condition afterwards.

**Design-rationale comments belong outside the doc comment**, separated by a blank line, or they
become `kubectl explain` text. On a field the block goes above the doc comment; on a *type* it goes
above the `+kubebuilder:` marker block, because a block between the markers and the doc displaces
the markers and they are **dropped with no error** — this repo has lost a
`+kubebuilder:resource:scope=Cluster` that way. After editing API comments, regenerate into a
scratch directory and diff against `config/crd/bases` with every `description` stripped.

## What decided it here, and when that stops applying

The wave chose **retain and refuse** first, then reversed to **delete outright** on evidence about
reach: one known consumer, and 17 release-asset downloads across twelve releases. At that size, the
refusal machinery — 457 lines in four files, plus its wiring at four call sites — is more surface
than the population it protects, and a well-written migration guide is the better artifact.

That reasoning has an expiry date, and it is worth writing down so the reversal is not cargo-culted:

- **Two consumers on different schedules** and the calculus flips. Delete-and-document assumes every
  affected object can be migrated by someone who read the release notes.
- **Any consumer you cannot contact** — a public chart install, an unknown fork — and the fail-open
  row of the matrix becomes unacceptable, because you will learn about it from a data-exposure
  report rather than a stalled object.
- **A field that fails open on prune** deserves retain-and-refuse at a much lower consumer count
  than one that fails closed. Do not apply one policy to a whole release; apply the matrix per
  field.

## See also

- [`../design/gittarget-api-wave.md`](../design/gittarget-api-wave.md) — the wave these facts came
  from, including the version-strategy argument for keeping `v1alpha4` as a *sweep* rather than a
  carrier.
- [`../UPGRADING.md`](../UPGRADING.md) — the migration guide this page's ordering constraint shaped.
- [`../spec/where-validation-lives.md`](../spec/where-validation-lives.md) — the repo-wide rule for
  which rung a check belongs on, and why a webhook is not one of them.
