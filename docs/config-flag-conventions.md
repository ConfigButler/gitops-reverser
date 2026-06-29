# Config flag conventions

How we name and document the controller's command-line flags. The goal is the
clarity Flux is known for: a reader should understand what a flag does, what its
two ends mean, and what happens by default — **from the flag's help text alone**.

These conventions are inspired by [Flux](https://github.com/fluxcd/flux2) — its
flag surface is the model we keep returning to — but they aren't a copy of it.
Several of the rules here are our own synthesis (the byte-size model, the
security-inversion exception, the `insecure` suffix-vs-prefix split); Flux gave us
the starting point and the taste, the rest is the judgement we layered on top. The
companion document [configuration.md](./configuration.md) is the catalogue of the
flags themselves; this document is the *rulebook* for adding or renaming one.

## The seven rules

### 1. Name the flag for what it controls, never how it's implemented

The flag is a contract with the operator. It should survive an internal rewrite.
A datastore that happens to also hold audit facts is still a datastore — call it
`--redis-addr`, not `--audit-redis-addr`. If the implementation changes, the name
shouldn't have to.

The same trap catches *feature* flags: name them for the goal, not the wire they
read it off. The author-attribution feature's goal is naming the real **author**
of each change; the audit webhook is merely how the facts arrive. So it's
`--author-attribution`, not `--audit-attribution` — and the name then matches the
code's own vocabulary (`AuthorResolver`, `AuthorLookup`, `AuthorFact`).

### 2. No `-enabled`/`-disabled` suffix, no `enable-`/`disable-` prefix

Flux booleans are terse noun- or verb-phrases. The name says *what the thing is*;
the help text says *what turning it on or off does*. This keeps names short and
puts the explanation where there's room for it.

Authentic Flux examples:

```go
// fluxcd/flux2 — bare noun/verb phrases, never "-enabled"
BoolVar(&args.watchAllNamespaces, "watch-all-namespaces", true,
    "watch for custom resources in all namespaces, if set to false it will only "+
    "watch the namespace where the Flux controllers are installed")
BoolVar(&args.tokenAuth, "token-auth", false,
    "when enabled, the personal access token will be used instead of the SSH deploy key")
BoolVar(&args.networkPolicy, "network-policy", true,
    "setup Kubernetes network policies to deny ingress access to the Flux controllers")
BoolVar(&args.recurseSubmodules, "recurse-submodules", false,
    "when enabled, configures the GitRepository source to initialize and include Git submodules")
BoolVar(&args.force, "force", false,
    "override existing Flux installation if it's managed by a different tool such as Helm")
```

Note `token-auth` and `network-policy`: a bare noun *is* an acceptable boolean
name when the help text disambiguates. `--author-attribution` is exactly this
shape — the same as `--token-auth`.

> **The one tolerated exception:** `--enable-http2` is scaffolded by Kubebuilder
> and is a well-known controller-runtime flag (`--enable-leader-election` is its
> sibling). Renaming it buys little and surprises people who know the ecosystem.
> If you want full purity, `--http2` is the conventional name; otherwise leave
> `--enable-http2` and treat it as the documented exception.

### 3. Polarity and default live in the help text — for *both* ends

Every boolean's usage string states what `true` does, what `false` does, and
which is the default. Flux's `watch-all-namespaces` text is the template: *"watch
… in all namespaces, if set to false it will only watch the namespace where the
controllers are installed."* One sentence, both ends, no ambiguity.

Template for our booleans:

```
"<what true does> (default). When false, <what false does>; <any caveat that still applies>."
```

### 4. Default to the safe / common value

Flux defaults its safety features **on** (`network-policy=true`,
`watch-all-namespaces=true`) and its irreversible or niche actions **off**
(`force=false`, `recurse-submodules=false`). Pick the default a careful operator
would want if they read nothing.

### 5. Group related flags under one consistent prefix

All flags for one component share a prefix and don't drift. If the toggle is
`--author-attribution`, the tuning knobs are `--author-attribution-ttl` and
`--author-attribution-grace` — not a bare `--attribution-ttl` next to a prefixed
`--audit-attribution-enabled`. Pick the prefix once; apply it to the whole group.

### 6. Positive phrasing; reserve `--no-<feature>` for genuine on-by-default toggles

Avoid double negatives in names and in help text. Flux controllers use
`--no-cross-namespace-refs` / `--no-remote-bases` for "on by default, here is the
off switch" — and only there, where `--no-…` reads naturally. Prefer a positive
name with a sensible default over a negative one wherever you can.

> **Security is the sanctioned exception — name the insecure deviation.** When the
> secure behaviour is the right default, invert the polarity: name the *insecure*
> opt-out, not the secure opt-in. So `--redis-insecure` (default off), never
> `--redis-tls` (default off). Disabling transport security is an opt-out of the
> right way to do things, and an opt-out of *security* in particular deserves to be
> loud and explicit — the flag carries the word `insecure` and the default stays
> secure. This is the Flux way (`--insecure-allow-http`,
> `--insecure-allow-missing-known-hosts`); it is the one place to deliberately reach
> for a negative name.
>
> There are two `insecure` shapes. A per-component transport toggle takes the
> `-insecure` *suffix* so it groups under its component (rule 5) —
> `--metrics-insecure`, `--audit-insecure`, `--redis-insecure`, each secure by
> default. A genuinely dangerous, dev-only escape hatch takes the loud `insecure-`
> *prefix* — `--insecure-allow-missing-known-hosts`. Same safe-by-default
> principle; suffix vs prefix signals "this component's transport" vs "scary
> one-off".

### 7. Validate early; fail with an actionable hint

Parse, then validate, then fail before the manager starts — with a message that
tells the operator how to fix it, not just what's wrong. Flux and our own
`validateAuditConfig` both do this. A required dependency that's missing should
say so *and* explain what it is for:

```
redis-addr is required: Valkey/Redis holds each GitTarget's watch resume cursors
```

## Boolean help-text checklist

Before merging a new or renamed boolean flag, confirm:

- [ ] Name is a terse noun/verb phrase — no `-enabled`/`-disabled`, no `enable-`/`disable-` (except the documented `enable-http2`).
- [ ] Help text says what `true` does **and** what `false` does.
- [ ] Help text states the default explicitly.
- [ ] Default is the value a careful operator would pick blind.
- [ ] Flag shares its prefix with the rest of its component's flags.
- [ ] Any required-dependency caveat that survives the "off" state is spelled out (e.g. "Redis is still required").

## Numbers, durations, and sizes — five more rules

The seven rules above were written with booleans in mind, but most of them carry
over: name the thing (1), default to the safe value (4), share a prefix (5),
validate early (7). Quantities add one question a boolean never has: **where does
the unit live — in the type, or in the name?** Flux answers it consistently.

### 8. Let the type carry the unit; let the name carry the role

A `time.Duration` flag already parses `5s`, `2m`, `3h` — so the *type* knows the
unit and the *name* is free to say what the duration is *for*. Flux never writes
`--timeout-seconds`; it writes `--timeout`. The role noun does the naming:

| Role | Flux flag | Default (as written in code) |
| --- | --- | --- |
| recurring period | `--interval` | `time.Minute` |
| deadline for one operation | `--timeout`, `--health-check-timeout` | `5*time.Minute`, `2*time.Minute` |
| wait before a retry | `--retry-interval`, `--retry-delay`, `--min-retry-delay`, `--max-retry-delay` | `0`, `10*time.Second`, `750*time.Millisecond`, `15*time.Minute` |
| lease / renew window | `--leader-election-lease-duration`, `--leader-election-renew-deadline`, `--leader-election-retry-period` | `35s`, `30s`, `5s` |
| poll cadence | `--poll-interval` | `5*time.Second` |

Defaults are written in human units (`35*time.Second`, not `35000000000`) so the
source reads honestly and the help text can quote the same value. Vocabulary,
smallest scope to largest: **`-delay`** (wait before one retry) → **`-interval`**
/ **`-period`** (recurring cadence) → **`-timeout`** / **`-deadline`** (give-up
point) → **`-duration`** (a span you set directly, like a lease). Match the role;
don't invent `-time`, `-wait`, or `-ms`.

### 9. When the type has no native unit, put the unit in the name

A rate, a percentage, or a raw byte count has no self-describing type the way
`time.Duration` does — so the unit moves **into the name**:

```go
// fluxcd/pkg/runtime — rate: "-qps", because Go has no rate type to carry it
Float32Var(&o.QPS, "kube-api-qps", 50.0,
    "The maximum queries-per-second of requests sent to the Kubernetes API.")
IntVar(&o.Burst, "kube-api-burst", 300,
    "The maximum burst queries-per-second of requests sent to the Kubernetes API.")
// percentage: "-percentage", and the help text shows the math and the bounds
Uint8Var(&o.Percentage, "interval-jitter-percentage", 5,
    "Percentage of jitter to apply to interval durations. A value of 10 will apply "+
    "a jitter of +/-10% to the interval duration. It cannot be negative, and must be less than 100.")
```

A plain *count* needs no unit — the noun is the unit: `--kube-api-burst`,
`--retries` (default 10), the controllers' `--concurrent`. Plural or noun, never
`-count`.

### 10. Byte sizes: pick a model, then make the name match it

There are two honest ways to take a size, and the *name must not lie about which*:

- **Raw integer bytes** → suffix `-bytes`, help text says "in bytes". This is our
  `--audit-max-request-body-bytes` (`Int64`).
- **Kubernetes resource quantity** (`8Mi`, `1Gi`) → the value is *not* a byte
  count, so do **not** suffix `-bytes`; name it `-size` (or `-max-…`) and say
  "Kubernetes resource quantity" in the help.

`8Mi` accepted by a flag named `--…-bytes` is a contradiction the reader has to
decode, so a quantity flag is named `-size` (e.g. `--branch-buffer-max-size`) and a
true byte count keeps `-bytes` (e.g. `--audit-max-request-body-bytes`, `Int64`).

### 11. Pair bounds with `min-`/`max-`; fold a port into its address

A floor and a ceiling on the same quantity share the stem under a `min-`/`max-`
prefix — `--min-retry-delay` / `--max-retry-delay` — not `-floor`/`-ceiling` or
`-low`/`-high`. And a port is half of an address: Flux exposes one
`--metrics-bind-address` (`host:port`), not `--metrics-host` plus
`--metrics-port`. Model "where to listen" once, per server: one
`--audit-bind-address`, not a separate `--audit-port`.

### 12. State the unit, the range, and what the extremes mean

Rule 3 ("polarity and default in the help text") becomes, for a number: the
**unit/format**, the **default**, and the **meaning of any sentinel**. Flux is
explicit that `0` is special and that bounds exist:

- `--scan-timeout` default `0`: *"a timeout for scanning; this defaults to the interval if not set."*
- `--interval-jitter-percentage`: *"must be less than 100"* — and it is rejected if it isn't (rule 7).
- operator-facing durations add the format hint *"(duration string)"*; the
  user-typed CLI `--since` spells it out: *"a relative duration like 5s, 2m, or 3h."*

### Number / duration help-text checklist

- [ ] Duration flags use `time.Duration` and a role noun (`-timeout`/`-interval`/`-delay`/`-period`/`-deadline`) — no `-seconds`/`-ms` suffix.
- [ ] Rates, percentages, and raw-byte counts carry the unit in the name (`-qps`, `-percentage`, `-bytes`); plain counts don't.
- [ ] A `-bytes` flag takes integer bytes; a quantity-string flag (`8Mi`) is named `-size`, not `-bytes`.
- [ ] Paired bounds use `min-`/`max-`; a port is folded into a `host:port` `-bind-address`.
- [ ] Help text states the unit/format, the default, and what `0` (or any sentinel) means; out-of-range values are rejected early (rule 7).
- [ ] The default is written in human units in code (`35*time.Second`, `8Mi`), not a raw literal.
