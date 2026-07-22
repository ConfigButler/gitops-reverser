# Documentation style guide

How markdown in this repository is written. Most of it is descriptive: it writes down conventions
the docs already follow. Three sections are prescriptive, because the repo is currently
inconsistent and needed a decision: [punctuation](#punctuation-no-em-dashes),
[spelling](#spelling-american-english), and [words to cut](#words-to-cut).

[`architecture.md`](architecture.md) and [`configuration.md`](configuration.md) are the reference
models for depth, structure, and tone. They predate this guide, so they still carry the habits
listed below as things to fix. Follow the guide, not their punctuation.

Applies to every tracked `.md` file: product docs, design records, chart READMEs, and
[`AGENTS.md`](../AGENTS.md).

## Voice

**Write to an operator who is competent but new to this product.** They know Kubernetes. They do
not know why a `ClusterProvider` is cluster-scoped. Explain the second thing, never the first.

**State what the software does, in present tense, active voice.** Not what it aims to do, is
designed to do, or will do. A doc describes behavior that exists; a design record under
[`docs/design/`](design/) is where intent belongs, and it says so explicitly.

**Give the reason when the reason is not obvious.** This is the house signature and the main thing
that makes these docs worth reading. `configuration.md` does not just say `ClusterProvider` is
cluster-scoped; it has a section called "Why the two provider types have different scopes". Keep
doing that. A rule with no rationale gets argued with later, or silently reverted.

**Do not sell.** No `powerful`, `seamless`, `robust`, `simply`, `effortlessly`. The docs currently
contain 15 uses of `powerful` and 7 of `robust`; none of them carry information. If a capability
is good, describing it precisely is the argument.

Backticks around those words are deliberate, and they are the general rule for naming a word rather
than using it. Vale exempts backticked text and fenced blocks, and nothing else, so a banned word
quoted in bare quotation marks flags itself.

**Prefer the concrete failure to the abstract reassurance.** "SSH host-key verification fails
closed, so the `known_hosts` line is required" beats "credentials are handled securely".

**Second person for instructions, third for behavior.** "Create a Secret" for something the reader
does. "The operator retries with fetch, reset, and replay" for something it does. Avoid "we"
outside design records, where it is fine because a design record is an argument someone is making.

## Punctuation: no em dashes

**Do not use `—` in prose.** This is the one hard rule in this guide.

The repo has 3,691 of them, 23 in the root README. They are load-bearing nowhere. Every one has a
better replacement, and the habit is the single strongest reason the docs read as machine-written.

The problem is not the character. It is that one mark got used for six unrelated jobs, so the
prose lost the distinction between an aside, a definition, a correction, and a new sentence.
Picking the right mark restores that.

### The replacement patterns

Match the mark to the job. Do not reflexively substitute a comma; that is how you get comma
splices.

**Aside or example → parentheses.**

```text
before:  the author becomes the real Kubernetes actor — user, service account, or CI identity —
         while the committer never moves
after:   the author becomes the real Kubernetes actor (user, service account, or CI identity),
         while the committer never moves
```

**Definition or expansion → colon.**

```text
before:  Two conditions stop the data plane and are worth recognizing — ClusterProviderNotFound
         and NamespaceNotAuthorized
after:   Two conditions stop the data plane and are worth recognizing:
         ClusterProviderNotFound and NamespaceNotAuthorized
```

**Two independent clauses → a period, or a semicolon when they are genuinely one thought.**

```text
before:  It refuses by name — before writing anything, reported as Stalled=True — generators,
         components, namePrefix/nameSuffix, ...
after:   It refuses these by name, before writing anything, and reports Stalled=True: generators,
         components, namePrefix/nameSuffix, ...
```

That last example is the worst shape: a 7-word interruption between the verb and its object. Split
it even if you keep a dash elsewhere.

**Bullet label → colon inside the bold.**

```text
before:  - **High availability** — `replicaCount > 1` is rejected today; needs leader coordination
after:   - **High availability:** `replicaCount > 1` is rejected today. It needs leader coordination
```

**Range → en dash, no spaces.** `0–300s`, `1–1024 chars`. This is the one dash that stays, and the
repo already uses it correctly in 64 places.

**Inside quoted output, change nothing.** If a CLI, an API message, or a commit body contains `—`,
reproduce it verbatim. Fenced blocks are specimens, not prose.

### The construction the dash is hiding

Removing the dash mechanically is not enough, because it usually props up a specific tic: the
corrective appositive, `X, not Y`. The repo has 325 instances of a dash or comma followed directly
by `not a` / `never the`, plus 293 of `rather than`.

```text
before:  That is a convenient, concrete reference—not a claim that `default` is always
         the local cluster.
after:   That is a convenient, concrete reference. It does not claim that `default` is
         always the local cluster.
```

The pattern is not banned. Used once, it lands. Used in every third paragraph it becomes a verbal
tic, and it is the thing readers point at when they say text "sounds like AI". Budget roughly one
per page, and prefer stating the correct thing positively over defining it by what it is not.

The same applies to the **rule of three**. "Install attempts, first-commit experience, and audit
delivery issues" is fine. Four consecutive three-item lists is a pattern the reader starts hearing
instead of reading. Vary the length: two items, then five, then one.

## Spelling: American English

**Decision: American English everywhere, with no carve-outs.** Write `behavior`, `organization`,
`defense`, `labeled`, `recognize`, `center`, and `license` for both the noun and the verb.

The prose is split today, which is why this needed a decision rather than an inference. Neither
variant is winning:

| Word | American | British |
|---|---|---|
| `behavior` / `behaviour` | 140 | 130 |
| `catalog` / `catalogue` | 219 | 7 |
| `organization` / `organisation` | 11 | 15 |
| `labeled` / `labelled` | 5 | 14 |

The tie is the point. There was no convention to discover, so the tie-break is which choice costs
less to hold. American wins that on three counts, and the first is decisive.

**The code is American, and the docs sit next to the code.** `authorization`, `sanitize`, `catalog`,
`finalize`, `materialize`, `analyzer`: those are package names, condition reasons, and API fields,
and none of them can be respelled. Kubernetes is American throughout, and so is every tool in this
stack. Choosing British for prose means the doc word and the code word differ for exactly the
vocabulary the docs use most.

**One rule replaces four.** The British version of this section needed a three-tier exemption
ladder for identifiers, a rule for the unbackticked name of a thing in the code, and a further rule
for what to do when both spellings would land in one paragraph. All of it existed to manage a
conflict that American simply does not create. If you can grep a word and land on the
implementation, that used to be a special case; now it is the normal case.

**Tooling defaults are American.** Vale ships an en_US dictionary, and the Google and Microsoft
style packages assume American. Going with the grain keeps a spelling rule to a short list of
British forms to reject, instead of a long list of American forms to swap plus a longer list of
exceptions to protect.

### -ize, not -ise

Use `-ize` where American does: `organize`, `recognize`, `initialize`, `summarize`, `realize`.

`-yze` is `-yze`, never `-yse`: `analyze`, `paralyze`.

The related forms that follow from the same choice:

- **Single L before a suffix**: `labeled`, `modeling`, `canceled`, `signaled`, `traveled`.
- **`-se` for both noun and verb**: a `license` and to `license`; `defense` and `offense` are always
  `-se`. `practice` is `-ce` for both.
- **`toward`**, not `towards` (1 to fix).
- **`while`**, not `whilst`; **`among`**, not `amongst`. Both hold in either variant, and the repo
  already has 254 `while` to 2 `whilst`.
- **Computing terms that never diverged stay put**: `program` for software (never `programme`),
  `disk`, `dialog` for a UI element.

### Identifiers are still verbatim

One exemption survives, and it is the one every style guide has: **anything in backticks keeps the
spelling the code uses.** `rbac.authorization.k8s.io`, `NamespaceNotAuthorized`, `internal/sanitize`,
`FinalizeFailed`, `APIResourceCatalog`, `sendInitialEvents`, `manifest-analyzer`. Proper nouns too:
the Apache License, the OpenSSF Best Practices badge.

That is not really a spelling rule. It is the rule that you do not edit a string that has to match
something. It also costs nothing to enforce, because Vale skips backticked text and fenced blocks
by default, so an identifier never reaches a spelling check in the first place.

**The cleanup list**, British to American, highest count first: `behaviour` (130), `organisation`
(15), `labelled` (14), `modelled` (9), `catalogue` (7), `defence` (7), `modelling` (6), `recognise`
(5), `licence` (5), `favour` (5), `behavioural` (5), `recognising` (4), `behaviours` (4), and a tail
of one or two each: `whilst`, `recognised`, `centre`, `analyse`, `analysed`, `travelled`, `towards`,
`summarise`, `signalled`, `programme`, `practise`, `organisations`, `centred`, `cancelled`,
`amongst`. About 236 words across 196 files.

Fix these opportunistically when you edit a file, not in one sweeping commit that collides with
everything in flight. Nothing gates on spelling today.

**Oxford comma: yes.** The docs already use it consistently ("client, discovery surface, watch
state, and attribution partition"). Keep it.

## Words to cut

Delete the word if deleting it does not change the meaning. Current counts across `docs/` and the
README:

| Word | Count | Verdict |
|---|---|---|
| `just` | 129 | almost always cut |
| `actually` | 113 | keep only when contrasting with a stated expectation |
| `genuinely` | 54 | almost always cut |
| `simply` | 52 | always cut, it blames the reader for finding it hard |
| `powerful`, `robust` | 22 | always cut, replace with the specific property |
| `really` | 10 | cut |
| `leverage` (verb) | 3 | use "use" |

"Actually" survives in a sentence like "the source document, where the value actually lives",
because it is marking a contrast with the rendered output you might have expected. It does not
survive in "the operator actually retries", where it is filler.

Also avoid: `it's worth noting`, `keep in mind`, `in summary`, `at the end of the day`,
`delve into`, `seamlessly`. The docs are mostly clean of these already. Keep them that way.

## Structure

**Sentence case headings.** Measured: 2,098 sentence case to 140 title case. `## What it writes to
Git`, not `## What It Writes To Git`. Proper nouns and identifiers keep their case.

**One `#` per file, matching the file's job.** Then `##` sections that a reader can scan to find
the answer without reading the file.

**Lead with the answer.** Each section's first sentence should be usable on its own. Background
goes after, not before.

**Bullets plain by default.** 883 of 3,576 bullets currently lead with bold. Use a bold lead only
when it is a genuine label the reader scans for, such as a CRD field name or a mode. If every
bullet in the list is bold-led, none of them stands out and the list reads as a slide.

**Tables for verdicts and comparisons**, not for prose. The `prune.mode` table in
`configuration.md` is the right shape: three modes, three columns, one phrase per cell. If a cell
needs a sentence with a subordinate clause, the content wants to be a paragraph.

**Wrap prose at 100 columns.** `rbac.md` and `security-model.md` already sit at 97% and 92% under
100. Tables, links, and code do not wrap. This is a convention, not a lint gate.

**Blockquotes for a single warning that would otherwise derail a paragraph.** Do not stack them.

## Code, identifiers, and names

**Backtick every identifier**: field paths (`spec.prune.mode`), conditions (`Ready=True`), flags
(`--author-attribution-grace`), file names, resource kinds, namespaces, and literal values.

**Fence languages in use**, keep to this set: `yaml` (133), `bash` (129), `text` (79), `mermaid`
(75), `go` (58), `promql` (31), `console` (5), `json`.

- `bash` for commands the reader copies. No `$` prefix.
- `console` for a command *plus* its output, with a `$` prefix on the command line.
- `text` for trees, file layouts, and quoted specimens.

**Product name.** `GitOps Reverser` in prose (167 uses). `gitops-reverser` for the binary, chart,
image, namespace, and any other identifier (829 uses). Never `Gitops Reverser`, which appears zero
times today.

**Kubernetes terms** follow upstream: `kube-apiserver`, `resourceVersion`, `managedFields`,
`ClusterRole`. Not `Kube API server` or `resource version` when the identifier is meant.

## Links

**Link on first mention, then stop.** Repeating the same link four times in a page is noise.

**Forward rather than duplicate.** If another doc owns a topic, link it and write one sentence of
context. The root README should describe what the quick start achieves and link the detail; it
should not restate `configuration.md`. Duplicated prose is prose that will disagree with itself in
six months.

**Relative paths only**, resolved from the file's own directory.

**`task lint-docs` checks that every reference resolves**, including repo-relative doc paths cited
inside Go comments, which no off-the-shelf link checker looks at. Two consequences:

- It resolves through `git ls-files`, so **`git add` a new doc before running it** or the links to
  it fail.
- **Never link into `external-sources/`.** Those checkouts are gitignored and the link will not
  resolve in CI.

## Before you commit

- [ ] No `—` in prose. `grep '—' <file>`, and check anything left is inside a fenced specimen.
- [ ] No more than one `X, not Y` corrective per page.
- [ ] American spelling in prose, `-ize` not `-ise`, and identifiers untouched.
- [ ] `just`, `simply`, `genuinely`, `actually` all pass the deletion test.
- [ ] Headings are sentence case.
- [ ] New docs are `git add`ed, then `task lint-docs` passes.
- [ ] A docs-only change stops here. See [`AGENTS.md`](../AGENTS.md) for when the full validation
      suite is required instead.
