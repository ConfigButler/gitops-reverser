# Remove the Correlation Webhook

## What this document is

A concrete plan for deleting the validating correlation webhook, the in-memory correlation
store, and all related username-guessing code now that the audit stream consumer provides
direct user attribution from audit events.

---

## Architectural verdict: yes, this can go

The correlation webhook (`/process-validating-webhook`) is a pure side-effect webhook: it
intercepts every Kubernetes API call, extracts the username from the admission request, stores
it in an in-memory LRU cache keyed on a content hash, and **always returns `allowed: true`**.
It never blocks or mutates any resource.

Its only purpose was to bridge a gap: the watch path (informers) sees resource changes but
cannot see who made them. The webhook saw the user but not the full change; the correlation
store joined the two together — imperfectly, via a content-hash race.

The audit stream consumer eliminates that gap entirely:

```
auditv1.Event.User.Username   // always present; no guessing required
auditv1.Event.ImpersonatedUser.Username  // also handled, in resolveUserInfo()
```

The consumer already calls `resolveUserInfo` and populates `git.UserInfo.Username` before
routing any event. The correlation store is no longer needed.

### The one webhook that stays

There are **two** webhooks in the codebase. Only the correlation webhook is removed here.

| Webhook | Path | Admission control? | Decision |
|---|---|---|---|
| Correlation / event webhook | `/process-validating-webhook` | No — always allowed | **DELETE** |
| GitTarget validator | registered via `SetupGitTargetValidatorWebhook` | **Yes** — blocks duplicate (repo, branch, path) tuples; validates encryption config | **KEEP** |

The GitTarget validator is doing real work: it is the uniqueness constraint that prevents two
GitTargets from writing to the same path in the same repository. It has nothing to do with
correlation and must remain.

---

## What will change when the correlation webhook is removed

### Watch path loses username attribution

The watch path (informers in `internal/watch/manager.go`) calls `tryEnrichFromCorrelation`
today. After the correlation store is deleted, watch-path events will emit git commits with
an empty `UserInfo.Username`. This is already the fallback behaviour — the code handles
empty strings. The audit consumer path still provides usernames for every event it sees.

**Implication:** during any window where the audit consumer is not running (e.g. pod restart,
cluster without audit logging), git commits may have no author name. This was also true
before the correlation webhook existed for clusters that did not deploy the webhook, so this
is not a regression.

### Three e2e tests are deleted

These three tests exist only to verify the correlation webhook infrastructure. They have no
value after the webhook is removed:

| Test | Location | Why deleted |
|---|---|---|
| `"should route webhook traffic to the running controller pod"` | `test/e2e/e2e_test.go` | Tests webhook routing to pod — infrastructure gone |
| `"should have webhook registration configured"` | `test/e2e/e2e_test.go` | Asserts `ValidatingWebhookConfiguration` exists — removed |
| `"should receive webhook calls and process them successfully"` | `test/e2e/e2e_test.go` | Verifies `gitopsreverser_events_received_total` counter — metric removed |

All other e2e tests continue to pass. The audit consumer path and GitTarget validator tests
are unaffected.

---

## Complete list of deletions and modifications

### Files to delete entirely

| File | Why |
|---|---|
| `internal/correlation/store.go` | The correlation store itself |
| `internal/correlation/store_test.go` | Tests for the store |
| `internal/webhook/event_handler.go` | The correlation webhook handler |
| `internal/webhook/event_handler_test.go` | Tests for the handler |

If `internal/correlation/` becomes empty after the above, delete the directory.

### `cmd/main.go`

- Remove import: `"github.com/ConfigButler/gitops-reverser/internal/correlation"`
- Remove constants: `correlationMaxEntries`, `correlationTTL`
- Remove block: `correlationStore := correlation.NewStore(...)` and the callback wiring
- Remove field: `CorrelationStore: correlationStore` from the `watchMgr` initialisation
- Remove block: `eventHandler` creation and `/process-validating-webhook` registration

