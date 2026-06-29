---
title: CommitRequest authorship from admission — a command, captured at the source
status: design (forward-facing)
date: 2026-06-29
related:
  - ../finished/design-commit-request-api.md
  - ../finished/commitrequest-attribution-divert-reliability.md
  - watch-first-ingestion-architecture.md
  - watch-event-ordering-and-attribution-grace.md
  - reconcile-triggering.md
  - redis-key-schema-v3.md
---

# CommitRequest authorship from admission

> **Thesis.** A `CommitRequest` is a **command** ("save now, as me"), not a piece of
> mirrored state. Its author should be captured where the command is issued — a
> **validating admission webhook** dedicated to our own command kinds (the
> *internal-commands* webhook) — into its **own Redis corner**, and read back by the
> controller with **no wait**. This is the opposite of how we attribute mirrored
> resources (audit facts joined to watch events), and deliberately so: the two have
> different provenance and must not share a store. This design **replaces** the
> audit-sourced CommitRequest attribution rather than layering on top of it.

This is the forward-facing companion to
[commitrequest-attribution-divert-reliability.md](../finished/commitrequest-attribution-divert-reliability.md),
which adopted "Option C" (capture the author as a keyed fact at audit ingestion).
Option C was the right *shape* — a divert-immune keyed fact, not an ordered-stream
scan — but it kept the **wrong source** (the audit event) and therefore kept a
**wait**. Watch-first has since severed the only other thing the audit event did for
a CommitRequest (timing — now the close-delay collect window), so the audit event's
last remaining job here is to carry one string. We can get that string earlier, more
reliably, and without the audit webhook at all.

---

## 1. Two kinds of authorship, two provenances

We answer two different questions with two different mechanisms. Keeping them
separate is the whole point.

| | **Mirrored-resource attribution** | **Command authorship** |
|---|---|---|
| Question | "Who performed mutation X that *persisted* at RV N?" | "Who *issued* this command?" |
| Source | Audit event (`ResponseComplete`, 2xx, RV changed) | Admission request `userInfo` |
| Timing vs persist | **After** persist (proof of persistence) | **Before** persist (an assertion) |
| What makes it trustworthy | The audit accept gate ([`classifyAuditIngress`](../../internal/webhook/audit_handler.go)) | The controller only finalizes **persisted** objects (the informer never sees an un-persisted command) |
| Join | identity **+ resourceVersion**, matched within a grace window | identity 1:1 (`uid`), no join, no window |
| Store | `AttributionIndex` (`AuthorFact`, conflict markers, RV/auditID) | dedicated `CommandAuthorStore` under `author:v1:command` (this doc) |
| Lifetime | the watch-event join grace | admission → finalize (effectively immediate, §2) |

The reliability argument the project built the audit path on — *admission sees
attempts, not persistence* ([mutation-capture-lab-design.md](mutation-capture-lab-design.md),
rows 11–13) — **still holds for mirrored state and is unchanged.** It does not bite
a command, because we never write the command object into git. We extract one thing
from it (the author), and whether that extraction is ever *used* is gated downstream
by persistence: the controller reconciles off the informer, which only ever delivers
objects that actually landed in etcd. The persistence guarantee is not lost — it is
**relocated** from the audit event to the controller's watch.

### Why not reuse the attribution index (the "don't pretend it came from audit" rule)

