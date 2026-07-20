# `debug/` — live investigations

Working notes for defects that are **understood but not fixed**, or **not yet understood**.
Unlike [`finished/`](../finished/), these are open; unlike [`design/`](../design/), they
describe something that is wrong now rather than something being decided.

Retire a page from here by fixing the defect and moving the write-up to `finished/`.

| Page | What it is | Status |
|---|---|---|
| [attribution-loss.md](attribution-loss.md) | ~7–10% of live commits are authored by the configured committer instead of the real actor, silently | **open** |
| [watch-construction.md](watch-construction.md) | Reference: how watches are built, and at what cardinality | reference |

## One-line state

`WatchRule`/`ClusterWatchRule` stream scoping is correct as of
[PR 2](../design/watchrule-source-namespace/pr2-stream-scope-collapse.md). The open defect is
in **author attribution**, and it is older than that work — it was found while landing PR 2,
not caused by it.
