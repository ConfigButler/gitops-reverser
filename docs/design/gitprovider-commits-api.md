# GitProvider `commit` block — API design

## Status

Design proposal. No implementation yet. Precedes and supersedes the committer-identity section of
[commit-signing-design.md](commit-signing-design.md).

**Field name decision:** `commit` (singular), mirroring the existing `push` block. Both are
verb-as-noun names for the action being configured. `spec.push` / `spec.commit` is the natural
parallel. Alternatives considered: `commits` (plural feels like a list), `commitPolicy` (implies
enforcement), `commitInstructions` (too long), `commitStyle` (doesn't cover signing).

---

## Motivation

Commit-related configuration in `GitProvider` currently spans two immediate concerns, neither of
which are configurable today:

1. **Committer identity** — who the operator appears as in Git history.
2. **Commit message** — the string written as the commit subject.

Author-email configuration is intentionally left out of this proposal for now. It can be revisited
later once the core `commit` block settles.

Commit signing requires the committer email to be a verified address on a git platform account —
making committer identity a user-facing concern for the first time. But the identity and message
questions are independent of signing. Introduce a single `commit` block on `GitProviderSpec` that
owns all commit-level configuration; signing is one sub-field.

---

## What is hardcoded today (and why it matters)

### Committer identity — hardcoded inconsistently in two places

`generateAtomicBatchCommit` ([git.go:828](../../internal/git/git.go#L828)):
```go
Author:    &object.Signature{Name: "gitops-reverser",  Email: "noreply@configbutler.ai"}
Committer: &object.Signature{Name: "gitops-reverser",  Email: "noreply@configbutler.ai"}
```

`createCommitForEvent` ([git.go:1052](../../internal/git/git.go#L1052)):
```go
Author:    &object.Signature{Name: event.UserInfo.Username, Email: ConstructSafeEmail(...)}
Committer: &object.Signature{Name: "GitOps Reverser",  Email: "noreply@configbutler.ai"}
```

**Bug**: committer name is `"gitops-reverser"` in the atomic path and `"GitOps Reverser"` in the
per-event path. Same operator, two different identities in the same Git log. Fixed as a side effect
of this change.

### Commit message — two separate code paths

Per-event commits call `GetCommitMessage(event)` ([git.go:396](../../internal/git/git.go#L396)):
```
[CREATE] apps/v1/deployments/my-deploy
[UPDATE] v1/configmaps/kube-system/my-config
[DELETE] v1/namespaces/my-namespace
```

Atomic batch commits (reconcile snapshots) use `request.CommitMessage`, set by
[folder_reconciler.go:202](../../internal/reconcile/folder_reconciler.go#L202):
```
reconcile: sync 47 resources from cluster snapshot
```

Both messages are hardcoded. Neither is overridable today.

---

## Author vs Committer — why the distinction matters

| Field | Meaning in gitops-reverser |
|---|---|
| **Author** | Who made the change. Per-event: the Kubernetes user. Batch: the operator itself. |
| **Committer** | Who wrote the commit object. Always the operator. |

This is load-bearing: `git log --author="alice"` finds everything Alice changed in the cluster.
`git log --committer="gitops-reverser"` finds everything the operator wrote. They're different
queries, and merging the two roles would break the audit trail.

Signing binds to the **Committer** identity — the platform checks that the committer email is
verified on the account that owns the signing key. The Author email is not checked.

---

## Commit message templates

### Why templates, not just a prefix

A `prefix` field (`chore:`) handles the most common case but breaks down quickly:
- It can't include the username (`[alice] [CREATE] ...`)
- It can't restructure the message (`CREATE deployment/my-deploy in default`)
- It can't produce different formats for different operations

Go's `text/template` covers all of these at the cost of a slightly longer YAML value. Most users
only need the simple cases; the template syntax makes those just as readable.

### Two template contexts

The two commit paths have different data available, so they have separate template fields with
separate context objects.

#### Per-event template context: `CommitMessageData`

```go
type CommitMessageData struct {
    Operation  string // "CREATE", "UPDATE", "DELETE"
    Group      string // API group, e.g. "apps". Empty for core resources.
    Version    string // API version, e.g. "v1"
    Resource   string // Plural resource kind, e.g. "deployments", "configmaps"
    Namespace  string // Resource namespace. Empty for cluster-scoped resources.
    Name       string // Resource name
    APIVersion string // "apps/v1" or "v1" for core — convenient shorthand
    Username   string // Kubernetes user who triggered the change
    GitTarget  string // Name of the GitTarget that owns this event
}
```

Default template (reproduces current behaviour exactly):
```
[{{.Operation}}] {{.APIVersion}}/{{.Resource}}/{{.Name}}
```

#### Batch template context: `BatchCommitMessageData`

Atomic batch commits cover multiple resources, so per-resource fields are not available.
This context is intentionally small for the first version and can grow later if new batch modes need
more summary data.

```go
type BatchCommitMessageData struct {
    Count     int    // Number of events/resources in this commit
    GitTarget string // Name of the GitTarget
}
```

Default template (reproduces current behaviour exactly):
```
reconcile: sync {{.Count}} resources
```

### `CommitMessageSpec`

```go
// CommitMessageSpec configures commit message formatting.
type CommitMessageSpec struct {
    // Template is a Go text/template string for per-event commit messages.
    // Available variables: see CommitMessageData.
    // Default: "[{{.Operation}}] {{.APIVersion}}/{{.Resource}}/{{.Name}}"
    // +optional
    Template string `json:"template,omitempty"`

    // BatchTemplate is a Go text/template string for atomic batch commit messages
    // (e.g. reconcile snapshots). Available variables: see BatchCommitMessageData.
    // Default: "reconcile: sync {{.Count}} resources"
    // +optional
    BatchTemplate string `json:"batchTemplate,omitempty"`
}
```

---

## Full proposed API

```go
type CommitSpec struct {
    // Committer configures the operator's bot identity in Git history.
    // When signing is enabled, Email must be a verified address on the account
    // that owns the signing key.
    // +optional
    Committer *CommitterSpec `json:"committer,omitempty"`

    // Message configures the commit message format.
    // +optional
    Message *CommitMessageSpec `json:"message,omitempty"`

    // Signing configures commit signing.
    // +optional
    Signing *CommitSigningSpec `json:"signing,omitempty"`
}

type CommitterSpec struct {
    // +optional
    // +kubebuilder:default="GitOps Reverser"
    Name string `json:"name,omitempty"`

    // +optional
    // +kubebuilder:default="noreply@configbutler.ai"
    Email string `json:"email,omitempty"`
}

type CommitMessageSpec struct {
    // +optional
    Template string `json:"template,omitempty"`
    // +optional
    BatchTemplate string `json:"batchTemplate,omitempty"`
}
```

Add to `GitProviderSpec`:
```go
// +optional
Commit *CommitSpec `json:"commit,omitempty"`
```

Add to `GitProviderStatus`:
```go
// SigningPublicKey is the operator's SSH signing public key in authorized_keys format.
// Register this as a Signing Key on your git platform.
// Only populated when commit.signing is configured.
// +optional
SigningPublicKey string `json:"signingPublicKey,omitempty"`
```

---

## YAML examples

### No configuration — defaults only

Nothing changes from today. The `commit` block is entirely optional.

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: GitProvider
metadata:
  name: my-provider
  namespace: gitops-system
spec:
  url: "git@github.com:my-org/k8s-audit.git"
  allowedBranches: ["main"]
  secretRef:
    name: git-creds
```

Produces commits like:
```
[CREATE] apps/v1/deployments/my-deploy
[UPDATE] v1/configmaps/kube-system/my-config
```

---

### Conventional commits — simple case

```yaml
spec:
  commit:
    message:
      template: "chore: [{{.Operation}}] {{.APIVersion}}/{{.Resource}}/{{.Name}}"
```

Produces:
```
chore: [CREATE] apps/v1/deployments/my-deploy
chore: [UPDATE] v1/configmaps/kube-system/my-config
```

---

### Conventional commits — operation-aware

Different conventional type per operation, following the spirit of the spec.

```yaml
spec:
  commit:
    message:
      template: >-
        {{if eq .Operation "DELETE"}}fix{{else}}chore{{end}}:
        [{{.Operation}}] {{.APIVersion}}/{{.Resource}}/{{.Name}}
```

Produces:
```
chore: [CREATE] apps/v1/deployments/my-deploy
fix: [DELETE] v1/configmaps/stale-config
```

---

### Include the Kubernetes username

Useful when reviewing history and wanting to see who made the cluster change at a glance,
without opening each commit.

```yaml
spec:
  commit:
    message:
      template: "[{{.Operation}}] {{.Resource}}/{{.Name}} ({{.Username}})"
```

Produces:
```
[UPDATE] deployments/my-deploy (alice)
[DELETE] configmaps/old-config (bob@company.com)
```

---

### Namespace-aware messages

The default format omits namespace. This makes it explicit:

```yaml
spec:
  commit:
    message:
      template: >-
        [{{.Operation}}] {{.APIVersion}}/{{.Resource}}/
        {{- if .Namespace}}{{.Namespace}}/{{end}}{{.Name}}
```

Produces:
```
[CREATE] apps/v1/deployments/production/my-deploy
[CREATE] v1/namespaces/staging               ← cluster-scoped, no namespace segment
```

---

### Team-tagged commits

Useful when multiple teams share a single audit repo and want to filter by team in `git log`.

```yaml
spec:
  commit:
    message:
      template: "[platform] [{{.Operation}}] {{.APIVersion}}/{{.Resource}}/{{.Name}}"
```

Produces:
```
[platform] [UPDATE] apps/v1/deployments/api-server
```

---

### Simplified — just resource/name, no API version noise

Some teams find the full `apps/v1/deployments/` prefix noisy for everyday review.

```yaml
spec:
  commit:
    message:
      template: "{{.Operation}}: {{.Resource}}/{{.Namespace}}/{{.Name}}"
```

Produces:
```
CREATE: deployments/production/api-server
UPDATE: configmaps/kube-system/aws-auth
DELETE: pods/staging/crashed-worker-7f4b2
```

---

### Custom batch message

The reconcile snapshot commit is a different code path with a different template.

```yaml
spec:
  commit:
    message:
      batchTemplate: "chore: snapshot sync ({{.Count}} resources)"
```

Produces (reconcile snapshot commits):
```
chore: snapshot sync (347 resources)
```

---

### Signed commits with auto-generated key — GitHub

```yaml
spec:
  url: "git@github.com:my-org/k8s-audit.git"
  allowedBranches: ["main"]
  secretRef:
    name: git-creds
  commit:
    committer:
      name: "GitOps Reverser"
      # Use the GitHub noreply address for the bot account that owns the signing key.
      # Find yours at: github.com/settings/emails (the no-reply format)
      email: "12345678+gitops-reverser-bot@users.noreply.github.com"
    signing:
      generateWhenMissing: true
      secretRef:
        name: gitops-reverser-signing-key
```

After `kubectl apply`, run:
```
kubectl get gitprovider my-provider -o jsonpath='{.status.signingPublicKey}'
```
Paste that key into GitHub → Settings → SSH and GPG Keys → New SSH Key (type: Signing Key).

---

### Signed commits with auto-generated key — GitLab

```yaml
spec:
  commit:
    committer:
      name: "GitOps Reverser"
      email: "gitops-reverser-bot@noreply.gitlab.com"
    signing:
      generateWhenMissing: true
      secretRef:
        name: gitops-reverser-signing-key
```

---

### Everything together

```yaml
spec:
  url: "git@github.com:my-org/k8s-audit.git"
  allowedBranches: ["main"]
  secretRef:
    name: git-creds
  push:
    interval: "10s"
    maxCommits: 20
  commit:
    committer:
      name: "GitOps Reverser"
      email: "12345678+gitops-reverser-bot@users.noreply.github.com"
    message:
      template: "chore: [{{.Operation}}] {{.APIVersion}}/{{.Resource}}/{{.Name}} ({{.Username}})"
      batchTemplate: "chore: snapshot sync ({{.Count}} resources)"
    signing:
      generateWhenMissing: true
      secretRef:
        name: gitops-reverser-signing-key
```

Per-event commits look like:
```
chore: [UPDATE] apps/v1/deployments/my-deploy (alice)
```

Reconcile snapshot commits look like:
```
chore: snapshot sync (347 resources)
```

Author on per-event commits: derived from the Kubernetes username using the existing implementation
Committer on all commits: `GitOps Reverser <12345678+gitops-reverser-bot@users.noreply.github.com>` ✓ Verified

---

## Backwards compatibility

All fields are optional. Defaults reproduce current exact behaviour:

| Current hardcoded value | Default in new API |
|---|---|
| Committer name `"gitops-reverser"` / `"GitOps Reverser"` (inconsistent) | `"GitOps Reverser"` (consistent) |
| Committer email `"noreply@configbutler.ai"` | `"noreply@configbutler.ai"` |
| Per-event message `[CREATE] apps/v1/deployments/name` | same — default template produces identical output |
| Batch message `reconcile: sync N resources from cluster snapshot` | `reconcile: sync N resources` — intentionally slightly shorter |

A `GitProvider` with no `commit` block behaves exactly as before, except the committer name
inconsistency is fixed.

---

## What deliberately stays out of this block

| Concern | Where it lives | Rationale |
|---|---|---|
| Push interval / batching | `spec.push` | Delivery strategy, not commit identity |
| Branch allowlist | `spec.allowedBranches` | Access control |
| Encryption | `spec.encryption` (GitTarget) | Per-target, not per-commit |
| Author **name** | Kubernetes username from audit event (immutable) | Must not be overridable — it is the audit record |
| Author email mapping | deferred / future work | Keep the initial `commit` block smaller and focused |

The Author name is intentionally not configurable. It is the Kubernetes username. Allowing overrides
would mean the Git history no longer tells you who actually changed the resource.

---

## Implementation notes

### Template evaluation

Templates are validated at reconcile time on the `GitProvider`, not at commit time. A malformed
template sets `Ready=False` with a clear message rather than failing silently at write time.

Semantics:

- `template: null` or an omitted field means "use the default template"
- `template: ""` means "render an empty commit message"

That second case is technically possible in git/go-git, so it should not silently fall back to the
default.

```go
func validateTemplate(tmplStr string, data any) error {
    tmpl, err := template.New("").Parse(tmplStr)
    if err != nil {
        return fmt.Errorf("invalid template: %w", err)
    }
    // Dry-run render to catch type errors early.
    return tmpl.Execute(io.Discard, data)
}
```

### Where templates are resolved

`GetCommitMessage(event Event)` at [git.go:396](../../internal/git/git.go#L396) becomes:

```go
func GetCommitMessage(event Event, tmpl *template.Template) string {
    if tmpl == nil {
        // Default — reproduces existing behaviour.
        return fmt.Sprintf("[%s] %s", event.Operation, event.Identifier.String())
    }
    data := CommitMessageData{
        Operation:  event.Operation,
        Group:      event.Identifier.Group,
        Version:    event.Identifier.Version,
        Resource:   event.Identifier.Resource,
        Namespace:  event.Identifier.Namespace,
        Name:       event.Identifier.Name,
        APIVersion: apiVersion(event.Identifier),
        Username:   event.UserInfo.Username,
        GitTarget:  event.GitTargetName,
    }
    var buf strings.Builder
    _ = tmpl.Execute(&buf, data) // validated at reconcile time; errors here are unexpected
    return buf.String()
}
```

The compiled `*template.Template` is resolved once per `commitAndPushRequest` call (read from the
provider spec, parsed and cached on the `BranchWorker` or passed in via `WriteRequest`), not
re-parsed per commit.

### Deferred

- **Per-GitTarget message override**: not planned. Commit messages are a provider-level concern
  consistent with the rest of the `commit` block.
- **GPG signing**: add `signingFormat: gpg` to `CommitSigningSpec` when needed. See
  [commit-signing-design.md](commit-signing-design.md).
