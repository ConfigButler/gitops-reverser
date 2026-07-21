# Docs linting with markdownlint-cli2 and Vale

**Proposal: add two linters that between them mechanize most of
[`style-guide.md`](../style-guide.md), and gate them differently because their backlogs differ by
an order of magnitude.**

[markdownlint-cli2](https://github.com/DavidAnson/markdownlint-cli2) checks structure: fences,
headings, lists, tables, and blank lines. [Vale](https://github.com/vale-cli/vale) checks prose:
the em dash rule, American spelling, the words to cut, and sentence-case headings. Neither replaces
`hack/doccheck`, which resolves references and is the only check that reads Go comments.

Nothing here is wired into `task lint` yet. The binaries and their configuration ship so the rules
can be exercised against real content before anything gates on them. Run either by hand:

```bash
vale docs/some-file.md
markdownlint-cli2 docs/some-file.md
markdownlint-cli2 --fix docs/some-file.md
```

**Every number below was measured**, not estimated, with the committed configuration over all 196
tracked `.md` files. `CHANGELOG.md` and `docs/finished/` are excluded by both configs, which leaves
174 files and 293,524 words actually linted. See [Exclusions](#exclusions).

## Status

| Piece | State |
|---|---|
| `markdownlint-cli2` and `vale` in the container, reachable from CI | done |
| [`.vale.ini`](../../.vale.ini) and `.vale/styles/HouseStyle/`, encoding [`style-guide.md`](../style-guide.md) | committed, gates nothing |
| [`.markdownlint-cli2.jsonc`](../../.markdownlint-cli2.jsonc) | committed, gates nothing |
| `task lint-markdown`, `task lint-prose` | not started |
| Folded into `task lint` | not started |

Both configs are live for editor extensions, which is the point: the Vale and markdownlint VS Code
extensions pick them up and mark a file as you write it. No build fails.

**The config filename matters and fails silently.** `.markdownlint.jsonc` takes bare rule keys only.
The `config` and `ignores` schema used here requires **`.markdownlint-cli2.jsonc`**; put it in the
other file and markdownlint reads none of it and reports the 12,449 defaults without a word of
complaint.

## Why two tools and not one

They do not overlap, and each is bad at the other's job.

- **markdownlint-cli2** parses markdown and knows nothing about English. It cannot see an em dash
  or a spelling, but it is the only one that can tell a fence without a language from one with it,
  or a heading with no blank line above it. Most of what it finds it also repairs.
- **Vale** parses markdown into prose and ignores code by default. It is the only one that can
  apply a rule to a sentence while leaving `rbac.authorization.k8s.io` alone, which is precisely
  the distinction [`style-guide.md`](../style-guide.md) is built on.
- **`hack/doccheck`** resolves every reference, including repo-relative doc paths cited inside Go
  comments, YAML, and shell. No off-the-shelf tool looks at those surfaces. It stays as it is.

## Where the tools live, and why Node moved

Both binaries are installed in the **`ci` stage** of
[`.devcontainer/Dockerfile`](../../.devcontainer/Dockerfile), not the `dev` stage. The `lint` job
in [`ci.yml`](../../.github/workflows/ci.yml) runs `task lint` inside that stage, so a linter
installed only in `dev` would pass locally and be missing in CI.

Vale is a single Go binary, downloaded with a checksum check like every other tool there. Its
release asset is named `vale_<version>_Linux_64-bit.tar.gz`, the one tool in this image that does
not say `amd64`.

markdownlint-cli2 publishes to npm and has no standalone binary, so it needs Node. Node.js
therefore moved from the `dev` stage to the `ci` stage, and `dev` inherits it. Nothing else
changed: `npx` is still available locally. The cost is roughly 130 MB on an image that already
carries a Go toolchain and the module cache.

Both are asserted in the tool-verification steps of the `CI base container` and
`Validate Dev Container` jobs, so a failed install breaks the build instead of surfacing later as a
missing binary in a lint run.

## The structural baseline

Default markdownlint rules produce **12,449 violations**, which says more about the defaults than
about the docs. The top four:

| Rule | Count | Verdict |
|---|---|---|
| `MD013` line-length | 9,831 | default limit is 80; the house convention is 100 |
| `MD060` table-column-style | 1,776 | wants padded cells; the repo writes `\|---\|---\|` |
| `MD032` blanks-around-lists | 121 | real, and repaired by `--fix` |
| `MD012` no-multiple-blanks | 117 | real, and repaired by `--fix` |

The committed configuration brings that to **723**. Running `markdownlint-cli2 --fix` clears 466 of
them, leaving **257**: 119 `MD013` plus **138 that need a human**. That is the entire manual backlog
for structure, and it is concentrated in a handful of files, led by `docs/architecture.md`,
`docs/spec/gitpath-foreign-content-stringency.md`, and
`docs/spec/deletecollection-attribution-expander.md`.

Three configuration choices needed a decision rather than an inference.

**`MD004` bullet style is `dash`.** The linted set has 3,369 dash bullets to 155 asterisks and 2
pluses. `consistent` only catches files that mix and leaves the split in the tree; `dash` reports
156 and `--fix` repairs every one. The asterisks cluster in `CHANGELOG.md`, which release-please
writes with 350 of them and which is excluded anyway.

**`MD013` is set to 120, not the 100 the style guide names.**
[`style-guide.md`](../style-guide.md) calls the 100-column wrap "a convention, not a lint gate", and
the measurements support that reading:

| Limit | Violations |
|---|---|
| 100 | 890 |
| 110 | 185 |
| 120 | 119 |
| 140 | 77 |

120 costs 119 fixes and still catches a runaway paragraph; 100 costs 890 and would reformat most of
`docs/`. The gate enforces the spirit at 120 while the guide keeps asking for 100, so a wrapped
paragraph stays the convention and only a runaway one fails.

**`MD040` can enforce the fence set.** It takes an `allowed_languages` list, so the language
inventory in [`style-guide.md`](../style-guide.md) becomes machine-checked. Actual usage is `bash`
(126), `yaml` (111), `text` (72), `mermaid` (67), `go` (45), `promql` (31), `console` (5), `json`
(3), `jsonc` (2), `http` (2), and one each of `sh`, `gitignore`, and `dockerfile`. The style
guide's list omits `jsonc`, `http`, `gitignore`, and `dockerfile`; either add them or convert those
six fences. `sh` should become `bash` regardless. Separately, 53 fences carry no language at all.

## The prose baseline

Vale with the committed style reports **1,032 errors and 808 warnings** in 2.9 seconds:

| Rule | Alerts | Level |
|---|---|---|
| `EmDash` | 1,032 | error |
| `Headings` | 378 | warning |
| `WordsToCut` | 350 | warning |
| `Spelling` | 152 | error |
| `Correctives` | 75 | warning |
| `Filler` | 5 | warning |

That is not close to a gate on day one. It is, however, accurate, and that is the finding that
matters.

### What Vale gets right for free

Vale skips fenced blocks and inline code by default (`SkippedScopes` and `IgnoredScopes`). That is
not a convenience: it is the style guide's one surviving spelling exemption, implemented by the
parser rather than by a list.

- Counting em dashes with `grep` gives 3,149 across the linted files. Counting markdown-aware gives
  **3,053 in prose and 96 inside code or fences**. Vale flags the prose and leaves the specimens
  alone.
- Vale flags at least one em dash in **149 of the 149 files that have one in prose**. Over all 196
  tracked files it is 171 of 171, and the single file with em dashes it does not flag is
  [`style-guide.md`](../style-guide.md) itself, where every one sits inside a `text` fence
  demonstrating what not to write. That is the correct answer, and no `grep`-based check produces
  it.
- The spelling rule fires 152 times, all of them British forms the American decision now rejects:
  `behaviour` 92, `labelled` 10, `catalogue` 7, `modelled` 6, and a tail. No identifier was flagged,
  and neither was `align="center"` in the README's HTML. Under the previous British decision the
  same corpus produced 190 alerts, so the flip neither created nor removed meaningful work: the
  repository was split almost evenly, which is why the decision had to be made on other grounds.

### What Vale gets wrong

**Per-occurrence counts under-report.** Vale finds 1,032 em dash alerts against 3,053 prose
occurrences. A sentence carrying two dashes sometimes yields two alerts and sometimes one,
depending on the surrounding block, and this reproduces on the repository's own README: the
paragraph at line 73 is flagged when the paragraph is linted alone and not flagged when the file is
linted whole. The mechanism was not pinned down.

The consequence is bounded. **File-level detection is complete**, so a gate never passes a file
that still has a violation, but the reported count is a lower bound: re-run after each pass instead
of trusting one list to be the whole job. Worth reporting upstream once there is a minimal
reproduction.

**Heading case needs a vocabulary.** The capitalization rule fires 503 times. Spot-checking says
most are real and they cluster in files written in title case throughout, such as
[`.devcontainer/README.md`](../../.devcontainer/README.md). But `VS Code`, `SSH`, and every product
name trip it too, so the rule is unusable without an `exceptions` list. Build that list from the
alerts, then reassess. This document trips it twice, on headings containing the word `Vale`.

**A quoted specimen in prose is not protected.** Vale exempts code fences and inline code, and
nothing else. A sentence that names a banned phrase in order to ban it gets flagged, which is what
happened to the `Filler` row of the table above until its examples were backticked. Backticking
them is the right fix under the style guide anyway, and it is the general answer: **quote a
specimen in backticks or in a fence, never in bare quotation marks.**

### Rules, mapped to the style guide

| Vale rule | Extension | Covers | Level |
|---|---|---|---|
| `EmDash` | `existence` | [no em dashes](../style-guide.md#punctuation-no-em-dashes) | error |
| `Spelling` | `substitution` | [American English](../style-guide.md#spelling-american-english) | error |
| `WordsToCut` | `existence` | [words to cut](../style-guide.md#words-to-cut) | warning |
| `Filler` | `existence` | `it's worth noting`, `keep in mind`, `delve into` | warning |
| `Headings` | `capitalization` | sentence-case headings | warning |
| `ProductName` | `substitution` | `Gitops Reverser` to `GitOps Reverser` | error |
| `Correctives` | `occurrence` | at most one `X, not Y` per file | warning |

`Correctives` is the interesting one: `occurrence` caps how often a pattern may appear in a scope,
which is the shape of the style guide's "budget roughly one per page" rule. It is the only rule
here that a human could not enforce by reading a diff.

Three things in the style guide stay unlinted and should: voice, "lead with the answer", and
varying the rule of three. A linter that guesses at those produces noise, and noise is how a gate
gets ignored.

### Why the built-in dictionary is off

The obvious reason to standardize on American spelling is that it lets you take Vale's defaults:
`BasedOnStyles = Vale` turns on `Vale.Spelling`, an en_US Hunspell dictionary, for free. It was
measured, and it is not usable here yet.

`Vale.Spelling` flags **4,915 words across 742 distinct terms**. The top of that list is
`namespace` (580), `repo` (302), `kustomize` (184), `resync` (165), `kubeconfig` (103), `apiserver`
(97), and `subresource` (88). None is a misspelling; all are the vocabulary these docs are written
in. The tail is the same: `goroutine`, `requeue`, `allowlisted`, `ciphertext`, `worktree`.

An accept vocabulary fixes it, but not cheaply. The 50 most common terms cover 3,138 alerts, the
top 200 cover 4,150, and 542 terms still remain past that point. Curating roughly 700 words and
maintaining the list as the product grows is real work, and it buys a check for typos that review
already catches.

So spelling stays a **short substitution list of British forms to reject**, which is the shape
American English makes possible. The dictionary is worth revisiting once the vocabulary exists for
another reason.

### No external Vale packages either

Vale can pull `write-good`, `proselint`, `Google`, or `Microsoft` through `vale sync`. Recommend
**none of them for now**, for two reasons.

1. `vale sync` downloads zips from GitHub at run time. Every lint run in CI would then depend on a
   network fetch, which is the kind of dependency this repository has otherwise removed. Vendoring
   the package into `.vale/styles/` avoids that and is the way in if we do adopt one.
2. The house style is already written down and counted.
   [`style-guide.md`](../style-guide.md) is more specific than any package, and encoding it
   directly costs about 120 lines of YAML.

The spelling decision removed the third reason this list used to carry. While the docs were British
these packages actively fought the house style; now the disagreement is narrower, mostly `Google`
wanting second person everywhere where this guide reserves it for instructions. `write-good` and
`proselint` are the plausible additions, and vendoring one is a self-contained experiment.

Consequence today: `.vale/styles/` is committed, the image needs no `vale sync` step, and a lint run
works offline.

## How to gate this

The two backlogs are 138 and roughly 3,000. They cannot take the same policy.

**Structure: fix once, then gate the whole repository.** Run `--fix` in one commit (mechanical, and
reviewable rule by rule), clear the 138 by hand, and make `markdownlint-cli2` an error gate over
every tracked file. That is achievable in a single change.

**Prose: gate only the files a change touches.** 3,053 em dashes cannot be fixed in one commit
without colliding with everything in flight, which [`style-guide.md`](../style-guide.md) already
says about the spelling cleanup. Run Vale over the files in the diff, at error level for the em
dash, spelling, and product name, and at warning level for the rest. The backlog drains through
normal editing, and nobody is asked to rewrite a file they did not open.

A whole-repo ratchet on the total count, in the style of `.coverage-baseline`, was considered and
is not recommended: the count is a lower bound (see above), so a ratchet on it can be satisfied
without fixing anything.

### Exclusions

- **`CHANGELOG.md`** is generated by release-please. Never lint it.
- **`docs/finished/`** is history by definition (see [`INDEX.md`](../INDEX.md)). Rewording it
  changes a record of what happened. Excluding it and `CHANGELOG.md` takes the corpus from 196
  files to 174.
- **`external-sources/`** holds gitignored upstream checkouts and contains symlink cycles that have
  OOM-killed the host once already through an unrooted `**` glob. Both linters must be handed an
  explicit file list from `git ls-files`, never a bare recursive glob.

## The Taskfile shape, when we get there

Sketch, not yet written:

```yaml
lint-markdown:
  desc: Structural markdown lint (markdownlint-cli2)
  cmds:
    - markdownlint-cli2 $(git ls-files '*.md' | grep -vE '^(CHANGELOG.md|docs/finished/)')

lint-prose:
  desc: Prose lint against docs/style-guide.md (Vale)
  cmds:
    - vale $(git ls-files '*.md' | grep -vE '^(CHANGELOG.md|docs/finished/)')
```

Two constraints will bite whoever writes this.

**Do not add `sources:`.** The real input is "every tracked `.md`", which Task cannot express. A
rooted per-tree list drifts out of sync silently, and an unrooted `**` walks `external-sources/`
and can exhaust host memory. `lint-docs` already carries this reasoning in a comment; follow it.

**Then update the cache assertion in CI.** The `lint` job runs `task lint` twice and asserts the
second run is fully cached apart from `lint-docs`, by name, in an `awk` filter. Un-fingerprinted
tasks added to `task lint` must be added to that filter or the job fails. Both tools finish in
under three seconds over the whole tree, so paying that on every lint run is cheaper than getting
the fingerprints wrong.

## Open questions

1. `MD013` at 120 as an error, or off entirely with the wrap left as a convention?
2. Extend the style guide's fence-language list to cover `jsonc`, `http`, `gitignore`, and
   `dockerfile`, or convert those six fences?
3. Does the prose gate run on changed files only forever, or is that a phase with an end?
4. Are `AGENTS.md`, `CLAUDE.md`, and the chart READMEs in scope? They are tracked markdown and the
   style guide says it applies to them, but they have a different audience.
