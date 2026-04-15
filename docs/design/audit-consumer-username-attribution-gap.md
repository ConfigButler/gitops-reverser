# Audit Consumer: Username Attribution Gap

## What happened

The correlation webhook removal was completed (all Go code, tests, manifests, Helm chart).
All unit tests and linter pass cleanly. The e2e suite mostly passes — but one test fails:

```
[FAILED] Manager Manager should create Git commit when ConfigMap is added via WatchRule
Expected <string> to contain substring "jane@acme.com"
```

The test creates a ConfigMap `--as=jane@acme.com` via `kubectl` and asserts that the
resulting git commit has `jane@acme.com` as the author. This is **correct behaviour** —
the audit consumer is supposed to provide exactly this. The test should not be weakened.

---

## Root cause

The audit consumer's `Start` method is called and the consumer is running. However, every
`XREADGROUP` call fails with:

```
XREADGROUP failed: NOGROUP No such key 'gitopsreverser.audit.events.v1' or consumer group
'gitopsreverser-consumer' in XREADGROUP with GROUP option
```

Running `XINFO GROUPS gitopsreverser.audit.events.v1` on the live Valkey pod confirms:
**the consumer group does not exist**, despite the stream having 1310 messages and
`ensureConsumerGroup` logging "Consumer group ready" without error.

### Suspected cause: `NeedLeaderElection() = true` on a single-replica deployment

The controller-runtime manager only calls `Start` on Runnables that need leader election
**after** the leader lease is acquired. In a single-pod e2e deployment this normally works,
but if the lease is held by a previous pod (from the `prepare-e2e` reinstall) and the new
pod hasn't acquired it yet, the consumer won't start. When it does start, the stream exists
but the group hasn't been created on this connection.

A cleaner hypothesis: `ensureConsumerGroup` succeeds (creates the group) but the group is
created in the **same startup context that gets cancelled** when the leader lease is
re-acquired or the pod restarts — wiping the in-memory group. The stream persists in
Valkey but the group is lost if Valkey is restarted (Valkey may not be using persistent
storage in the e2e cluster).

### What to verify in a new session

1. **Is Valkey persistent?**
   ```
   kubectl get deploy -n valkey-e2e valkey -o yaml | grep -A5 storage
   kubectl exec -n valkey-e2e deploy/valkey -- valkey-cli CONFIG GET save
   kubectl exec -n valkey-e2e deploy/valkey -- valkey-cli CONFIG GET appendonly
   ```
   If Valkey uses no persistence and is restarted between test setup and pod startup,
   the group creation done by `ensureConsumerGroup` would be lost.

2. **Does the group get created at all?**
   After `prepare-e2e` and pod start, immediately run:
   ```
   kubectl exec -n valkey-e2e deploy/valkey -- valkey-cli XINFO GROUPS gitopsreverser.audit.events.v1
   ```
   If the group appears during pod startup but disappears later, Valkey is losing state.

3. **Does `ensureConsumerGroup` return an error that is being swallowed?**
   The `isAlreadyExistsErr` check only looks for `BUSYGROUP`. If Valkey returns a
   different error (e.g. auth, ACL, or connection error) for `XGroupCreateMkStream` that
   is NOT `BUSYGROUP` but is also not nil, the current code would return it as a fatal
   error from `Start`. But the log shows the consumer IS running (producing NOGROUP errors
   repeatedly), which means `ensureConsumerGroup` returned nil — so it logged "Consumer
   group ready" but the group doesn't actually exist.
   
   The most likely explanation: `XGROUP CREATE ... MKSTREAM` succeeds (returns OK), Valkey
   confirms the group, but then Valkey is restarted or the data is flushed, and by the
   time XREADGROUP runs, the group is gone.

4. **Is the audit handler actually writing to the same Valkey instance?**
   The producer (audit handler) and consumer use the same `--audit-redis-addr`. Check
   that the `config/deployment.yaml` address (`valkey.valkey-e2e.svc.cluster.local:6379`)
   resolves to the same pod that `XINFO` is run against.

---

## State of the removal at the time of pausing

### Completed
| Item | Status |
|---|---|
| Delete `internal/correlation/` package (6 files) | ✅ |
| Delete `internal/webhook/event_handler.go` + `_test.go` | ✅ |
| `cmd/main.go`: remove import, constants, store init, event handler, webhook registration | ✅ |
| `internal/watch/manager.go`: remove `CorrelationStore` field, `tryEnrichFromCorrelation`, unused imports | ✅ |
| `internal/watch/informers.go`: replace correlation call with `git.UserInfo{}` | ✅ |
| `internal/telemetry/exporter.go`: remove 4 correlation metrics | ✅ |
| `config/webhook.yaml`: replace correlation stanza with GitTarget validator | ✅ |
| `config/webhook/manifests.yaml`: same | ✅ |
| `charts/gitops-reverser/templates/admission-webhook.yaml`: GitTarget validator only | ✅ |
| `charts/gitops-reverser/values.yaml`: remove `webhook.validating`, add `webhook.caBundle` | ✅ |
| `test/e2e/e2e_test.go`: remove 3 correlation webhook tests | ✅ |
| `task lint` passes | ✅ |
| `task test` (unit tests) passes | ✅ |

### Remaining blocker
The `jane@acme.com` author assertion in the WatchRule e2e test fails because the audit
consumer's `XREADGROUP` can't find its consumer group. The consumer group is either not
being created persistently or is lost between Valkey restarts. This needs investigation
before the e2e suite can pass end-to-end.

The `test/e2e/e2e_test.go` author assertion at line ~811 has been **restored** to its
original form (it was temporarily removed during investigation but then put back). The
test is correct and should not be weakened.

---

## What the fix is likely to be

Either:
- Enable Valkey persistence in the e2e cluster (`appendonly yes` or RDB snapshots), **or**
- Make `ensureConsumerGroup` retry with backoff (in case the group is lost after a Valkey
  restart mid-session), **or**
- Move `ensureConsumerGroup` into the read loop so the group is re-created whenever
  `NOGROUP` is encountered (self-healing approach):

```go
func (c *AuditConsumer) readAndProcessBatch(ctx context.Context) error {
    streams, err := c.client.XReadGroup(ctx, ...).Result()
    if err != nil {
        if strings.Contains(err.Error(), "NOGROUP") {
            // Group was lost (Valkey restart?); recreate it.
            _ = c.ensureConsumerGroup(ctx)
        }
        return fmt.Errorf("XREADGROUP failed: %w", err)
    }
    ...
}
```

The self-healing approach is safest for production resilience regardless of whether the
e2e Valkey is persistent.
