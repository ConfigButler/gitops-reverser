# `test/fixtures/` — the two corpora, and which question each one answers

Both folders here are collections of GitOps repository layouts, both are read by Go tests, and
their names have been confused for each other more than once. They answer opposite questions.

| Corpus | Direction | Asks | Executed by |
|---|---|---|---|
| [`gitops-layouts/`](gitops-layouts/README.md) | Git in | **What is this layout, and what does it force us to decide?** Real-world repository shapes, checked in as found. Records observations, never verdicts | `internal/manifestanalyzer` (render-root discovery, solvability), plus a generated baseline |
| [`layout-corpus/`](layout-corpus/README.md) | Git out | **Given a live object, which file receives it and what does the commit look like?** Our own configuration, a seeded repository, and the exact patch we expect | `TestLayoutCorpus` in `internal/git`, with the controller's half in `internal/controller` |

The short version: `gitops-layouts/` is input we did not write and do not control, and
`layout-corpus/` is a specification we did write, stated as fixtures so it cannot quietly stop
being true.

## Why that difference matters when you add a fixture

A new folder in `gitops-layouts/` is evidence. It can be as strange as the repository it came
from, it needs no configuration of ours beside it, and adding one does not make a claim about
what the operator supports. The
[support contract](../../docs/design/support-boundary/support-contract.md) is where verdicts live.

A new folder in `layout-corpus/` is a promise. It needs a `config/` that decodes into the real
API types, a `repository/` to start from, an `input/` object, and either an `expected-*.patch` or
an `expected-*-status.yaml` if the right answer is a refusal. If you cannot state the expected
result, the scenario is not ready to be a fixture yet.
