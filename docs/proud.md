# What I'm proud of

GitOps Reverser started as one idea — *turn live Kubernetes API activity into clean, versioned
YAML in Git* — but the part I keep coming back to is everything **around** that idea. The
developer experience, the build graph, the test harness, and the honesty of the docs. This page
is a tour of the things I'm proud of, and a thank-you to the projects that made them possible.

Screenshots are placeholders for now — each one has an HTML comment above it saying exactly what
to capture.

---

## 1. You can start programming in one click

Open the repo in VS Code, **Reopen in Container**, and everything is already there. No "install
these twelve tools first" README. The [devcontainer](../.devcontainer/devcontainer.json) ships every
pinned tool the project needs — Go, `kubectl`, k3d, Helm, Kustomize, Kubebuilder, Flux, Tilt,
`golangci-lint`, Delve, `valkey-cli`, `actionlint` — plus Go module/build caches on named volumes
and SSH commit signing auto-synced from your host agent.

## 2. docker-outside-of-docker + k3d, on purpose

I deliberately use **docker-outside-of-docker** and **k3d** instead of a nested engine. It means
the k3d nodes and the images you build show up in *your own* Docker instance, and your local
`kubectl`, `k9s`, and other tools connect to the cluster with no extra plumbing. The cluster isn't
hidden inside a black box — it's right there next to everything else you run.

## 3. Every service you'd want is already forwarded

The moment the cluster is up, six labeled ports auto-forward — no hunting for URLs:

| Port | Service | Why it's there |
|---|---|---|
| 13000 | **Gitea** | A real Git server for the reversed repos |
| 10350 | **Tilt** | The live development loop UI |
| 19081 | **Allure** | The e2e test report |
| 19090 | **Prometheus** | Controller metrics + history |
| 19080 | **Flux Operator UI** | The GitOps side of the house |
| 16379 | **Valkey (Redis-compatible)** | Watch-resume state |
## 4. Tilt turns the whole thing into a playground

