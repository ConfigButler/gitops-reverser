# Mutation-capture lab on its own cluster

Status: built, 2026-10-27. [As built](#as-built) lists where the build departs from this design.

The [mutation-capture lab](../spec/mutation-capture-lab-design.md) records what Kubernetes emits
through watches, audit, admission, CRD conversion, and an aggregated API, and commits it as a
corpus under [`test/mutationlab/corpus/`](../../test/mutationlab/corpus/). That corpus is evidence
the product relies on: `internal/queue/fact_index_corpus_test.go` and
`fact_index_selector_corpus_test.go` replay it as fixtures. It is also how a new Kubernetes
version is judged: capture again, and the corpus diff is the behavioral changelog.

This page designs the lab as a cluster of its own, which can run beside the e2e cluster, and lists
the requirements that keep the capture faithful and the lab working.

## What broke, and why it could

The lab never owned its wiring. `task lab-e2e` swapped the lab image into the product's controller
Deployment on the e2e cluster (`swap-image.sh`, since removed) and borrowed
everything around it:

| The lab needs | It came from | Since `fd6f6933` |
|---|---|---|
| an admission webhook registration for `/validate-all` | the product's `ValidatingWebhookConfiguration` | deleted |
| a serving certificate, port 9443, and its mount | the product's `Certificate`, `Service`, and Deployment | deleted |
| the CRD conversion webhook (row 14) | the CA bundle and Service read off that registration | broken with it |
| audit deliveries | the API server's webhook, aimed at the product's NodePort and route, with the product's certificates | works |
| a pod, an identity, and RBAC | the product's Deployment and ServiceAccount | works, by accident |

The driver's readiness check waits for an admission record, so the run now fails before its first
scenario, although audit still flows.

The deletion only exposed the root cause: nothing runs the lab, so a product change could remove
what the lab depended on, and every check stayed green.

## Decision

The lab gets its own k3d cluster, `gitops-reverser-capture-lab`, which runs the lab and nothing of
the product. Every source the corpus records arrives the way it does on any cluster:

- the API server's **audit webhook backend** posts batches to the lab over mutual TLS;
- a **`ValidatingWebhookConfiguration`** sends admission reviews to the lab's Service;
- the **CRD conversion webhook** of the scenario Widget CRD calls the lab's Service;
- the **sample aggregated API server** (`wardle`) serves flunders behind an `APIService`;
- the lab **watches** through its own ServiceAccount.

Nothing is replayed, proxied, or read from a file. That is the point: a new Kubernetes version is
tested by creating the lab cluster on it, and only real deliveries say what that version does.

A namespace on the e2e cluster cannot get there. The API server takes one audit webhook, set when
the cluster is created, and on the e2e cluster it belongs to the product. Sharing it brings back
the swap.

## Design

```text
 host: task lab-capture ── go test driver ── port-forward :18081 ─────────────┐
                                                                              │
 k3d cluster gitops-reverser-capture-lab (one node, k3s pinned by the lab)    │
 ┌────────────────────────────────────────────────────────────────────────────┼─┐
 │ kube-apiserver                                                             │ │
 │   audit webhook backend ── mTLS ── 127.0.0.1:30444 (NodePort) ─┐           │ │
 │   ValidatingWebhookConfiguration mutation-capture-lab ─────────┤           │ │
 │   CRD conversion (Widget) ─────────────────────────────────────┤           │ │
 │   APIService v1alpha1.wardle.example.com ── wardle-server      │           │ │
 │                                                                ▼           ▼ │
 │   namespace mutation-capture-lab:  Deployment mutation-capture-lab          │
 │     :9444 audit (mTLS) · :9443 /validate-all + /convert · :8080 records API │
 │     watches: configmaps, deployments, pods, widgets, flunders               │
 └─────────────────────────────────────────────────────────────────────────────┘
```

### The cluster

[`start-cluster.sh`](../../test/e2e/cluster/start-cluster.sh) creates it, with:

- `CLUSTER_NAME=gitops-reverser-capture-lab`, so the context is `k3d-gitops-reverser-capture-lab`
  and its stamps live under `.stamps/cluster/k3d-gitops-reverser-capture-lab/`;
- `K3S_IMAGE` from `LAB_K3S_IMAGE`, which defaults to the image recorded in
  [`CLUSTER.md`](../../test/mutationlab/corpus/CLUSTER.md), independent of the e2e cluster's;
- `AUDIT_DIR_REL` pointing at the lab's stamp directory, which holds the e2e audit policy, copied
  from the one tracked file, and the lab's final audit webhook kubeconfig;
- `K3D_AGENT_COUNT=0`. The e2e cluster runs an agent because it restarts its server node to load
  the audit kubeconfig; the lab never restarts, so one node is enough.

The API server's audit flags, batch settings included, come from the script unchanged, so both
clusters deliver audit the same way.

### Certificates, before the cluster exists

The e2e cluster restarts its server because cert-manager issues the audit certificates only after
the cluster is up, and the API server reads its audit kubeconfig only at startup. The lab issues
its certificates first, with a small Go command, into the lab's stamp directory:

- one CA;
- a serving certificate for `mutation-capture-lab.mutation-capture-lab.svc`, used by the audit,
  admission, and conversion servers;
- a client certificate for the API server.

The audit kubeconfig is then complete when the cluster is created: server
`https://127.0.0.1:30444/audit-webhook`, the CA, the client certificate, and the Service's DNS
name as `tls-server-name`. The lab requires and verifies that client certificate. The swapped lab
never did, because the swap passed no client CA.

After creation, the serving certificate and CA go into the cluster as Secrets, and the CA is
written into the registration's `caBundle` when it is applied. The driver writes it into the Widget
CRD's conversion `caBundle` from the stamp directory. cert-manager is not installed: the lab's own
certificates have no rotation to manage, and cert-manager arrives on the e2e cluster only through
the whole Flux stack.

### The deployment

`test/mutationlab/deploy/`, a kustomization, holds:

- namespace `mutation-capture-lab`;
- ServiceAccount `mutation-capture-lab`, and a ClusterRole that grants `get`, `list`, and `watch`
  on what `--watch-resources` names, nothing else;
- the Deployment, one replica, so every audit batch reaches the one store;
- Service `mutation-capture-lab`: ClusterIP 443 to 9443 for admission and conversion, NodePort
  30444 to 9444 for audit, and 8080 for the records API;
- `ValidatingWebhookConfiguration` `mutation-capture-lab`, with the rules the corpus was captured
  under: `resources: ["*"]` top-level only, `CREATE`, `UPDATE`, `DELETE`, `scope: "*"`,
  `matchPolicy: Equivalent`, `failurePolicy: Ignore`, `timeoutSeconds: 1`. Rows 5 and 6 record no
  admission for `/status` and `/scale` because those rules match no subresources; Kubernetes would
  send them if asked.

The sample aggregated API comes from
[`test/e2e/setup/manifests/sample-apiserver/`](../../test/e2e/setup/manifests/sample-apiserver/),
which gains a `kustomization.yaml` so the e2e setup and the lab apply the same directory.

### Tasks

The lab's tasks move into their own Taskfile, `test/mutationlab/Taskfile-lab.yml`, included flat
like the e2e one. The verbs follow the playground's:

| Task | Does |
|---|---|
| `lab-bootstrap` | issues the certificates, creates the cluster, builds and imports the lab image, applies the deployment and the sample API server, and waits until every source is live |
| `lab-capture` | runs the scenarios against the lab cluster and compares the capture to the corpus |
| `lab-corpus-update` | runs the scenarios and writes the capture as the new corpus, `CLUSTER.md` included |
| `lab-status` | shows the cluster, its k3s version, the lab pod, and how many records it holds |
| `lab-cleanup` | deletes the cluster and its stamp directory |

`lab-e2e`, `swap-image.sh`, and the lab's dependency on `_controller-deployed` are removed.

Testing a new Kubernetes version:

```bash
task lab-cleanup
task lab-bootstrap LAB_K3S_IMAGE=rancher/k3s:v1.38.0-k3s1
task lab-corpus-update
git diff test/mutationlab/corpus/
```

### Beside the e2e cluster

Both clusters can run at once. What each one owns:

| Concern | e2e | lab |
|---|---|---|
| cluster and context | `gitops-reverser-test-e2e` | `gitops-reverser-capture-lab` |
| stamps | `.stamps/cluster/k3d-gitops-reverser-test-e2e/` | `.stamps/cluster/k3d-gitops-reverser-capture-lab/` |
| API port on the host | picked by k3d | picked by k3d |
| NodePort 30444 | inside its own node container | inside its own node container |
| run lock | `e2e.lock` in its stamps | `lab.lock` in its stamps |
| host port-forward | Gitea, Prometheus, Valkey, Flux, Argo CD | records API on 18081 |
| k3s image | the e2e pin | `LAB_K3S_IMAGE` |

`start-cluster.sh` switches the current kube context to the cluster it creates. The lab passes a
new `K3D_SWITCH_CONTEXT=false`, and the script's devcontainer kubeconfig rewrite reads the named
context rather than the current one. Otherwise bootstrapping the lab would move a person's
`kubectl`, or an e2e run started without `CTX`, onto the lab cluster.

## What is reused

| Piece | How |
|---|---|
| `start-cluster.sh` | as is, plus `K3D_SWITCH_CONTEXT` and a context-scoped kubeconfig rewrite |
| the audit policy | the same tracked file, so the corpus shows what the product's clusters audit |
| the audit flags and batch settings | from the script, unchanged |
| the sample aggregated API server | the same manifests, through a new `kustomization.yaml` |
| the lab binary, recorders, normalizer, corpus, and driver | unchanged, apart from the client-CA flag, the context, and the readiness check |
| the content-hashed lab image and its build | unchanged |
| `lab-port-forward.sh` | with the lab's namespace and selector |
| the audit kubeconfig | written by the certificate command, not by `generate-audit-webhook-kubeconfig.sh` (see [As built](#as-built)) |

Not reused: `inject-webhook-tls.sh`, whose node restart the lab avoids, and the Flux stack.

## Requirements

Each one names the failure it prevents.

1. **Real deliveries only.** Every record arrives from the API server or the aggregated API server
   through its own mechanism: the audit webhook backend, a webhook registration, a CRD conversion
   webhook, a watch. No log scraping, replay, or proxy. Otherwise a new Kubernetes version is
   judged through a translation that may hide what changed.
2. **Mutual TLS on audit, verified.** The API server presents its client certificate and the lab
   checks it, as a production receiver does. Otherwise the lab records deliveries such a receiver
   would refuse.
3. **The registration's match rules stay those the corpus was captured under.** Otherwise rows 5
   and 6 change meaning while looking like a Kubernetes change.
4. **One audit policy file, shared with e2e.** Otherwise the corpus drifts from what the product's
   clusters audit.
5. **Self-contained.** No lab manifest, script, or task names a product resource or depends on a
   product task. A static check enforces it. Otherwise the next product change breaks the lab
   unnoticed, as this one did.
6. **Coexistence.** The lab has its own cluster name, stamps, lock, and port, and never switches
   the current kube context. Otherwise running it beside e2e corrupts one of the two.
7. **Every source live before the first scenario.** `lab-bootstrap` ends by proving audit,
   admission, conversion, the aggregated API, and the watch each with a probe write it then finds
   among the records. Today's check covers admission alone.
8. **The version is pinned and recorded by the tool.** `lab-corpus-update` writes the k3s image
   and the server version into `CLUSTER.md`. Otherwise the corpus and its provenance disagree, as
   they did when e2e moved to 1.37 and the corpus stayed on 1.36.4.
9. **Something runs it.** A scheduled CI job runs `lab-bootstrap` and `lab-capture`, and fails on
   any corpus diff. Without it the other requirements hold only until the next unnoticed change.

## Build order

1. `K3D_SWITCH_CONTEXT` and the context-scoped rewrite in `start-cluster.sh`, and the
   `kustomization.yaml` for the sample API server.
2. The certificate command, and the lab's client-CA verification.
3. `test/mutationlab/deploy/` and `Taskfile-lab.yml` with `lab-bootstrap`, `lab-status`, and
   `lab-cleanup`.
4. The driver: the lab context, the CA from the stamps, and the five-source readiness check. Then
   `lab-capture` and `lab-corpus-update`; delete `lab-e2e` and `swap-image.sh`.
5. A capture on the pinned k3s v1.36.4, committed on its own: its diff must be empty, or explained
   line by line, because only the delivery path changed.
6. A second commit that moves the pin to the e2e cluster's version, so its diff is the Kubernetes
   change alone.
7. The scheduled CI job, the static self-containment check, and the lab's README and
   [spec](../spec/mutation-capture-lab-design.md#isolated-test-setup) rewritten to match.

## As built

- **Two Services, not one.** `mutation-capture-lab` is a ClusterIP Service for admission,
  conversion (443 to 9443), and the records API (8080); `mutation-capture-lab-audit` is the NodePort
  30444 to 9444. A NodePort Service gives every port it lists a node port.
- **The certificate command writes the audit kubeconfig.** `generate-audit-webhook-kubeconfig.sh`
  defaults to the product's names and reads Secrets; requirement 5 keeps the lab off it.
- **The webhook keeps the name `validate-all.configbutler.ai`.** The API server keys the admission
  recorder's audit annotation by it (`validate-all.configbutler.ai/scenario`) and quotes it in row
  12's rejection, so the name is part of the corpus, like the match rules of requirement 3.
- **The checks are tests in the driver.** `TestSourcesLive` is the five-source check:
  `lab-bootstrap` runs it, and `lab-capture` skips it. `TestClusterProvenance` writes `CLUSTER.md`
  on an update and fails a capture when the cluster serves another version. The static check is
  [`selfcontained_test.go`](../../test/mutationlab/selfcontained_test.go), in `task test`.
- **The capture on v1.36.4 differs in one field.** 19 of 21 scenarios reproduced the corpus byte for
  byte. The other two, rows 8a and 19, gained `authenticationMetadata.impersonationConstraint:
  impersonate:user-info` on each of their 15 impersonated audit events, and on nothing else. A plain
  k3s v1.36.1 and v1.36.4 cluster with an audit log backend and none of the lab's wiring records the
  same field for the same request, so it comes from the API server (constrained impersonation, beta
  in 1.36), not from the new delivery path. Why the committed rows, captured on the e2e cluster,
  lack it is not known.
- **Memory.** The lab cluster's node used about 600 MiB with the lab and the sample API server
  running, so `lab-capture` does not take the e2e lock.
- **CI.** [`mutation-lab.yml`](../../.github/workflows/mutation-lab.yml) runs `lab-capture` nightly
  and on manual dispatch.
