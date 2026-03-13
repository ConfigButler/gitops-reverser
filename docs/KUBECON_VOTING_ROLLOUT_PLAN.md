# KubeCon Voting Portal Rollout Plan

## Goal

Run a simple audience voting portal on `voting.reversegitops.dev`, hosted from this devcontainer-driven setup, in a way that:

- is realistic enough for the talk,
- exercises the "path back" from live changes into Git,
- does not introduce avoidable demo-day risk,
- leaves behind useful test coverage for the project.

## Short Recommendation

Using this environment is reasonable for a talk demo, but only if we treat it as a staged rollout and not as a last-minute live experiment.

The biggest risk is not Kubernetes itself. It is the full chain:

1. audience phones,
2. public DNS,
3. Cloudflare tunnel,
4. devcontainer host,
5. k3d cluster,
6. app behavior under bursty writes,
7. GitOps Reverser behavior under concurrent changes.

That means we should prove the chain in layers, in this order, and keep the voting app intentionally boring.

## Proposed Order

### Phase 1: Define the demo contract

Before installing anything else, lock down the shape of the demo.

Deliverables:

- decide whether votes are anonymous or tied to a session/user identifier,
- decide whether duplicate voting is allowed,
- decide whether results must update live or can refresh every few seconds,
- decide whether GitOps Reverser is in the critical request path or only observing,
- decide what failure is acceptable during the talk.

Recommendation:

- keep GitOps Reverser out of the synchronous request path,
- store votes in a simple in-cluster service first,
- let GitOps Reverser observe resulting Kubernetes changes or supporting resources rather than acting as the database.

Why:

The talk demo should prove the concept, not depend on every moving piece succeeding within one HTTP request.

### Phase 2: Create a dedicated talk environment branch

Introduce a dedicated branch for talk-specific manifests and automation.

Suggested shape:

- `main`: normal project development,
- `talk/kubecon-voting`: talk environment manifests and overlays,
- optionally `talk/kubecon-voting-rehearsal`: dry-run branch for repeated practice.

Recommendation:

- yes, adding Flux is probably worth it here,
- use Flux to install only the talk-specific layer,
- do not point Flux at the same exact resources GitOps Reverser is writing back for the demo.

Why:

- Flux makes it much easier to manage a branch with extra components like the voting app and `cloudflared`,
- it also mirrors the real-world "Git in control" story more clearly,
- but the repo already warns about feedback loops, so we should keep ownership boundaries explicit.

Suggested ownership split:

- Flux manages infra and app deployment manifests,
- the voting app handles vote persistence,
- GitOps Reverser observes selected resources or a dedicated demo namespace,
- GitOps Reverser does not write back into the same Flux-managed path that is being continuously reconciled.

### Phase 3: Make ingress boring and rehearseable

Get public access working early.

Recommendation:

- use `cloudflared` only after the app works locally inside k3d,
- expose one hostname only: `voting.reversegitops.dev`,
- avoid adding extra public endpoints during the talk setup,
- prepare a local fallback URL or port-forward for rehearsal.

What to validate:

- DNS resolution,
- tunnel reconnect behavior,
- TLS from a real phone on real mobile data,
- behavior after restarting `cloudflared`,
- behavior after restarting the app deployment,
- behavior after a devcontainer restart, if that is realistic for your host setup.

### Phase 3.5: Add a private TTY path for live slides

In addition to the public voting endpoint, keep a separate private path into the dev environment for the talk itself.

Recommendation:

- run a TTY server in the background on the host or inside the dev environment,
- do not expose that TTY service to the public internet,
- open only the required port on the private side,
- access it through your existing VPN from the slide deck or demo machine,
- treat this as the preferred operator/demo access path during the talk.

Why:

- you have already been testing this workflow,
- it separates audience traffic from presenter access,
- it avoids turning shell access into a public-facing risk,
- it is likely the safest and most reliable option for live terminal control.

What to validate:

- VPN reachability from the presentation machine,
- TTY reconnect behavior after network hiccups,
- terminal latency while the voting demo is under load,
- behavior after restarting the TTY server,
- whether the service survives a devcontainer restart or can be brought back quickly.

### Phase 4: Add a minimal end-to-end app smoke test

Yes, some of this belongs in e2e, but only the deterministic parts.

Recommended e2e scope:

- deploy the voting app into the existing k3d harness,
- submit a small number of votes,
- verify the expected Kubernetes-side effect exists,
- verify GitOps Reverser still behaves correctly if that side effect is something it watches,
- verify the app remains usable after a rollout/restart.

This should be a normal e2e or quickstart-style smoke test, not a high-load test.

Why not put the full load test into the normal e2e suite:

- CI-friendly e2e should stay deterministic and reasonably fast,
- "100 people hammering phones at once" is more of a scenario test than a correctness test,
- load tests often fail for environmental reasons and become noisy in CI.

### Phase 5: Add a dedicated load test target that reuses the e2e harness

This is where I would put the audience simulation.

