# Production Debug Report: gitops-reverser

Date: 2026-02-26
Workspace: `/workspaces/gitops-reverser2`

## Scope
- Investigate why configmap/secret changes in `gitops-reverser-example-audit-namespace` are not being tracked.
- Read-only investigation against cluster, except allowed restart of gitops-reverser and changes in `gitops-reverser-example-audit` namespace.

## Findings Log

### 1) Context and deployment health

Command:
```bash
kubectl config current-context
kubectl -n gitops-reverser get all
helm list -A | rg gitops-reverser
```

Result:
- Current context: `k3s`
- Pod/Deployment/Service for gitops-reverser are healthy in namespace `gitops-reverser`
- Helm release: `gitops-reverser-gitops-reverser` chart `gitops-reverser-0.17.0+01bf7040249c`

---

### 2) Config objects and watch scope

Command:
```bash
kubectl -n gitops-reverser-example-audit get gittargets,watchrules -o yaml
kubectl explain watchrule.spec
kubectl get clusterwatchrules -A -o yaml
```

Result:
- `WatchRule only-configmaps` exists in `gitops-reverser-example-audit`
- Rule watches resources: `configmaps`, `secrets`
- `WatchRule` is namespace-scoped (per `kubectl explain watchrule.spec`)
- No `ClusterWatchRule` exists

Interpretation:
- Current setup only tracks objects in namespace `gitops-reverser-example-audit`.

---

### 3) Namespace mismatch check

Command:
```bash
kubectl get ns | rg gitops-reverser-example
kubectl get ns gitops-reverser-example-audit-namespace -o yaml
```

Result:
- Existing namespace: `gitops-reverser-example-audit`
- Namespace `gitops-reverser-example-audit-namespace` does **not** exist

Interpretation:
- If changes were made in `gitops-reverser-example-audit-namespace`, they cannot be tracked because the namespace does not exist.

---

### 4) Metrics before test

Command:
```bash
kubectl -n gitops-reverser exec deploy/gitops-reverser -- sh -c 'wget -qO- http://127.0.0.1:8080/metrics | grep "^gitopsreverser_" | sort'
```

Key values before controlled test:
- `gitopsreverser_events_received_total`: 6089
- `gitopsreverser_events_processed_total`: 18
- `gitopsreverser_commits_total`: 1
- `gitopsreverser_objects_written_total`: 1

---

### 5) Controlled end-to-end test in `gitops-reverser-example-audit`

Command:
```bash
ns="gitops-reverser-example-audit"
cm="codex-debug-cm-081911"
sec="codex-debug-secret-081911"

kubectl -n "$ns" create configmap "$cm" --from-literal=foo=one
kubectl -n "$ns" create secret generic "$sec" --from-literal=foo=one
kubectl -n "$ns" create configmap "$cm" --from-literal=foo=two -o yaml --dry-run=client | kubectl apply -f -
kubectl -n "$ns" create secret generic "$sec" --from-literal=foo=two -o yaml --dry-run=client | kubectl apply -f -
kubectl -n "$ns" delete configmap "$cm" --wait=true
kubectl -n "$ns" delete secret "$sec" --wait=true
```

Log correlation command:
```bash
kubectl -n gitops-reverser logs deploy/gitops-reverser --since=5m | rg 'codex-debug-cm-081911|codex-debug-secret-081911|Created commit|All events pushed|writeEvents operation|Starting git commit and push'
```

Observed logs (highlights):
- `Starting git commit and push` with `eventCount: 6`
- `Created commit` for:
  - `CREATE v1/configmaps/codex-debug-cm-081911`
  - `CREATE v1/secrets/codex-debug-secret-081911`
  - `UPDATE v1/configmaps/codex-debug-cm-081911`
  - `UPDATE v1/secrets/codex-debug-secret-081911`
  - `DELETE v1/configmaps/codex-debug-cm-081911`
  - `DELETE v1/secrets/codex-debug-secret-081911`
- `All events pushed to remote` after each event

Post-test metrics/status:

Command:
```bash
kubectl -n gitops-reverser exec deploy/gitops-reverser -- sh -c 'wget -qO- http://127.0.0.1:8080/metrics | grep -E "^gitopsreverser_(events_received_total|events_processed_total|objects_written_total|commits_total|git_operations_total|git_commit_queue_size)" | sort'
kubectl -n gitops-reverser-example-audit get gittarget to-folder-live-cluster -o jsonpath='{.status.lastCommit}{"\n"}{.status.lastReconcileTime}{"\n"}{.status.snapshot.stats.created}{"\n"}{.status.snapshot.stats.deleted}{"\n"}'
```

Result:
- `events_processed_total`: 18 -> 24
- `commits_total`: 1 -> 2
- `git_operations_total`: 1 -> 7
- `objects_written_total`: 1 -> 7
- `GitTarget.status.lastCommit`: changed to `3e4cfd329e3b3e7638253bb3db3beb61ce363a0c`

