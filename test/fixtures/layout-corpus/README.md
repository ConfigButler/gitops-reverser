# The layout corpus — where a document goes in Git, stated as fixtures

Every folder below is a worked example that a test executes. A scenario seeds a worktree from its
`repository/`, folds the object in its `input/` through the real plan-then-flush write path with
the configuration in its `config/`, and compares a normalized diff against the committed
`expected-*.patch`. Nothing here is illustration: if the writer disagrees with a fixture, the build
goes red.

That is why the corpus lives under `test/` rather than under `docs/`. It used to sit in
`docs/layout/`, where its READMEs were read by people and its fixtures by nobody. The argument it
serves still lives in [`docs/layout/`](../../../docs/layout/README.md); this is the evidence.

```bash
task test                                    # the corpus runs as part of the unit suite
go test ./internal/git/ -run TestLayoutCorpus -v
go test ./internal/git/ -run TestLayoutCorpus -update   # rewrite expected-*.patch from the observed diff
```

Use `-update` to see what a change did, then read the resulting diff as the review. Never use it to
make a red test green without reading it: the patch is the specification, so overwriting one is
editing the specification.

## What is in it

| Folder | Holds |
|---|---|
| [`shapes/`](shapes/README.md) | the cross-product. Flat and tree, each with and without `metadata.namespace` in the committed document, plus one kustomize folder, base-and-overlays, and layered. The same live object is written into all of them, so the only thing that differs between two folders is the configuration that produced it |
| [`specific-examples/`](specific-examples/README.md) | the remainder: an Argo CD app-of-apps and a Flux two-layer repository, which are ecosystem scenarios rather than folder shapes, plus the shared `GitProvider` prerequisites |

The sibling corpus at [`../gitops-layouts/`](../gitops-layouts/README.md) answers the opposite
question and is easy to mistake for this one. [`../README.md`](../README.md) is the one-table
distinction.

## The conventions, and why each one is load-bearing

Three of these are stated in `internal/git/layout_corpus_test.go` as well, because breaking one of
them is how a corpus quietly stops being one.

**A scenario folder holds `config/`, `repository/`, `input/`, and its expectations.** `repository/`
is rooted at the repository root rather than at `spec.path`, so a fixture shows the folder a user
would actually browse. Patches carry no `index` lines, so a diff does not churn on blob hashes.

**`config/` decodes into the real API types, strictly.** A `gittarget.yaml` naming a field the API
does not have fails to parse rather than being ignored, which is what keeps the worked examples and
the shipped API the same API. This is not theoretical: it is what the corpus caught during the
breaking wave, when five fields were removed and a stale fixture would otherwise have kept passing.

**Refusals are fixtures too.** A scenario whose right answer is "we write nothing" asserts an
`expected-*-status.yaml` instead of a patch, and the harness reads that condition whole: status,
reason and message. A set of examples in which every write succeeds is advertising rather than
specification, which is why the refusing halves are here at all.

**A scenario for behavior that is not built yet is written now and skipped, naming the track that
unskips it.** The corpus is then the definition of done for that track. One such skip is live
today: shape 8's `images:` authoring belongs to track C of
[`build-order.md`](../../../docs/design/build-order.md).

## Adding a scenario

1. Create the folder with `config/`, `repository/` and `input/`.
2. Add a row to `layoutCorpus()` in `internal/git/layout_corpus_test.go`.
3. Run with `-update` to generate the patch, then **read it** and decide whether it is what you
   meant. That reading is the whole value of the step.
4. If the right answer is a refusal, write the `expected-*-status.yaml` by hand instead and set
   `status:` rather than `patch:` on the row.

`TestLayoutCorpus_EveryFixtureFolderIsExecuted` closes the set over the filesystem: a folder with
an `input/` that no row names fails the build, so a fixture cannot be added and left unrun. A
folder with no `input/` illustrates prerequisites rather than a write, and is skipped by that
guard.
