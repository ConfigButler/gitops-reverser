# Render fidelity: red-first scenarios and fixtures

> **design** — the executable examples for
> [render-fidelity.md](render-fidelity.md). They are deliberately separate from the
> layout corpus: the layout corpus has repository bytes but no corresponding live
> objects, while fidelity is a render-**vs-live** claim.

This is the acceptance suite for `RenderMatchesLive`. Implement the fixture reader and
the first failing test before the predicate or gate. A hand-written unit test that
constructs only the happy path is not an adequate substitute: the two regressions this
fence exists to prevent are both plausible-looking shortcuts.

## 1. Fixture boundary

Create a self-contained fixture suite at:

```text
internal/manifestanalyzer/testdata/render-fidelity/
  <case>/
    git/                  # the exact tracked tree; may contain kustomization.yaml
    live.yaml             # the sanitized live object(s), one or more YAML documents
    want.yaml             # condition result and the stable diagnostic field paths
```

The test renders `git/` with the same `renderRoot` path production uses; it does **not**
check in a second, hand-maintained `render.yaml`. `want.yaml` expresses only what the
test owns:

```yaml
condition: "True" # or "False"
reason: "RenderMatchesLive" # or "RenderDoesNotMatchLive"
divergences:
  - file: deployment.yaml
    field: spec.template.spec.containers[app].env[REGION].value
    token: ${REGION}
```

The field notation is diagnostic output, not an API for locating arbitrary edits. The
comparison itself walks parsed structured values and treats presence as significant:
a rendered token whose live field is absent or `null` is a divergence too.

Do **not** add these cases to `test/fixtures/gitops-layouts/`. That corpus answers
whether a Git layout is structurally supported; it cannot answer whether a particular
orchestrator output matches its render. These fixtures are a live-pair corpus.

## 2. Predicate fixtures — start here

Each row is a committed fixture directory and a table-driven subtest. The first eight
must be red before `diverges(render, live)` exists.

| Fixture | Git/render shape | Live shape | Expected | Why it is load-bearing |
|---|---|---|---|---|
| `plain-postbuild-token` | Plain Deployment env value `${REGION}` | `us-east` | `False`, one divergence | The corruption case: no kustomize document model is required. |
| `literal-crd-description` | CRD schema description `${var:=default}` | Same literal description | `True` | Regression for the reverted structural fence. A description is a parsed value, not an excluded field. |
| `literal-kro-template` | KRO template `${schema.spec.replicas}` | Same literal template | `True` | A non-CRD literal-token guardrail. |
| `literal-nginx-config` | ConfigMap data `${host}` | Same literal data | `True` | A template/config file that legitimately keeps a token. |
| `comment-only-token` | Comment contains `${REGION}`; values do not | Same object | `True` | Proves the walk reads parsed values, not YAML bytes. |
| `native-dollar-paren` | `$(POD_IP)` / unresolved `$(FOO)` value | Same or different literal value | `True` | Brace-only matching must not classify Kubernetes or kustomize vars. |
| `label-overwrites-source-token` | Source label `${ENV}`; supported `labels: {env: prod}` renders `prod` | `prod` | `True` | Proves the predicate reads the **render**, not the Git source. |
| `label-injects-render-token` | Source has no `env`; supported `labels: {env: ${ENV}}` injects it into the render | `prod` | `False`, one divergence | Proves a render-only token is found even though the source document lacks it. |
| `token-field-removed-live` | Rendered value `${REGION}` | Field absent or `null` | `False` | Presence is part of equality; a removal must not be mirrored over the token. |
| `nested-list-token` | Container `app`, env `REGION=${REGION}` | `REGION=us-east` | `False`, stable named-list path | Exercises a practical nested list and makes diagnostics usable. |

The two label cases use constructs supported today. They must not be postponed behind
the later `patches:` work: a patch that injects a token is a useful future extension,
but labels already prove the same render-not-source property.