`tilt up`, open [localhost:10350](http://localhost:10350), and you get a live loop: edit a `.go`
file and the controller rebuilds and redeploys itself. But the part I love is the **playground** —
one-click buttons in the Tilt UI to `upsert`/`delete` a ConfigMap or Secret, apply ten at once, or
fire a random ConfigMap, then watch a commit land in Gitea seconds later. It's the fastest way to
*feel* what the operator does.

## 5. Task, pushed about as far as it goes

The build isn't a `Makefile` — it's a real **DAG** expressed in [Task](https://taskfile.dev). Every
step from source file to a running controller under e2e declares its real `sources`, `generates`,
and `deps`, so **only what changed re-runs**. A cold e2e bring-up is ~5–6 minutes; a warm re-run of
the *same* suite is ready in **seconds**, because every stamp is still up to date. The whole graph,
and why it pays off, is written up in [docs/tasks-overview.md](tasks-overview.md).

## 6. An e2e harness that respects your time — and helps when it breaks

The suite runs **in parallel** (Ginkgo processes) and renders as an **Allure** timeline, one lane
per process, so you can see exactly what ran concurrently and where the bottlenecks are. The
cluster is **reused** between runs via `.stamps`, not torn down. And when things go wrong:

- **Ctrl-C, or a setup failure, preserves the entire live state** so you can poke around.
- A **failed spec auto-dumps diagnostics** — events, `CommitRequest`s, controller logs, and a pod
  describe — into the log and the Allure attachment.
- The spec's **Git checkout survives** under `.stamps/repos/`, with a working remote and
  credentials, so you can `git log`, `git pull`, or `git push` the reversed history of a past run.

## 7. The `.stamps` folder is a living snapshot of the last run

`.stamps/` is throwaway state, but it reads like a logbook. The controller image digest sits in
`.stamps/image/controller.id`. Every reversed repo is checked out under `.stamps/repos/<spec>/`,
and its Git log *is* the product output:

```text
$ git -C .stamps/repos/e2e-manager-crd-XXXX log --oneline
9a9cff1 reconciled 42 customresourcedefinitions (last resourceVersion: 121494)
0b0554a [CREATE] crd-lifecycle.e2e.example.com/v1/icecreamorders/charlies-order
b2c0f6f [UPDATE] crd-lifecycle.e2e.example.com/v1/icecreamorders/bobs-order
39accac [DELETE] crd-lifecycle.e2e.example.com/v1/icecreamorders/alices-order
```

Those checkouts are kept on purpose. When they pile up, `task cleanup-stamp-repos` reclaims the
disk in one command.

## 8. The product itself: clean, attributed, reviewable YAML

The output is the point. Live objects come in noisy; what lands in Git is **sanitized** YAML
(`status`, `managedFields`, and runtime noise stripped), diffed against current content, with
useful commit metadata. Commits can carry the **actual user or service account** as author (when
audit attribution is on), `Secret`s can be **SOPS + age encrypted** before commit, and commits can
be **SSH-signed**. It even captures resources served by an **aggregated API server**.

## 9. Rigor I didn't cut corners on

- A **self-ratcheting coverage gate** (`cover-check`) that only ever moves the floor up, with
  merged unit + e2e coverage reported to Codecov.
- **Three install modes** — config-dir, Helm, and plain manifests — all exercised by e2e.
- A **mutation-capture lab** (`lab-e2e`) for stress-testing capture correctness.
- `actionlint` on the CI workflows, Conventional Commits driving `release-please`.
- A genuinely deep [docs/architecture.md](architecture.md) that answers the "but how does it
  actually…" questions instead of hand-waving.

---

## Standing on the shoulders of giants

None of the above exists without a huge amount of excellent open-source work. Thank you to the
maintainers of all of these.

**Kubernetes & the operator toolkit**
- [Kubernetes](https://kubernetes.io/) and `kubectl` — the platform this whole project reflects.
- [Kubebuilder](https://kubebuilder.io/), [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime),
  and [controller-tools](https://github.com/kubernetes-sigs/controller-tools) — the operator scaffolding and codegen.
- [Kustomize](https://kustomize.io/) and [Helm](https://helm.sh/) — how the controller is packaged and installed.
- [cert-manager](https://cert-manager.io/) — TLS for the webhooks.

**GitOps & the pattern**
- [Flux](https://fluxcd.io/) and the [Flux Operator](https://github.com/controlplaneio-fluxcd/flux-operator)
  — the reconcilers this project reverses *into*, and part of the e2e environment.
- [Argo CD](https://argo-cd.readthedocs.io/) — studied closely while thinking about credentials, drift, and comparisons.

**The local cluster & dev loop**
- [k3d](https://k3d.io/) and [k3s](https://k3s.io/) (SUSE/Rancher) — a real cluster in seconds, visible in your own Docker.
- [Tilt](https://tilt.dev/) — the live development loop and playground UI.
- [Docker](https://www.docker.com/) and the [Dev Containers](https://containers.dev/) spec — the one-click environment.

**Git server, state & secrets**
- [Gitea](https://about.gitea.com/) — the local Git server the reversed repos push to.
- [Valkey](https://valkey.io/) — Redis-compatible watch-resume state — and [Redis](https://redis.io/), whose protocol and lineage it carries.
- [SOPS](https://github.com/getsops/sops) and [age](https://github.com/FiloSottile/age) — Secret encryption before commit.

**Testing, quality & observability**
- [Ginkgo](https://onsi.github.io/ginkgo/) and [Gomega](https://onsi.github.io/gomega/) — the parallel e2e suite.
- [Allure](https://allurereport.org/) (Qameta) — the timeline report.
- [Prometheus](https://prometheus.io/) and the [Prometheus Operator](https://prometheus-operator.dev/) — metrics.
- [golangci-lint](https://golangci-lint.run/), [staticcheck](https://staticcheck.dev/),
  [actionlint](https://github.com/rhysd/actionlint), and [Delve](https://github.com/go-delve/delve) — keeping the code honest and debuggable.
- [envtest / setup-envtest](https://github.com/kubernetes-sigs/controller-runtime/tree/main/tools/setup-envtest) and [Codecov](https://about.codecov.io/) — fast API-server tests and the coverage ratchet.

**Build, release & language**
- [Go](https://go.dev/) — the language.
- [Task](https://taskfile.dev/) (go-task) — the build DAG I'm so fond of.
- [release-please](https://github.com/googleapis/release-please) — automated, Conventional-Commit-driven releases.
- [scc](https://github.com/boyter/scc) — quick line-of-code counts.

**Docs & media**
- [Excalidraw](https://excalidraw.com/) — the architecture diagrams.
- [asciinema](https://asciinema.org/) and [agg](https://github.com/asciinema/agg) — the hero demo GIF.
- [Mermaid](https://mermaid.js.org/) — the DAG diagrams rendered inline in the docs.

And the broader idea this project builds on lives at [reversegitops.dev](https://reversegitops.dev).

---

*If you're a first-time reader: the fastest way to see all of this is `tilt up`, then open the Tilt
UI and click a playground button while watching Gitea. Everything else in this list is what makes
that loop fast, observable, and trustworthy.*