Recommendation:

- reuse the existing k3d/Gitea/e2e setup,
- add a separate load-test target rather than folding it into `make test-e2e`,
- keep one tiny load-related assertion in e2e if we want regression coverage,
- keep the real stress scenario as an opt-in target for rehearsal.

Suggested shape:

- `make test-load-voting` or `make test-e2e-voting-load`
- deploy the talk stack,
- generate 100-200 virtual users with bursty vote submission,
- capture:
  - request latency,
  - error rate,
  - app pod CPU/memory,
  - tunnel stability,
  - API server pressure,
  - GitOps Reverser queue depth / processing latency / commit rate,
  - Git write behavior under bursts.

Success criteria:

- no user-visible failures for the expected audience size,
- no uncontrolled backlog growth,
- no Git corruption or push thrashing,
- recovery after the burst finishes,
- acceptable latency on cheap mobile devices and conference Wi-Fi.

My recommendation on tooling:

- use a simple HTTP load generator outside the normal Go e2e assertions,
- keep result evaluation threshold-based,
- archive a markdown summary or artifact from rehearsal runs.

### Phase 6: Observe the full GitOps story under rehearsal conditions

Once the app and load path are stable, add the "interesting" GitOps proof.

What to prove:

- votes or vote-driven actions create a clean, understandable Git history,
- the generated YAML is meaningful enough to show on stage,
- the commit frequency under burst load stays sane,
- Flux and GitOps Reverser do not fight each other,
- rollback and redeploy behavior are understandable.

This is the place to decide whether every vote should produce a Git-visible change.

Recommendation:

- probably not.

For the talk, a better pattern may be:

- app records votes normally,
- a summarizer periodically writes an aggregate ConfigMap or CR,
- GitOps Reverser captures the aggregate state,
- the Git history becomes legible instead of becoming 100 near-identical commits.

That will tell a cleaner story on stage and reduce risk significantly.

### Phase 7: Rehearsal and failure drills

Before KubeCon, run at least two full rehearsals with the public hostname.

Rehearsal checklist:

- cold start from the current branch,
- Flux sync completes,
- tunnel comes up,
- real phone can vote,
- 100-user synthetic load passes,
- GitOps Reverser output is still clean,
- restart one pod during voting,
- restart `cloudflared`,
- confirm recovery time is acceptable,
- verify logs and dashboards are easy to read during stress.

## What I Would Build First

If we want the safest path, I would build in this order:

1. a very small voting app with one namespace and one hostname,
2. local k3d deployment without public ingress,
3. Cloudflare tunnel exposure,
4. Flux-managed talk branch,
5. smoke e2e coverage,
6. separate load-test target,
7. GitOps Reverser integration for the stage-worthy story,
8. rehearsal automation and dashboards.

## What I Would Not Do

- I would not make Git commits part of the synchronous vote request.
- I would not let Flux and GitOps Reverser continuously own the same resources.
- I would not rely on the conference network for the first real test.
- I would not make the live demo depend on per-vote Git writes unless that behavior has already survived repeated load drills.

## Suggested Repo Changes

I would expect this work to land as a few separate tracks:

### 1. Talk environment docs

- a dedicated setup doc for the voting environment,
- architecture diagram for request flow and Git flow,
- rehearsal checklist,
- rollback/fallback playbook.

### 2. Deployment assets

- talk-specific manifests or Flux kustomizations,
- `cloudflared` manifests,
- TTY server background service or startup configuration,
- voting app manifests,
- optional monitoring/dashboard manifests.

### 3. Test assets

- e2e smoke for voting flow,
- separate load test harness and Make target,
- metrics capture or summary output from rehearsal runs.

## Open Questions

These are the main decisions still worth answering before implementation:

1. What exactly should GitOps Reverser record during the demo: every vote, periodic aggregates, or only app/config changes?
2. Is the voting app meant to be disposable for the talk, or should it become a reusable example in this repo?
3. Do you want the public service to survive a devcontainer restart, or is a manual recovery step acceptable?
4. Are you comfortable depending on `cloudflared` for the live talk, or do you want a second ingress fallback?
5. Is conference Wi-Fi the expected client path, or should we test explicitly from mobile data as the primary path?
6. Which TTY server do you want to standardize on for the slide integration, and how should it be started automatically?
7. Do you want Flux only for the talk environment, or is this intended to be the first real Flux integration for the repo itself?
8. What is the acceptable audience limit: 50, 100, 200?
9. What is the acceptable degraded mode: slow updates, delayed Git commits, read-only results page, or full failover?

## Concrete Next Step

The best next step is to turn this into a small implementation track:

1. define the demo contract,
2. create the talk branch and manifests skeleton,
3. ship a tiny voting app locally,
4. expose it through `cloudflared`,
5. add one smoke e2e,
6. add one separate load-test target,
7. rehearse against the real hostname.

If you want, the next pass can turn this document into a more execution-ready checklist with specific repo paths, Make targets, and proposed Flux layout.
