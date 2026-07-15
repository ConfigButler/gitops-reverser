# Kustomize never emits a `${...}` token, verified from source

> **reference** — durable background. Index: [`../INDEX.md`](../INDEX.md)
>
> Established by **reading the kustomize source code** in `external-sources/kustomize` at commit
> `6a1560da2` (`git describe`: `kustomize/v5.8.1-62-g6a1560da2`), plus executing the
> token regex against a table of vectors. These are source-derived + test-verified
> facts, not observations from a live cluster.

## Why this matters

The render-fidelity fence ([`RenderMatchesLive`](../design/support-boundary/render-fidelity.md)) uses a
`${...}` token as the lever to spot a value **our render did not produce**. Two facts make it reliable:

- kustomize can neither create nor resolve a `${...}` token (facts 1–2 below), so any `${...}` left in
  a render **output** was carried in verbatim from an *input* — a source document, or the kustomization
  itself (e.g. a `patches:` block) — never minted by kustomize.
- So if a rendered field still holds a token but the **live** value there has *diverged*, our render
  did not produce that live value. **What did is not knowable here — Flux `postBuild`, a direct live
  edit, an admission mutation, another controller — and it does not need to be:** we cannot reproduce
  the live value from the source, so the honest action is to refuse (`RenderDoesNotMatchLive`), not to
  guess a cause. ("Must have been substituted" would be the guess.)

**The fence must read the render output, not the source.** kustomize does not *preserve* every
token-bearing source field: a supported `labels` / `commonLabels` transform overwrites
`metadata.labels[...]` wholesale via `SetEntry` (`api/filters/labels/labels.go`). So a source
`metadata.labels.env: ${ENV}` under `labels: {env: prod}` renders to `env: prod` — no token, equal to
a live `prod`. Comparing the *source* to live would falsely refuse that faithful folder; comparing the
*render* (`dm.Rendered.Object`, or the Git document itself for a plain manifest) does not.

## The three facts

1. **Kustomize's own variable syntax is `$(...)`, not `${...}`.** The variable engine
   `api/filters/refvar/expand.go` (under `external-sources/kustomize`)
   is hard-coded to `operator='$'`, `referenceOpener='('`, `referenceCloser=')'`. In
   `tryReadVariableName`, a `$` followed by `{` hits the `default` branch and is returned
   **verbatim** (`isVar=false`). So kustomize never *parses or resolves* `${...}` — it
   passes it straight through. There is also **no** `os.Expand` / `os.ExpandEnv` on
   resource content anywhere in `api`/`plugin`/`kyaml`.

2. **Kustomize never *originates* a `${...}` token.** A sweep of `api` + `plugin` +
   `kyaml` for any string literal, raw string, or `Sprintf` building `${` in non-test,
   non-comment Go returns **zero** hits (the only `${...}` in the tree are code comments
   describing plugin-path templates). Every builtin generator/transformer is a
   concat/copy operation, not a templating engine. `HelmChartInflationGenerator` merely
   `exec.Command`s the `helm` binary, so any `${...}` in its output is chart-authored
   source content.

3. **Read parsed field *values*, not raw bytes.** Parsing removes comments, so a
   `${var}` in `# commentary` never enters the predicate. A CRD schema `description`
   is a real parsed scalar and **must** enter it: the literal `${var:=default}` that
   broke CRD mirroring is safe because the rendered and live descriptions are equal,
   not because descriptions receive a structural exemption. The live comparison, not
   a field-name exception, avoids that false positive.

## Token regex — test results

Regex: `\$\{[A-Za-z0-9_.][^}]*\}`, executed against these vectors:

| String | Matches | Expected | Note |
|---|---|---|---|
| `${cluster_domain}` | ✅ | ✅ | plain token |
| `${schema.spec.replicas}` | ✅ | ✅ | dotted path |
| `${var:=default}` | ✅ | ✅ | Flux default-value syntax — caught |
| `prefix-${x}-suffix` | ✅ | ✅ | token mid-string |
| `$(POD_IP)` | ❌ | ❌ | parens, not braces |
| `$(kustomize_leftover_var)` | ❌ | ❌ | unresolved kustomize var — correctly ignored |
| `${}` | ❌ | ❌ | empty |
| `${ spaced }` | ❌ | ❌ | leading space not allowed |
| `# a comment with ${var}` | ✅ | — | regex alone can't tell; fact 3 handles it |

All rows matched expectation.

## One nuance

`$(POD_IP)` is not *only* "native Kubernetes env expansion" — `$(...)` is **also**
kustomize's own vars syntax, and an unresolved kustomize var is re-emitted as `$(FOO)`
(via `MakePrimitiveReplacer` → `syntaxWrap`). This strengthens the fence rather than
weakening it: a brace-only regex ignores leftover kustomize vars too, so it will not
false-positive on them.
