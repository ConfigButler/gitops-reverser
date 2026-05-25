# gitops-reverser feature request

## Title

Bound and monitor the durable audit queue by default

## Problem

The durable audit Redis/Valkey stream can grow without bound. On the demo
cluster this produced a multi-GB Valkey dataset and repeated Valkey restarts,
which temporarily broke audit ingestion.

Live observation:

```text
namespace: gitops-reverser
pod:       valkey-867df97d8b-cckmm
restartCount: 9
lastState.terminated.exitCode: 137
lastState.terminated.reason: Error
```

Valkey startup log after one restart:

```text
RDB memory usage when created 6124.68 Mb
Done loading RDB, keys loaded: 13359, keys expired: 645.
DB loaded from disk: 14.964 seconds
Ready to accept connections tcp
```

During that loading window, `gitops-reverser` could not enqueue or consume
audit events:

```text
XREADGROUP failed: dial tcp 10.96.98.248:6379: connect: connection refused
failed to claim audit decision ... LOADING Valkey is loading the dataset in memory
```

This is operationally risky because the audit queue is the handoff between the
Kubernetes API server audit webhook and the controller. A bloated queue can
turn normal demo traffic into a queue outage, and queue outages can lead to
missed or delayed Git commits.

## Current behavior

The chart default is unbounded:

```yaml
queue:
  redis:
    maxLen: 0
```

This renders:

```text
--audit-redis-max-len=0
```

`0` disables stream trimming. The debug stream also defaults to unbounded when
enabled:

```yaml
webhook:
  audit:
    debugStream:
      maxLen: 0
```

For a long-running cluster or a noisy audit policy, this allows unbounded data
growth in Valkey.

## Expected behavior

`gitops-reverser` should have a bounded, production-safe default for audit
stream storage. A default install should not be able to grow Valkey to several
GB without the operator explicitly opting into that behavior.

The queue should provide enough retention for normal controller catch-up and
short outages, but it should not act as an infinite audit archive. Kubernetes
audit logs or object storage are better places for long-term raw audit
retention.

## Suggested implementation

### 1. Change chart defaults

Set a bounded default for the hot audit stream, for example:

```yaml
queue:
  redis:
    maxLen: 10000
```

Treat the default as an operational starting point rather than a universal
capacity answer. Sizing should be based on expected audit events per second,
the outage/catch-up window the queue should tolerate, and a safety factor.

If exact retention is not required, keep using approximate Redis stream
trimming for performance:

```text
XADD ... MAXLEN ~ 10000
```

This is a deliberate tradeoff: bounded streams protect Valkey memory and reload
time, but they can trim entries before a slow or stopped consumer has processed
them. The implementation should make that tradeoff visible through metrics and
documentation.

Keep `0` available as an explicit opt-out for users who really want unbounded
retention:

```yaml
queue:
  redis:
    maxLen: 0 # explicit unbounded mode
```

### 2. Bound the debug stream too

If `webhook.audit.debugStream.enabled=true`, the debug stream should also have
a bounded default. For example:

```yaml
webhook:
  audit:
    debugStream:
      enabled: false
      maxLen: 10000
```

The debug stream is especially dangerous because it stores every decoded audit
event before filtering, joining, or dropping.

### 3. Surface a warning for unbounded mode

At startup, log a clear warning when either stream is unbounded, whether the
configuration came from Helm values or direct binary flags:

```text
audit redis stream max length is 0; queue retention is unbounded
```

This makes accidental unbounded production installs visible in logs.

### 4. Add metrics

Expose queue size and trimming-relevant metrics, for example:

- `gitops_reverser_audit_queue_stream_length`
- `gitops_reverser_audit_queue_consumer_lag`
- `gitops_reverser_audit_queue_pending_entries`
- `gitops_reverser_audit_queue_oldest_entry_age_seconds`
- `gitops_reverser_audit_queue_oldest_pending_age_seconds`
- `gitops_reverser_audit_debug_stream_length`

These should be low-cardinality and scoped to the configured stream names.
`pending_entries` alone is not enough: Redis pending entries are claimed but
unacked messages, while consumer-group lag captures unread backlog that may be
trimmed before consumption.

### 5. Document retention semantics

Document that:

- the Redis/Valkey stream is a durable work queue, not the long-term audit log;
- `maxLen` is an approximate upper bound if Redis `MAXLEN ~` is used;
- too-low values can drop events before slow consumers catch up;
- too-high values can create memory pressure and long restart/reload times.

## Configuration recommendation for current deployments

Until defaults change, set this explicitly:

```yaml
queue:
  redis:
    maxLen: 10000
```

For demo/dev deployments with debug stream enabled:

```yaml
webhook:
  audit:
    debugStream:
      enabled: true
      maxLen: 10000
```

## Acceptance criteria

- A fresh chart install renders a non-zero `--audit-redis-max-len` by default.
- Debug stream, when enabled, renders a non-zero
  `--audit-debug-redis-max-len` by default.
- Setting either value to `0` still works, but logs a startup warning.
- Unit tests cover bounded `MAXLEN ~` behavior and explicit unbounded `0`
  behavior for the canonical and debug streams.
- Chart tests or rendered-manifest assertions cover the bounded defaults and
  explicit `0` opt-out.
- Queue stream length, consumer-group lag, pending entries, and oldest
  entry/pending ages are observable via metrics.
- Helm README and project documentation explain the retention tradeoff and
  recommend sizing guidance.
- A long-running demo cluster under normal audit traffic does not grow Valkey
  into multi-GB RDB files without explicit unbounded configuration.

## Related bug

This was discovered while investigating generated-name `CommitRequest` objects
stuck in `WaitingForAuditEvent`. The queue growth was not the root cause of
that bug, but it made the failure mode worse by causing audit ingestion outages.

See:

```text
docs/tasks/generated-name-support.md
```
