# E2E CI stability — how the setup works & where it hurts

A walkthrough of how the e2e job in [`.github/workflows/ci.yml`](../../.github/workflows/ci.yml)
runs today, a check of your assumptions, and concrete answers to the two
questions (run `prepare-e2e` up front; observe disk usage).

---

## 1. How a run is wired today

### 1.1 The `e2e` job and its matrix

```
e2e (runs-on: ubuntu-latest)
 ├─ matrix leg "quickstart"  → E2E_GINKGO_PROCS=2, K3D_AGENT_COUNT=1, needs_artifact=true
 │     script: INSTALL_MODE=helm task test-e2e-quickstart-helm
 │           → task test-image-refresh
 │           → INSTALL_MODE=plain-manifests-file task test-e2e-quickstart-manifest
 └─ matrix leg "full"        → E2E_GINKGO_PROCS=1, K3D_AGENT_COUNT=1, needs_artifact=false
       script: task test-e2e-full
```

`needs: [build-ci-container, docker-build, lint-helm]` — so by the time e2e
starts, the CI base image, the project image, and the release bundle
(`install.yaml` + `gitops-reverser.tgz`) already exist.

Each matrix leg runs the actual suite inside the CI container via
**Docker-outside-of-Docker**:

```yaml
docker run --rm --network host \
  -v "${GITHUB_WORKSPACE}:/workspaces/gitops-reverser" \
  -v /var/run/docker.sock:/var/run/docker.sock \
  ... ${CI_CONTAINER} bash -c "<matrix.script>"
```

The container talks to the **runner's** Docker daemon (the mounted socket), and
that same daemon is what k3d uses to create the cluster nodes. So everything —
the CI container, k3d node containers, the project image, and every Flux-managed
service image — lands on the **one** runner VM's disk.

### 1.2 What `task test-e2e-*` actually does

The Task targets ([`test/e2e/Taskfile.yml`](../../test/e2e/Taskfile.yml)) don't
prepare the cluster themselves — they just invoke Ginkgo:

```
go run .../ginkgo --procs=$E2E_GINKGO_PROCS --timeout=$E2E_GO_TEST_TIMEOUT ... ./test/e2e/
```