Keep everything related to:
- The audit handler (`/audit-webhook/{clusterID}`)
- `SetupGitTargetValidatorWebhook`
- The audit consumer (`queue.NewAuditConsumer`)

### `internal/watch/manager.go`

- Remove field: `CorrelationStore *correlation.Store` from the `Manager` struct
- Remove method: `tryEnrichFromCorrelation` (the method that queries the store)
- Remove the call site: wherever `tryEnrichFromCorrelation` is called during event processing
- Remove import: `"github.com/ConfigButler/gitops-reverser/internal/correlation"`

The watch manager will still produce git commits; it just will not populate `UserInfo.Username`
unless a future path (e.g. passing user context through informer events) provides it.

### `internal/telemetry/`

Remove the following metrics (and their `prometheus.MustRegister` calls and any helper
references):

- `WebhookCorrelationsTotal` (or equivalent) — correlation store write counter
- `EnrichHitsTotal` — correlation lookup hit counter
- `EnrichMissesTotal` — correlation lookup miss counter
- The `gitopsreverser_events_received_total` counter incremented by the correlation webhook
  handler (distinct from the audit events counter which stays)

Verify the exact metric names by reading `internal/telemetry/exporter.go` and grepping for
uses in `internal/webhook/event_handler.go`.

### Config / manifests

- `config/webhook.yaml` or equivalent: remove the `ValidatingWebhookConfiguration` stanza
  that matches `*/*` resources and routes to `/process-validating-webhook`. Leave any stanza
  for the GitTarget validator.
- `config/webhook/manifests.yaml`: same — remove the correlation webhook entry.
- Any Helm chart values or CRD/RBAC manifests that reference
  `gitops-reverser-validating-webhook-configuration` by that name should be reviewed; the
  name may be shared with the GitTarget validator, so verify before deleting.

### `test/e2e/e2e_test.go`

Remove the three test cases identified above. If they are inside a `Describe` block that
contains only these three tests, remove the entire `Describe` block. If the block has other
surviving tests, remove only the `It` entries.

---

## Tests that confirm nothing broke

After the deletion, the following must all pass:

```
make test                          # unit tests — no correlation or event_handler tests remain
make test-e2e                      # full e2e suite excluding the three deleted tests
make test-e2e-audit-redis          # producer + consumer path end-to-end
```

Pay particular attention to any e2e test that asserts a git commit contains a non-empty
author name — if such a test exists it needs to be driven through the audit consumer path,
not the watch path.

---

## What to leave alone

- `internal/watch/` — keep the watch manager and all informer logic. The watch path is still
  how the controller discovers resources initially (List+Watch). It just will not have
  usernames.
- `internal/sanitize/` — used by both paths; not affected.
- `internal/correlation/store.go` references in `internal/watch/manager.go` other than the
  enrichment call — there should be none, but verify with a grep.
- RBAC for the admission webhook server — the GitTarget validator still needs the
  `ValidatingWebhookConfiguration` RBAC. Do not remove webhook RBAC generically; remove
  only the specific rule/entry that covers `*/*` resources for the correlation path.

---

## Order of work

1. **Delete** `internal/correlation/store.go` and `store_test.go`.
2. **Delete** `internal/webhook/event_handler.go` and `event_handler_test.go`.
3. **Fix** `cmd/main.go`: remove imports, constants, correlation store init, `CorrelationStore`
   field, and the `/process-validating-webhook` registration block.
4. **Fix** `internal/watch/manager.go`: remove `CorrelationStore` field, `tryEnrichFromCorrelation`
   method, and the call site.
5. **Fix** `internal/telemetry/`: remove the three metrics and their registration.
6. **Fix** config manifests: remove the correlation `ValidatingWebhookConfiguration` entry.
7. **Fix** `test/e2e/e2e_test.go`: remove the three correlation webhook tests.
8. Run `make lint` and fix any remaining references the compiler or linter surfaces.
9. Run `make test` to confirm unit tests pass.
10. Run `make test-e2e` to confirm e2e tests pass with the three tests gone.