Interpretation:
- ConfigMap/Secret tracking is functioning in `gitops-reverser-example-audit`.

---

### 6) Additional finding: startup seed race (likely non-blocking after startup)

Command:
```bash
kubectl -n gitops-reverser logs deploy/gitops-reverser --since=20m | rg -i 'Failed to route event to GitTargetEventStream|dropping event'
```

Observed error:
- `Failed to route event to GitTargetEventStream - dropping event`
- `error: no GitTargetEventStream registered for gitops-reverser-example-audit/to-folder-live-cluster`
- Appears around startup/initial seed window.

Interpretation:
- There is a race during initial seed listing where events can be dropped before the stream is registered.
- This did **not** prevent later live create/update/delete processing in this test.

---

## Conclusion

- The system is actively tracking ConfigMap and Secret changes in `gitops-reverser-example-audit` and pushing them to Git.
- Main issue found in this debugging session: namespace mismatch (`gitops-reverser-example-audit-namespace` does not exist; watch rule is namespace-scoped to `gitops-reverser-example-audit`).
- Secondary issue found: startup seed event-drop race exists in logs and may affect initial catch-up behavior at startup.

---

### 7) Root cause for "committed then immediately removed" (`oeps3`)

#### 7.1 Reproduction evidence from logs

Command:
```bash
kubectl -n gitops-reverser logs deploy/gitops-reverser --since=90m | rg -n 'oeps3|Starting git commit and push|Deleted file from repository|Created commit'
```

Observed sequence:
- `08:21:02` -> `Created commit` `operation:"CREATE"` `resource:"v1/configmaps/oeps3"`
- `08:21:10` -> `Deleted file from repository` `file:"live-cluster/v1/configmaps/gitops-reverser-example-audit/oeps3.yaml"`
- `08:21:10` -> `Created commit` `operation:"DELETE"` `resource:"live-cluster/v1/configmaps/oeps3"`

At the same time, object still exists in cluster:

Command:
```bash
kubectl -n gitops-reverser-example-audit get cm oeps3 -o yaml
```

Result:
- `ConfigMap oeps3` exists (`creationTimestamp: 2026-02-26T08:20:55Z`).

#### 7.2 Why this happens (code-path analysis)

1) Repo-state reconciliation asks BranchWorker to list resources under GitTarget path:

- [`internal/watch/event_router.go`](/workspaces/gitops-reverser2/internal/watch/event_router.go:151)
  - `worker.ListResourcesInPath(gitTarget.Spec.Path)`

2) BranchWorker walks `basePath = repoPath + path`, but computes relative path against **repo root**:

- [`internal/git/branch_worker.go`](/workspaces/gitops-reverser2/internal/git/branch_worker.go:358)
- [`internal/git/branch_worker.go`](/workspaces/gitops-reverser2/internal/git/branch_worker.go:371)
  - `relPath := filepath.Rel(repoPath, path)`

This means files are parsed like:
- `live-cluster/v1/configmaps/gitops-reverser-example-audit/oeps3.yaml`

3) Path parser interprets first segment as API group for 5-part paths:

- [`internal/git/helpers.go`](/workspaces/gitops-reverser2/internal/git/helpers.go:97)
  - For 5 parts, sets `group = parts[0]`.

So parsed identifier becomes effectively:
- `group=live-cluster, version=v1, resource=configmaps, namespace=gitops-reverser-example-audit, name=oeps3`

instead of core group (`group=""`).

4) FolderReconciler compares cluster identifiers vs git identifiers and emits deletes for git-only items:

- [`internal/reconcile/folder_reconciler.go`](/workspaces/gitops-reverser2/internal/reconcile/folder_reconciler.go:126)
- [`internal/reconcile/folder_reconciler.go`](/workspaces/gitops-reverser2/internal/reconcile/folder_reconciler.go:145)

Because keys mismatch (`"/v1/..."` vs `"live-cluster/v1/..."`), it emits DELETE.

5) Event stream behavior makes this asymmetrical:

- [`internal/reconcile/git_target_event_stream.go`](/workspaces/gitops-reverser2/internal/reconcile/git_target_event_stream.go:161)
  - Events with `Object=nil` are skipped **except DELETE**.

So reconcile-created CREATE events are dropped, but reconcile-created DELETE events are processed, causing file removal commits.

#### 7.3 Additional corroboration

Command:
```bash
kubectl -n gitops-reverser exec deploy/gitops-reverser -- sh -c 'find /tmp/gitops-reverser-workers/gitops-reverser-example-audit/your-repo/test-gitops-reverser/repos/54d901857c780feacd94c0da35957f7a/live-cluster -type f | sort'
```

Result at capture time:
- `live-cluster/v1/configmaps/...` file for `oeps3` was absent in repo cache
- `oeps3` still existed in Kubernetes

This confirms Git side deletion without cluster deletion.