Cluster prep happens **inside** the suite. `SynchronizedBeforeSuite` (process #1
only) calls `prepareE2EClusterOnce()`
([`e2e_suite_test.go:82`](../../test/e2e/e2e_suite_test.go#L82)), which shells out
to `task prepare-e2e` with the leg's `CTX / INSTALL_MODE / NAMESPACE /
INSTALL_NAME`.

### 1.3 The `prepare-e2e` dependency graph

`prepare-e2e` is a go-task DAG, every node gated by a stamp file under
`.stamps/cluster/<ctx>/`. Stamps make it **idempotent**: a node whose stamp
already exists and whose sources are unchanged is skipped. The chain
(leaf → root):

```
prepare-e2e
 ├─ _prepare-e2e-ready
 │    ├─ _controller-deployed ── _image-loaded ── _project-image-ready (load-image.sh; pull mode in CI)
 │    │                                        └─ _cluster-ready ── start-cluster.sh (k3d create)
 │    ├─ _webhook-tls-ready ─┐
 │    ├─ _sops-secret-applied ┤
 │    └─ _aggregated-api-ready┘
 │         └─ _services-ready ── _flux-setup-ready ── _flux-installed ── _cluster-ready
 └─ portforward-ensure (runs after the ready stamp, never in parallel with it)
```

The genuinely slow / hang-prone nodes:

| Node | What it waits on | Timeout |
|------|------------------|---------|
| `_cluster-ready` | `start-cluster.sh`: k3d create, all nodes `Ready` | 60×2s health loop |
| `_flux-installed` | flux-operator install + FluxInstance + Flux deployments `Available` | `FLUX_WAIT_TIMEOUT=500s` |
| `_flux-setup-ready` | every HelmRelease/Kustomization (gitea, valkey, prometheus, traefik…) `Ready` | `FLUX_SERVICES_WAIT_TIMEOUT=600s` per resource |

The whole Ginkgo run — prepare **plus** the specs — is bounded by one timeout:
`E2E_GO_TEST_TIMEOUT=15m` (smoke/quickstart) or `E2E_FULL_TIMEOUT=30m` (full).

### 1.4 Why it "goes silent for ~15 min, then green on retry"

This is the most important mechanical detail. `prepare-e2e` runs **inside
Ginkgo's `SynchronizedBeforeSuite`**, and its output is written to
`GinkgoWriter`. In `-v` (non-streaming) mode Ginkgo **buffers** that writer and
only flushes it when the node finishes or fails. So while the cluster + Flux +
services are coming up, the CI log shows nothing.

If any readiness wait stalls (a HelmRelease that never reconciles, a node that
never goes `Ready`, a slow/stalled image pull), prepare sits there until the
outer **15m Ginkgo timeout** fires — at which point you get a goroutine dump
instead of a useful "service X never became ready" message. A re-run starts from
a clean VM and usually wins the race, which is exactly the signature of a
**flaky readiness/resource-contention problem**, not a hard deterministic
failure.

---

## 2. Your assumptions, checked

> **Every job (incl. every matrix leg) gets a new runner.** ✅ Correct.
GitHub provisions a fresh VM per job, and each matrix combination is its own job.
The two e2e legs never share disk, Docker daemon, or `.stamps`.

> **Public-repo runners: max 20 concurrent, 4 vCPU / 16 GB / 14 GB SSD.** ✅
Matches the current standard free Linux runner (4 vCPU, 16 GB RAM, 14 GB SSD)
and the 20-concurrent-job ceiling for the free tier. The **14 GB SSD is the
squeeze** — the runner image already pre-consumes a chunk of it, and *everything*
(CI container, k3d nodes, project image, all Flux service images, buildx/Go
caches) shares that one disk.

> **Biggest chance is running out of disk.** ⚠️ Plausible but **not yet proven —
and the symptom argues against it.** A full disk almost always fails *loudly and
fast* ("no space left on device"), not with a 15-minute silent hang that
disappears on retry. The silent-hang-then-green pattern points more at a
**readiness wait stalling** (Flux service contention, image-pull stall, node not
Ready). Disk pressure can *cause* those stalls (e.g. kubelet
`DiskPressure` taint → pods won't schedule → HelmRelease never Ready), so it's
worth ruling in or out — but **measure before you commit to that theory.** That's
exactly what §4 adds.

> **Set `K3D_AGENT_COUNT=1`.** ✅ Already done — both matrix legs pass
`k3d_agent_count: "1"` (1 server + 1 agent = 2 nodes). You could go to **0
agents** (single-node; k3s schedules workloads on the server) to shave one more
node-container's worth of RAM/disk/inotify. Trade-off: less parallelism headroom
for `--procs=2`. Worth trying on the quickstart leg if measurements show
resource pressure.

> **Spin up 4 e2e legs and run more in parallel.** ◑ Mixed. More legs = more
*isolated* runners, so wall-clock can drop **if you split specs across them**.
But it does **not** reduce per-leg disk/RAM pressure — each new leg re-pulls the
full image set onto its own 14 GB disk. If disk/resource pressure is the real
root cause, adding legs spreads load but each leg is just as likely to hit it.
Fix the per-leg footprint first (measure → free disk → maybe 0 agents), *then*
fan out for speed.

---

## 3. Question 1 — bring the cluster up explicitly first

**Recommended: yes — and target `_services-ready`, not the full `prepare-e2e`.**
Add an explicit cluster-bring-up step *before* the test `docker run`, in the same
container + workspace volume, with logs streaming live. Keep the in-Ginkgo
`prepareE2EClusterOnce()` call as-is — because the DAG is stamp-gated, it then
**no-ops** the parts already done.

### Why `_services-ready` is the right target (not `prepare-e2e`)

The hang is in *waiting for the Flux install + Flux-managed services to become
ready*. `_services-ready` is precisely that subtree and nothing more:

```
_services-ready
 └─ _flux-setup-ready ── _flux-installed ── _cluster-ready (k3d create)
```

It deliberately stops short of `_image-loaded`, `_controller-deployed`, and the
whole install-mode tail. That gives two concrete advantages over running
`prepare-e2e`:

- **No install-mode variables to decide.** The entire `_services-ready` chain is
  independent of `INSTALL_MODE` / `NAMESPACE` / `INSTALL_NAME`; it needs only
  `CTX`, which already defaults to `k3d-gitops-reverser-test-e2e`. So the step is
  the same for *both* matrix legs — no `helm` vs `plain-manifests-file` vs
  `config-dir` branching, and no risk of priming the wrong install tail.
- **It's exactly the hang surface.** Cluster create + Flux operator/instance +
  every HelmRelease/Kustomization `Ready` wait — the three slow nodes from §1.3
  — and nothing you don't care about debugging right now.

The image load and controller deploy stay inside Ginkgo's `prepare-e2e`, which is
fine: those aren't where it hangs, and `_cluster-ready`/`_flux-*`/`_services-ready`
are already stamped by the time Ginkgo runs, so it skips straight to them.

Why this kills the symptom either way: run **outside** Ginkgo, the output
**streams** instead of being buffered to `GinkgoWriter`, so you see *which* node
hangs in real time instead of a 15-min silence + goroutine dump — and a genuine
bring-up failure fails a short, clearly-named CI step instead of masquerading as
a test timeout.

### Two ways to invoke it

`task _services-ready` works **today** — no task in
[`test/e2e/Taskfile.yml`](../../test/e2e/Taskfile.yml) is marked
`internal: true`, so the leading `_` is only a naming convention, not a runtime
guard. But depending on a `_`-prefixed target from CI is fragile (a future
`internal: true` or rename breaks CI silently). Cheapest hardening is a one-line
public wrapper:

```yaml
# test/e2e/Taskfile.yml
  prepare-cluster:
    desc: Bring up the e2e cluster + Flux + Flux-managed services (install-mode independent)
    cmds:
      - task: _services-ready
```

CI step (new, before "Run E2E tests in CI container"), identical for both legs:

```yaml
- name: Bring up cluster + Flux services (explicit, streaming logs)
  run: |
    docker run --rm --network host \
      -v "${GITHUB_WORKSPACE}:${{ env.CI_WORKDIR }}" \
      -v /var/run/docker.sock:/var/run/docker.sock \
      -w "${{ env.CI_WORKDIR }}" \
      -e IMAGE_DELIVERY_MODE=${{ env.IMAGE_DELIVERY_MODE }} \
      -e K3D_AGENT_COUNT=${{ matrix.k3d_agent_count }} \
      -e HOST_PROJECT_PATH=${{ github.workspace }} \
      ${{ env.CI_CONTAINER }} \
      bash -c "
        git config --global --add safe.directory ${{ env.CI_WORKDIR }}
        task prepare-cluster   # or: task _services-ready
      "
```

Note this step needs **no `PROJECT_IMAGE`, no `INSTALL_MODE`** — that's the whole
point of stopping at `_services-ready`. Keep `CTX` at its default (or pass the
same value the suite uses) so the stamps match and Ginkgo skips the work.

### 3.1 Why the boundary stays at `_services-ready` (and not `_aggregated-api-ready`)

A natural question is whether to push the explicit pre-step one node further, to
`_aggregated-api-ready`. **No — that node is not install-mode- or
image-independent.** Its full subtree:

```
_aggregated-api-ready
 └─ _aggregated-api-webhook-kubeconfig-ready
      ├─ _services-ready                       ← cluster + Flux + all flux services
      └─ _webhook-tls-ready
           └─ _controller-deployed             ← install-mode-specific + project image
                ├─ install        (INSTALL_MODE: helm / plain-manifests-file / config-dir)
                └─ _image-loaded  (needs PROJECT_IMAGE)
```

Folding `_aggregated-api-ready` into the pre-step would re-introduce exactly the
two things stopping at `_services-ready` lets us avoid — choosing `INSTALL_MODE`
and passing `PROJECT_IMAGE` — and would tie the step to one matrix leg's mode
(the quickstart leg switches modes mid-run). So `_services-ready` is the last
node that is both **install-mode-agnostic** and **image-agnostic**; one step
further and that property is gone. The aggregated-api bring-up keeps running
inside the in-Ginkgo `prepare-e2e`, where it belongs.

### 3.2 Observation: `_services-ready` is an all-or-nothing barrier (future refactor)

`_aggregated-api-ready` *functionally* needs only **cert-manager** and the
**aggregated-api** HelmRelease — its commands only wait on the two cert-manager
`Certificate`s, the `aggregated-api` namespace deployments, and the `wardle`
APIService/discovery. It does **not** need gitea, valkey, prometheus-operator,
kro, reflector, or ingress. But it depends on `_services-ready`, and
[`hack/e2e/wait-flux-services.sh`](../../hack/e2e/wait-flux-services.sh) holds
that single gate until **every** HelmRelease/Kustomization is `Ready`.

Consequence for stability: `_services-ready` is a coarse barrier. If *any one*
Flux service is slow or flaky to reconcile, it blocks aggregated-api, the
controller install, and effectively the whole suite — even specs that never
touch that service. A finer-grained dependency (e.g. aggregated-api waiting only
on cert-manager + its own release) would isolate such failures.

**For now we are not refactoring this** — start with the immediate
`_services-ready` pre-step and let the streaming logs + disk metrics tell us
whether a specific service is the culprit. The barrier-narrowing is a candidate
follow-up *if* the data shows a single slow service is gating everything.

---

## 4. Question 2 — observe disk usage (after the run, on failure, and the peak)

Three layers, cheap to add, all `if: always()` so they fire on failure too.

### 4.1 Snapshot after the run (and on failure)

```yaml
- name: Disk & Docker usage (post-run)
  if: always()
  run: |
    echo "## Disk after ${{ matrix.name }}" >> "$GITHUB_STEP_SUMMARY"
    { echo '```'; df -h; echo; docker system df -v; echo '```'; } >> "$GITHUB_STEP_SUMMARY"
    echo "### Biggest consumers"
    sudo du -xh --max-depth=1 /var/lib/docker 2>/dev/null | sort -rh | head
    du -sh .stamps 2>/dev/null || true
```

`df -h` tells you headroom; `docker system df -v` breaks it down by
images / containers / build cache / **volumes** (k3d node data lives here).

### 4.2 Capture the *peak* with a background sampler

A post-run snapshot misses the peak (k3d image imports + Flux pulls spike
mid-prepare, then settle). Sample throughout instead:

```yaml
- name: Start disk sampler
  run: |
    ( while true; do
        printf '%s %s\n' "$(date -u +%H:%M:%S)" \
          "$(df --output=avail -BM / | tail -1 | tr -d ' ')"
        sleep 10
      done ) > df-samples-${{ matrix.name }}.log &
    echo "SAMPLER_PID=$!" >> "$GITHUB_ENV"

# ... prepare + test steps ...

- name: Stop sampler & report peak
  if: always()
  run: |
    kill "${SAMPLER_PID}" 2>/dev/null || true
    echo "### Min free space on / during ${{ matrix.name }}" >> "$GITHUB_STEP_SUMMARY"
    sort -k2 -n df-samples-${{ matrix.name }}.log | head -1 >> "$GITHUB_STEP_SUMMARY"
    { echo '```'; cat df-samples-${{ matrix.name }}.log; echo '```'; } >> "$GITHUB_STEP_SUMMARY"

- name: Upload disk samples
  if: always()
  uses: actions/upload-artifact@v7
  with:
    name: e2e-disk-samples-${{ matrix.name }}
    path: df-samples-${{ matrix.name }}.log
    if-no-files-found: ignore
```

Lowest "avail" line = your true low-water mark. If it never drops below a few
hundred MB, disk is **not** your problem and you should look at Flux/service
readiness instead.

### 4.3 If §4 confirms disk pressure — reclaim space first

GitHub runners ship with large unused toolchains. A one-liner up front buys
~10–30 GB before the cluster is even created:

```yaml
- name: Free disk space
  run: |
    sudo rm -rf /usr/share/dotnet /usr/local/lib/android /opt/ghc \
                /opt/hostedtoolcache/CodeQL /usr/local/.ghcup
    sudo docker image prune -af
    df -h /
```

(or use `jlumbroso/free-disk-space` / `easimon/maximize-build-space`.)

---

## 5. Recommended order of attack

1. **Add the instrumentation (§4.1–4.2) — do this first.** One run tells you
   whether disk is actually the bottleneck. Cheap, non-invasive, settles the
   open question instead of guessing.
2. **Add the explicit streaming `task _services-ready` / `prepare-cluster` step
   (§3).** Independently valuable: turns the silent 15-min hang into a readable,
   correctly-attributed failure, needs no install-mode variables, and the
   in-Ginkgo `prepare-e2e` call still no-ops the cluster/Flux/services part.
3. **Then, based on the data:**
   - Disk-bound → reclaim space (§4.3), consider `K3D_AGENT_COUNT=0`.
   - Readiness-bound → look at *which* HelmRelease/Kustomization stalls (now
     visible thanks to step 2) and tune that service or its wait.
4. **Only after the per-leg run is stable**, consider fanning out into more
   matrix legs for wall-clock speed (§2) — splitting specs, not duplicating the
   whole suite.

The throughline: the "green on retry" behaviour is a **flaky readiness race**,
and the current design **hides the evidence** by burying prepare inside a
buffered Ginkgo writer under a single coarse 15-minute timeout. Steps 1 and 2
make the evidence visible; everything else is tuning once you can see it.