Folding the admission assertion into the audit `AuthorFact`
([attribution_index.go:88-102](../../internal/queue/attribution_index.go#L88-L102))
would silently inherit semantics that are wrong for a command:

- **Conflict detection.** `storeFactKey` writes a `Conflict` marker when two users
  hit the same key ([attribution_index.go:457-473](../../internal/queue/attribution_index.go#L457-L473)).
  A command has exactly one creator; that machinery is dead weight at best and a
  corruption vector at worst.
- **RV / auditID / stageTimestamp.** All empty for an admission capture — the fields
  exist to disambiguate a *post-persist join*, which we do not do.
- **TTL semantics.** The fact TTL is tuned for the watch-join grace; the command
  record's TTL is purely for cleanup (§3).
- **Provenance blur.** A reader could no longer tell "proven persisted, by audit"
  from "asserted at admission, persistence enforced by the watch." Those are
  different trust models and should be legible as different.

So: a **separate store, separate author subnamespace, separate record type.** It is
not cosmetic separation — it keeps each path's invariant clean. The Redis naming plan
is `author:v1:audit` for persisted-resource facts and `author:v1:command` for
admission-captured commands ([redis-key-schema-v3.md](redis-key-schema-v3.md)).

---

## 2. The authorship invariant (why there is no wait)

> **By the time a command object is visible to anything — `kubectl get`, the informer,
> the controller — its author has already been recorded.**

This falls out of admission ordering:

1. A client `CREATE`s a `CommitRequest`.
2. The apiserver resolves `generateName → name`, assigns `metadata.uid`, runs
   mutating admission, then runs **validating** admission — our internal-commands handler.
3. Our handler **synchronously writes** `{uid → author}` to Redis and returns
   `Allowed`. The write completes *before* `Handle` returns.
4. *Only then* does the apiserver persist the object to etcd and return to the client.
5. *Only after persist* does the informer deliver it and the controller reconcile.

Step 3 strictly precedes steps 4–5. So when the controller first sees the object, the
record is **present-or-never**: present if the webhook ran, never if it did not (the
internal-commands webhook is not configured, or a Redis write failed under
`failurePolicy: Ignore`). **Waiting cannot help** — there is no asynchronous arrival
to wait for, unlike an audit event that trickles in seconds after persist. This is the
precise reason the 60 s `commitRequestAttributionTimeout` wait
([commitrequest_controller.go:77-82](../../internal/controller/commitrequest_controller.go#L77-L82))
disappears: it was an artifact of the audit event's *post-persist* timing, and that
timing is gone.

Contrast with audit, where the order is inverted (persist → audit event → maybe
seconds later → fact in Redis), which is exactly why the audit-sourced controller
*had* to poll and fall back on a timeout.

**This does not weaken the "prior edits already landed" guarantee.** That was an
audit-stream-ordering property in the audit-first era; in watch-first it is the
**close-delay collect window** (`finalizeAt = receipt + CloseDelaySeconds`,
[commit_request_attach.go:87-90](../../internal/git/commit_request_attach.go#L87-L90)),
which is independent of where the author string comes from. Moving authorship to
admission touches the *who*, never the *when*.

---

## 3. The dedicated Redis corner

A small store hung off the always-present `RedisStore`
([redis_store.go](../../internal/queue/redis_store.go)), mirroring how
`AttributionIndex` and the watch-cursor store share the one connection but own
disjoint key namespaces. It lives under the same top-level `author` Redis domain as
audit-sourced authorship, but in the separate `command` subfamily because its
provenance and lookup semantics are different. It is generic over command kinds
(keyed by `uid` alone, which is globally unique) so a future command CRD reuses it
unchanged.

```go
// internal/queue/command_author_store.go

// commandAuthorKeySuffix namespaces the keys this store owns. It shares the top-level
// author domain with audit-sourced resource facts, but the command subfamily has a
// different provenance and no grace-window join.
const commandAuthorKeySuffix = ":author:v1:command:"

// commandAuthorRecordTTL bounds a captured authorship record. It is NOT tuned to cover
// any wait: by the authorship invariant (§2) the record is written before the object
// exists, and the controller reads it on the first reconcile after persist — typically
// sub-second. The TTL exists ONLY so an orphan record (a command object deleted before
// its reconcile) self-cleans. It is a fixed internal constant, deliberately not a flag:
// there is nothing to tune. An hour is generous headroom for a slow reconcile backlog
// or a leader failover.
const commandAuthorRecordTTL = time.Hour

// CommandAuthor is the minimal authorship captured at admission for one command object.
// It carries only what a git commit author needs — no RV, no auditID, no conflict bit:
// this is a 1:1 command capture, not a post-persist join.
type CommandAuthor struct {
	Author      string `json:"author"`
	DisplayName string `json:"displayName,omitempty"`
	Email       string `json:"email,omitempty"`
	RequestedAt string `json:"requestedAt,omitempty"` // RFC3339Nano, for lag metrics/debug
}

// CommandAuthorStore records and reads command authorship. It shares RedisStore's
// connection but is wired whenever the internal-commands webhook is enabled —
// independent of --author-attribution, which only governs mirrored-resource attribution.
type CommandAuthorStore struct {
	client *redis.Client
	ttl    time.Duration
}

// RecordCommandAuthor is the admission-side write: capture the authenticated submitter
// the instant a command CREATE is admitted, before it persists. Last-write-wins (a
// CREATE fires admission once; a retried admission re-asserts the same user).
func (s *CommandAuthorStore) RecordCommandAuthor(
	ctx context.Context, uid types.UID, author CommandAuthor,
) error {
	raw, err := json.Marshal(author)
	if err != nil {
		return fmt.Errorf("marshal command author: %w", err)
	}
	return s.client.Set(ctx, s.key(uid), raw, s.ttl).Err()
}

// LookupCommandAuthor is the controller-side read, keyed by the persisted object's UID.
// ok=false means no record was captured — the internal-commands webhook is not
// configured (or a best-effort write missed) — and the controller finalizes as the
// committer, immediately, with AuthorAttributed=False (§5).
func (s *CommandAuthorStore) LookupCommandAuthor(
	ctx context.Context, uid types.UID,
) (CommandAuthor, bool) {
	raw, err := s.client.Get(ctx, s.key(uid)).Bytes()
	if err != nil {
		return CommandAuthor{}, false
	}
	var a CommandAuthor
	if json.Unmarshal(raw, &a) != nil || a.Author == "" {
		return CommandAuthor{}, false
	}
	return a, true
}

// key identifies the command by UID alone — globally unique, like the watch cursor key,
// so namespace/name/kind would be redundant (kept out for a tight key).
func (s *CommandAuthorStore) key(uid types.UID) string {
	return keyPrefix + commandAuthorKeySuffix + escapeKeyField(string(uid))
}
```

And the builder on `RedisStore`, alongside `AttributionIndex(...)`:

```go
// CommandAuthorStore builds the command-authorship store on this connection. Wire it
// when the internal-commands webhook is enabled; it does not depend on attribution.
func (s *RedisStore) CommandAuthorStore() *CommandAuthorStore {
	return &CommandAuthorStore{client: s.client, ttl: commandAuthorRecordTTL}
}
```

> **Keying by UID only.** The controller reads by the persisted object's `uid`, which
> admission already assigned. `generateName` is a non-issue: the server-assigned name
> *and* uid are both present in `request.Object` by the time validating admission runs,
> so we never have to recover a name from a response body the way the audit path did.

---

## 4. The internal-commands admission handler

One handler on its own path, `/internal-commands`, that recognizes **our own command
kinds** and captures the submitter of each. It is not CommitRequest-specific by
construction — it dispatches on a small registry of command resources, so a future
command CRD is one table entry plus one webhook rule, not a new handler. It replaces
the no-op [`AdmissionAllowHandler`](../../internal/webhook/admission_allow_handler.go)
*for this purpose*; the catch-all `*` observer in
[validating-webhook.yaml](../../config/webhook/validating-webhook.yaml) is a separate,
future policy extension point and stays as-is.

```go
// internal/webhook/internal_commands_handler.go

const InternalCommandsPath = "/internal-commands"

// commandKinds is the registry of our own command CRDs whose submitter we capture at
// admission. Adding a command kind is one entry here plus one rule in the webhook
// config — the handler body does not change.
var commandKinds = map[metav1.GroupResource]struct{}{
	{Group: "configbutler.ai", Resource: "commitrequests"}: {},
	// future: {Group: "configbutler.ai", Resource: "<next-command>"}: {},
}

// InternalCommandsHandler captures the authenticated submitter of one of our command
// objects into the CommandAuthorStore and always allows. It is pure observation with a
// single side effect (a Redis upsert); it never rejects.
type InternalCommandsHandler struct {
	Store   *queue.CommandAuthorStore
	Decoder admission.Decoder
}

func (h *InternalCommandsHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	gr := metav1.GroupResource{Group: req.Resource.Group, Resource: req.Resource.Resource}
	if _, ok := commandKinds[gr]; !ok {
		return admission.Allowed("not a command kind") // belt-and-suspenders; rules already scope us
	}
	// Dry-run never persists, so the controller will never read this record — and we
	// declare sideEffects: NoneOnDryRun, so we must honor it.
	if req.DryRun != nil && *req.DryRun {
		return admission.Allowed("dry-run: not recorded")
	}

	// metadata.uid identifies the object (not req.UID, which identifies the review).
	obj := &metav1.PartialObjectMetadata{}
	if err := h.Decoder.Decode(req, obj); err != nil {
		// Cannot key the record without a uid; allow the command, fall back to committer.
		log.FromContext(ctx).Error(err, "decode command object for authorship", "name", req.Name)
		return admission.Allowed("undecodable: not recorded")
	}

	author := queue.CommandAuthor{
		Author:      req.UserInfo.Username, // effective (impersonated) user — apiserver resolved it
		DisplayName: firstExtraValue(req.UserInfo.Extra, displayNameExtraKey),
		Email:       firstExtraValue(req.UserInfo.Extra, emailExtraKey),
		RequestedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if author.Author == "" {
		return admission.Allowed("no user to attribute") // anonymous never reaches a persisted CREATE
	}

	// Synchronous, best-effort: the write completes before we return, so it is present
	// before the object is visible (the authorship invariant, §2). A failure degrades to
	// the committer — never block the command.
	if err := h.Store.RecordCommandAuthor(ctx, obj.UID, author); err != nil {
		log.FromContext(ctx).Error(err, "record command author; finalizing as committer",
			"namespace", req.Namespace, "name", req.Name)
	}
	return admission.Allowed("author recorded")
}
```

The matching `ValidatingWebhookConfiguration` rule — **narrow, `Ignore`, and packaged
in the Helm chart** (this is product behavior now, not the e2e-only `*` observer):

```yaml
webhooks:
  - name: internal-commands.configbutler.ai
    clientConfig:
      service: { name: gitops-reverser-webhook, namespace: <ns>, path: /internal-commands, port: 9443 }
    rules:
      - operations:  [CREATE]
        apiGroups:   [configbutler.ai]
        apiVersions: [v1alpha2]
        resources:   [commitrequests]   # one line per command kind
    failurePolicy:    Ignore            # authorship is best-effort; never block the command
    sideEffects:      NoneOnDryRun       # Redis write on real requests; nothing on dry-run
    matchPolicy:      Equivalent
    timeoutSeconds:   2
    admissionReviewVersions: [v1]
```

`failurePolicy: Ignore` here is correct for the opposite reason it is in the e2e
observer: not to avoid a bootstrap deadlock, but because a missed author capture must
degrade to the committer, never reject a user's command. The handler also returns
`Allowed` even when the Redis write errors, so the command's success never depends on
the failure policy — `Ignore` only covers the webhook being entirely unreachable.

---

## 5. The controller, before and after

The state machine loses its first wait entirely. Author resolution becomes a single
synchronous lookup with two outcomes, both immediate and both settled.

**Before** ([commitrequest_controller.go:164-233](../../internal/controller/commitrequest_controller.go#L164-L233)):

```
first sight ─▶ Attributed=Unknown, WaitingForAuditEvent
              │
              ▼  attributeAuthor: poll LookupCommitRequestAuthor every 2s
              ├─ hit  ───────────────▶ attributionResolved
              ├─ within 60s, miss ──▶ attributionPending  → requeue 2s (THE WAIT)
              └─ past 60s, miss ────▶ attributionTimedOut (Attributed=False)
              ▼
            ATTACH + POLL (close-delay) ─▶ terminal
```

**After:**

```
first sight ─▶ attributeAuthor: LookupCommandAuthor (one read)
              ├─ hit  ─▶ author named,  AuthorAttributed=True  (AttributedFromAdmission)
              └─ miss ─▶ committer,     AuthorAttributed=False (CommitterFallback)
              ▼  (no Unknown phase, no requeue-for-author)
            WaitingForCloseDelay ─▶ ATTACH + POLL ─▶ terminal
```

Concretely, `attributeAuthor` collapses to:

```go
func (r *CommitRequestReconciler) attributeAuthor(
	ctx context.Context, cr *configbutleraiv1alpha2.CommitRequest,
) (queue.CommandAuthor, commitRequestAttribution) {
	if r.AuthorLookup == nil { // internal-commands webhook disabled → committer-only
		return queue.CommandAuthor{}, attributionCommitter
	}
	if a, ok := r.AuthorLookup.LookupCommandAuthor(ctx, cr.UID); ok {
		return a, attributionFromAdmission
	}
	return queue.CommandAuthor{}, attributionCommitter // present-or-never: a miss is committer, now
}
```

The reconcile body drops the `waitForAudit` branch and its
`RequeueAfter: commitRequestAttributionRetryDelay`
([commitrequest_controller.go:168-171](../../internal/controller/commitrequest_controller.go#L168-L171));
first-sight stamping goes straight to `WaitingForCloseDelay`.

### Status: the `AuthorAttributed` condition

Rename the domain condition `Attributed` → **`AuthorAttributed`**
([constants.go:59-63](../../internal/controller/constants.go#L59-L63)) and give it a
clean **binary, immediately-settled** meaning — no `Unknown`, no timeout state:

| `AuthorAttributed` | Reason | Meaning |
|---|---|---|
| `True` | `AttributedFromAdmission` | The submitter was captured at admission and named as the commit author. |
| `False` | `CommitterFallback` | No admission author record — the internal-commands webhook is not configured (or did not record one) — so the commit is authored by the configured committer. |

`AuthorAttributed=False` is **not a failure** and does not affect `Ready`: the command
still commits successfully, just as the committer. The pairing `Ready=True` +
`AuthorAttributed=False` is the precise, honest signal a user reads as *"saved — but as
the bot, because author attribution isn't wired up here."* This is the status surface
for the edge case the whole design hinges on (record absent ⇒ webhook not configured ⇒
committer), and it is why the absent case earns a real condition rather than being
hidden.

Removed reasons from
[commitrequest_finalize.go:36-50](../../internal/controller/commitrequest_finalize.go#L36-L50):
`WaitingForAuditEvent` (and its `Attributed=Unknown` first-sight state),
`AttributedFromAuditEvent` (→ `AttributedFromAdmission`), `AuditEventNotObserved`, and
`AttributionNotRequired` (→ `CommitterFallback`). The `commitRequestAttribution` enum
drops `attributionPending`/`attributionTimedOut`, keeping `attributionFromAdmission`
and `attributionCommitter`. e2e specs asserting the old reasons or the
`WaitingForAuditEvent` phase must be updated — call this out in the PR. (v1alpha2 is
alpha, so the status-contract change is acceptable.)

---

## 6. Wiring (`cmd/main.go`)

Authorship capture is gated by the **internal-commands webhook**, not
`--author-attribution`:

```go
// Command authorship is captured at admission and lives in its own Redis corner,
// independent of --author-attribution (which governs mirrored-resource attribution).
var commandAuthorStore *queue.CommandAuthorStore
if cfg.internalCommandsWebhookEnabled {
	commandAuthorStore = redisStore.CommandAuthorStore()
	mgr.GetWebhookServer().Register(
		webhookhandler.InternalCommandsPath,
		&ctrladmission.Webhook{Handler: &webhookhandler.InternalCommandsHandler{
			Store:   commandAuthorStore,
			Decoder: admission.NewDecoder(scheme),
		}},
	)
}
// ...
if err := (&controller.CommitRequestReconciler{
	// ...
	AuthorLookup: commandAuthorStore, // nil when the webhook is disabled → committer-only, immediate
}).SetupWithManager(mgr); err != nil { /* ... */ }
```

The `AuthorLookup == nil` contract is preserved and *strengthened*: nil → committer-only;
non-nil + miss → committer-only **for that request** — both immediate, neither waits,
both surface `AuthorAttributed=False`.

**HA.** Admission can hit any replica (it is a `Service`); the controller leader reads
from the shared Redis corner. The synchronous-write-before-persist invariant holds per
request regardless of which replica served admission, so a leader failover between
admission and finalize still finds the record.

> **Flag naming.** The existing `--admission-webhook` flag and its `admission-webhook-*`
> cert flags ([cmd/main.go](../../cmd/main.go)) gate the e2e `*` observer today. Decide
> whether the internal-commands webhook rides the same admission server (one flag, one
> cert, one port — likely yes) or gets its own gate. Recommended: **one admission
> server**, both handlers registered on it, gated by the existing `--admission-webhook`;
> the internal-commands handler is always registered when that server is on.

---

## 7. TLS — same story as the other certs

The internal-commands webhook needs a serving cert exactly like the metrics endpoint
and the audit webhook. Follow the **established pattern**: full configuration for those
who need it, and a cert-manager easy-route shipped in the chart for everyone else.

- **Full configuration (BYO).** Keep the existing serving-cert flags
  (`--admission-webhook-cert-path` / `-cert-name` / `-cert-key`,
  [cmd/main.go](../../cmd/main.go)) so an operator can mount any cert (their own PKI, a
  different issuer, a rotated secret). The cert-watcher already hot-reloads on rotation,
  as the audit and metrics certs do.
- **Easy route (chart + config).** The Helm chart and the `config/` kustomize overlay
  ship the cert-manager resources that wire it up automatically, the same way the other
  endpoints are templated:
  - an `Issuer`/`Certificate` minting the admission serving cert into a Secret the
    Deployment mounts;
  - the `cert-manager.io/inject-ca-from` annotation on the
    `ValidatingWebhookConfiguration` so the CA bundle is injected for you — the
    config overlay already uses exactly this annotation
    ([validating-webhook.yaml](../../config/webhook/validating-webhook.yaml)).
  Gate the whole block on a chart value (e.g. `internalCommands.enabled`,
  `internalCommands.certManager.enabled`) so a non-cert-manager user can opt into BYO
  certs instead. See [audit-webhook-tls-design.md](audit-webhook-tls-design.md) for the
  long-lived-CA + short-lived-serving-cert rotation model to mirror.

This is the one genuinely new packaging surface (the chart "ships no webhook/cert
templates" today, per the header in
[validating-webhook.yaml](../../config/webhook/validating-webhook.yaml)). It is real,
but it is the standard operator-managed-webhook pattern and far less than the
apiserver-side wiring the audit webhook demands (§9).

---

## 8. Edge cases and degradation

| Case | Behavior |
|---|---|
| **Internal-commands webhook not configured** | `AuthorLookup == nil` (or record absent) → committer, immediate, **`AuthorAttributed=False (CommitterFallback)`**. The headline edge case: no record means the webhook isn't wired up; fall back to the committer and *say so* in status. |
| Redis write fails at admission | Handler logs, returns `Allowed`; controller finds no record → committer, `AuthorAttributed=False`. |
| Webhook pod unreachable | `failurePolicy: Ignore` admits the command; no record → committer, `AuthorAttributed=False`. |
| `generateName` | Name + uid present in `request.Object` at validating admission; recorded by uid. No response-body recovery (the audit path's `generateName` headache is gone). |
| Impersonation (`--as`) | `req.UserInfo` is the effective (impersonated) user — parity with the audit path's `resolveUserInfo`. |
| Service-account submitter | Recorded by its `system:serviceaccount:…` username like any other; `AuthorAttributed=True`. |
| Object deleted before reconcile | Informer never reconciles it; the orphan record self-cleans via the fixed TTL. Harmless. |
| Recreate same name (new uid) | Different uid → different key; no stale inheritance. |
| Webhook on but record missing | A real anomaly (write miss). `AuthorAttributed=False` like any committer fallback; surface it via a `command_author_lookup{result=miss}` metric so it is observable rather than silent. |

---

## 9. Note for end users — why your *commands* are attributed but your *edits* may not be

There is an honest asymmetry worth stating plainly, because users will notice it:

> *"You can name me as the author of a CommitRequest I create, but my normal edits to a
> ConfigMap commit as the bot unless I run the audit webhook. Why?"*

Because the two are not the same kind of thing, and crediting them the same way would
be wrong:

- A **command** (a CommitRequest) is something you hand us directly, and we only ever
  *act* on it once it has actually persisted. If your command never persists (rejected
  by another webhook, a conflict), we simply never act — so attributing it at admission
  can never put a wrong author on a real commit. We can safely sign for the parcel you
  handed us.
- A **normal edit** attributed at admission could credit you with a change that **never
  landed** — a later webhook rejects it, an optimistic-concurrency conflict drops it, a
  dry-run was only a rehearsal. If we mirrored that into git, the repository would claim
  a change the cluster never accepted. We will not sign for a delivery we only saw
  *attempted*. Proof-of-persistence for an arbitrary edit lives only in the **audit
  event**, which is why faithful attribution of normal activity needs the audit webhook.

So the asymmetry is a **feature, not a gap**: gitops-reverser gives you the most
attribution each environment can *correctly* support. On a managed cluster where you
cannot enable an apiserver audit webhook, your commands are still attributed (admission
is enough for them), while your normal edits commit as the configured committer until
audit is available — and the `AuthorAttributed` condition tells you, per command,
exactly which path you got. Nothing is ever attributed on a guess.

This is the same principle as the rest of the design: **command authorship is "who
asked"; state attribution is "what actually happened, and who."** We never conflate them.

---

## 10. What this removes (forward-facing, not kept in parallel)

Per the "don't keep the old audit ingestion around for this" directive:

- **`AttributionIndex.LookupCommitRequestAuthor`**
  ([attribution_index.go:410-423](../../internal/queue/attribution_index.go#L410-L423))
  and the `CommitRequestAuthorLookup` interface
  ([commitrequest_controller.go:54-59](../../internal/controller/commitrequest_controller.go#L54-L59))
  — deleted; the controller depends on `CommandAuthorStore` instead.
- The **wait machinery**: `commitRequestAttributionTimeout`,
  `commitRequestAttributionRetryDelay`, the `waitForAudit` return, the
  `WaitingForAuditEvent` progress state, and the `Attributed=Unknown/False` audit
  states (§5).
- `commitRequestResolveTimeout` shrinks by its `+60s` attribution component
  ([commitrequest_controller.go:92-98](../../internal/controller/commitrequest_controller.go#L92-L98)).
- The audit handler still records facts for every mutating resource generically, so
  `commitrequests` facts are *written* but **no longer read** for authorship.
  Optionally skip recording the `commitrequests` resource in `RecordFact` to avoid dead
  keys (minor).

The audit webhook itself is **untouched and still required for mirrored-resource
attribution** — this change is scoped to the command path only.

---

## 11. Resolved decisions & remaining open questions

**Resolved (this iteration):**

- **Record cleanup:** TTL, fixed internal constant (`commandAuthorRecordTTL`), **no
  flag** — the timing is deterministic, so there is nothing to tune (§3).
- **Handler shape:** a dedicated, extensible **internal-commands** handler/path, not a
  CommitRequest-only one — future command kinds are a table entry plus a webhook rule (§4).
- **Absent record:** fall back to committer and surface it as
  **`AuthorAttributed=False (CommitterFallback)`** (§5, §8).
- **TLS:** the established pattern — full BYO-cert configuration plus cert-manager
  resources shipped in the chart/`config` for the easy route (§7).

**Still open:**

- **Cleanup nicety:** rely on TTL alone (chosen) or also delete the record on terminal
  status? TTL is sufficient; deletion is optional polish.
- **Capture-lag metric:** record `now − RequestedAt` at lookup as an
  admission→finalize latency gauge, and a `command_author_lookup{result}` counter to
  catch write-misses (§8)?
- **One admission server or two webhooks:** ride the existing `--admission-webhook`
  server (recommended, §6) vs. a separate gate.
- **Chart value surface:** the exact value names for enabling the webhook and choosing
  cert-manager vs. BYO certs (§7).