## 3. Folder-gate state tests

The condition must be tested separately from manifest rendering. Give the state machine
a small, pure API and table-drive the following trace. A scope is `(GVR, namespace)`;
the example uses `deployments.apps/default` and `configmaps/default`.

| Step | Event | Expected condition | Writes allowed? |
|---:|---|---|---|
| 1 | Begin epoch `E1` with both scopes pending | `Unknown/Rechecking` | No |
| 2 | Deployment scope reports clean | `Unknown/Rechecking` | No |
| 3 | ConfigMap scope reports clean | `True/RenderMatchesLive` | Yes |
| 4 | Begin `E2` after a Git revision or watch-set change; discard its open target window | `Unknown/Rechecking` | No |
| 5 | Deployment scope reports a `${REGION}` divergence | `False/RenderDoesNotMatchLive` | No |
| 6 | ConfigMap scope reports clean | Still `False` | No |
| 7 | A normal live write arrives | Still `False`; no window/commit | No |
| 8 | Stale clean result from `E1` arrives | Ignored; still `False` | No |
| 9 | Begin `E3` after the Git repair; both scopes report clean | `True/RenderMatchesLive` | Yes |
| 10 | A steady-state write finds a divergence | Immediately `False` with a sample | No |

Test the zero-scope case explicitly. Once structural acceptance has passed, a target with
no active watch scopes is `True` by vacuous comparison; it cannot receive a normal live
write. This avoids leaving an otherwise idle target permanently `Unknown`.

The test names should make the safety contract obvious:

```text
TestRenderFidelityGate_RequiresEveryScopeInEpoch
TestRenderFidelityGate_IgnoresStaleEpochResult
TestRenderFidelityGate_PerWriteDivergenceClosesTarget
TestRenderFidelityGate_FullFreshEpochReopensAfterGitRepair
```

## 4. Writer and watch integration tests

After the two pure suites are red and passing, add a minimal worktree/worker test that
does all of the following in one ordered trace:

1. Initial scoped resync finds `plain-postbuild-token` and creates **no** Git commit.
2. The worker records `RenderMatchesLive=False` before it processes the next queued
   event for that target.
3. A clean-resource event queued behind the refusal cannot open a write window or
   change a file.
4. Beginning a fresh epoch discards an already-open, uncommitted window for that target;
   the resync must not finalize it before measuring the new epoch.
5. A fresh complete epoch after an incoming Git edit that removes or changes the token
   cleanly re-evaluates the current worktree and reopens writes only after every scope
   passes.

At the watch-manager layer, pin that a clean scoped resync does not overwrite another
scope's failed result. The existing `GitPathAccepted` tests are not enough: fidelity is
a separate condition and its result is a reduction over the epoch map, not the last
resync reply.

## 5. End-to-end proof

Add a dedicated e2e fixture under `test/e2e/fixtures/render-fidelity/`; do not reuse a
render-root-scoping fixture. It needs a real Flux `Kustomization` whose repository
Deployment contains `${REGION}` and whose `postBuild.substitute` resolves it to
`us-east`.

The e2e assertions are:

1. The GitTarget reaches `RenderMatchesLive=False` with reason
   `RenderDoesNotMatchLive`, `Ready=False`, and `Stalled=True`.
2. The Git file still contains `${REGION}` and no reverse-GitOps commit was created.
3. A subsequent live edit to an otherwise clean object is not mirrored while the gate
   remains false.
4. After an incoming Git revision makes the local render equal live, a complete replay
   flips the condition to `True` and normal writes resume.
5. The existing CRD-lifecycle e2e remains green: its literal `${var:=default}` schema
   description must never fail the condition.

The feature is not complete if it only passes the synthetic predicate tests. The Flux
fixture proves that the operator observes the exact extra render context the local
renderer cannot see, while the CRD lifecycle spec proves it did not revive the rejected
structural check.
