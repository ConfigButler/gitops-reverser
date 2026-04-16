# Why We Switched From Make To Task

This file used to be the migration plan. The migration is done now, so the useful question is no
longer "how should we move?" but "why was the move worth it?"

Short version: switching from `Makefile` to Task was a very good choice for this repository.

## The main win

The old `Makefile` had grown into three things at once:

- developer commands
- e2e orchestration
- incremental state management through `.stamps`

Make can do all of that, but the price was high. The file mixed shell, dependency graph tricks,
dynamic variables, grouped outputs, and recursive `$(MAKE)` calls in one place. That gave us a lot
of freedom, but it also made the execution model harder to read and safer changes harder to make.

Task gave us a better split:

- Taskfiles own orchestration and task discovery
- `.stamps` still own the explicit runtime state we actually care about
- helper scripts still do the detailed cluster and install work

That separation fits this repo much better.

## What got better

### 1. The command surface became clearer

The current root [Taskfile.yml](/workspaces/gitops-reverser/Taskfile.yml) is intentionally small:

- it includes build tasks
- it includes e2e tasks
- it exposes one flat command surface

That is easier to reason about than one large Makefile trying to be both user interface and
execution engine.

### 2. Build tasks became more explicit

The current [Taskfile-build.yml](/workspaces/gitops-reverser/Taskfile-build.yml) makes the important
parts visible:

- `desc`
- `sources`
- `generates`
- `deps`
- `cmds`

For example, `manifests` now reads like a declaration of intent: what changes trigger it, what it
produces, and what it runs.

In the old [Makefile.oldway](/workspaces/gitops-reverser/Makefile.oldway), the same area depended on
Make-specific constructs like grouped targets:

```make
manifests: $(MANIFEST_OUTPUTS)
$(MANIFEST_OUTPUTS) &: $$(MANIFEST_INPUTS)
```

That is powerful, but it asks every future maintainer to keep a fairly large chunk of GNU Make in
their head before they can safely edit a routine build step.

### 3. E2E orchestration became much easier to read

The e2e flow is where Task helped most.

In the current [test/e2e/Taskfile.yml](/workspaces/gitops-reverser/test/e2e/Taskfile.yml),
`prepare-e2e` is spelled out step by step:

```yaml
prepare-e2e:
  cmds:
    - task: install
    - task: _project-image-ready
    - task: _image-loaded
    - task: _controller-deployed
    - task: _age-key
```

That is boring in a good way. The order is visible immediately.

In `Makefile.oldway`, the same behavior was spread across file targets, prerequisites, and shell
recipes:

```make
prepare-e2e: $(CS)/$(NAMESPACE)/prepare-e2e.ready portforward-ensure
$(CS)/$(NAMESPACE)/prepare-e2e.ready: ...
```

Again, Make can express this, but the flow is much less obvious to someone reading it fresh. You
have to jump between definitions and expand variables mentally before the real sequence becomes
clear.

### 4. We kept the good part: explicit stamp state

One important lesson from the migration is that we did not actually want to replace everything.

The `.stamps` model was still useful. It captures runtime facts and readiness boundaries that Task's
own cache should not own for us. Keeping `.stamps` while moving orchestration to Task turned out to
be the right split.

That is an important hindsight point:

- switching away from Make was good
- throwing away explicit state tracking would not have been good

## Why the old freedom was costly

`Makefile.oldway` shows how much raw power Make gives you:

- special forms like `.ONESHELL`, `.SECONDEXPANSION`, and grouped targets
- recursive `$(MAKE)` calls inside recipes
- target names that are also file paths and readiness markers
- a lot of behavior encoded indirectly through prerequisite relationships

That flexibility is real, but it pushes complexity onto every maintainer.

Task is more structured and more limited, and that has been an advantage here. The YAML shape makes
it harder to be too clever. In this repo, that constraint improved readability and maintainability.

## Why Task fits this repo better

This repository benefits from a tool that makes these things obvious:

- what the public commands are
- which tasks are build tasks versus e2e orchestration
- which inputs and outputs matter
- where ordering is intentional
- which state is externalized in `.stamps`

Task does that well enough without pretending our cluster runtime can be reduced to a pure checksum
graph.

## Current file layout

The current arrangement is a good outcome:

- [Taskfile.yml](/workspaces/gitops-reverser/Taskfile.yml): small root entrypoint
- [Taskfile-build.yml](/workspaces/gitops-reverser/Taskfile-build.yml): build and local artifact tasks
- [test/e2e/Taskfile.yml](/workspaces/gitops-reverser/test/e2e/Taskfile.yml): e2e orchestration
- [Makefile.oldway](/workspaces/gitops-reverser/Makefile.oldway): historical reference only

That split is much easier to work in than the old single-file Make model.

## Bottom line

In hindsight, moving from Make to Task was not just neutral cleanup. It improved the repo.

The biggest reasons are:

- the public command surface is clearer
- the e2e flow is easier to follow
- build tasks are more declarative
- `.stamps` stayed where explicit runtime state still matters
- maintainers no longer need as much Make-specific knowledge to change routine automation safely

So this document should no longer be read as a migration checklist. It is now the rationale for why
the current Task-based structure is the better long-term home for this repository.
