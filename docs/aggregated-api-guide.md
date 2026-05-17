# Capturing aggregated API server objects

GitOps Reverser learns about cluster changes from kube-apiserver audit events. For **core
Kubernetes resources** (Deployments, ConfigMaps, Secrets, CRDs and their custom resources, and so
on) those events carry the full object, so no extra setup is needed.

Resources served by an **aggregated API server** (a separate backend registered through an
`APIService`) are different. kube-apiserver proxies those requests to the backend and cannot see
the backend's request and response bodies. The audit events it emits for them are "shallow": they
record who did what and when, but not the object itself.

## Which setup do you need?

| Situation | Advice |
|---|---|
| I don't need aggregated API resources | Nothing to do. The standard install already captures all core resources. You can stop here. |
| I only need aggregated API resources | Run `apiservice-audit-proxy` pointed at `/audit-webhook`. This skips the kube-apiserver audit webhook setup entirely. See [Proxy only](#proxy-only-capture-just-aggregated-api-objects). |
| I need both aggregated API and normal resources | Run `apiservice-audit-proxy` pointed at `/audit-webhook-additional`, alongside kube-apiserver on `/audit-webhook`. See [Alongside kube-apiserver](#alongside-kube-apiserver-capture-everything). |

If you are unsure whether this applies to you, run `kubectl get apiservices` and look at the
`SERVICE` column:

```
NAME                     SERVICE                      AVAILABLE   AGE
v1.apps                  Local                        True        90d
v1beta1.metrics.k8s.io   kube-system/metrics-server   True        90d
```

`Local` means kube-apiserver serves that group itself (all built-in resources, plus anything from
CRDs). A real `<namespace>/<name>` Service means an aggregated API server. Those are the rows this
guide is about.

Two things have to be true before you need the proxy: a group is Service-backed, *and* you
actually want its objects committed to Git. Most clusters only have `metrics.k8s.io`
(`metrics-server`), whose objects are ephemeral CPU and memory readings that nobody wants in
version control. The proxy earns its keep when an aggregated API serves real configuration you
would want reviewed and versioned. [Cozystack](https://cozystack.io/) is a clear example: its
`apps.cozystack.io` group exposes tenant resources such as managed databases, virtual machines,
and Kubernetes clusters through an aggregated API server, and those are exactly the objects a team
would want captured in Git.

## Why this needs special setup

GitOps Reverser needs the object body to write YAML to Git. A shallow audit event has no body, so
it cannot be turned into a manifest. To avoid writing meaningless stub commits, events that arrive
with no object body are dropped, counted in `gitopsreverser_audit_shallow_dropped_total` and
logged at WARN.

The practical consequence: **out of the box, changes to aggregated-API objects are not written to
Git.**

## How to capture them

Deploy [`apiservice-audit-proxy`](../external-sources/apiservice-audit-proxy/) in front of the
aggregated backend. It is itself a pass-through aggregated API server: kube-apiserver routes the
`APIService` to the proxy, the proxy forwards to the real backend, observes the request and
response bodies, and emits a complete synthetic `audit.k8s.io/v1` event.

There are two ways to wire it up.

### Alongside kube-apiserver: capture everything

The proxy posts to GitOps Reverser's **`/audit-webhook-additional`** endpoint, while
kube-apiserver keeps posting native events to `/audit-webhook`:

```
kube-apiserver          --official EventList-->  /audit-webhook
apiservice-audit-proxy  --body EventList------>  /audit-webhook-additional
```

The operator's audit joiner pairs the proxy-supplied body with the official kube-apiserver event
by `auditID` and emits one complete event into the pipeline. Use this when you want both core and
aggregated-API resources in Git.

### Proxy only: capture just aggregated-API objects

If you *only* care about aggregated-API objects, point the proxy straight at **`/audit-webhook`**
and leave kube-apiserver out of it entirely:

```
apiservice-audit-proxy  --complete EventList-->  /audit-webhook
```

Events on `/audit-webhook` are the canonical source and drive Git writes directly. The proxy
already supplies a complete event, so no joining is needed.

This is a meaningfully simpler install: it skips configuring the kube-apiserver audit webhook
backend, which needs admin access to control-plane flags and is the most awkward part of the
standard setup. Registering the proxy as an `APIService` is a normal in-cluster operation.

The trade-off is in the name. GitOps Reverser sees **only** the aggregated-API resources that
pass through the proxy. Changes to core resources (Deployments, ConfigMaps, Secrets, and so on)
are not captured, because nothing posts them to `/audit-webhook`.

### Reference

Setup for the proxy itself (TLS, `APIService` registration, the webhook kubeconfig) is in the
[`apiservice-audit-proxy` README](../external-sources/apiservice-audit-proxy/README.md). The
operator-side endpoints and their Helm values (`auditEventJoin.*`) are in the
[chart README](../charts/gitops-reverser/README.md). The full ingestion pipeline (the joiner,
deduplication, and the timing assumption it relies on) is described in
[architecture.md](architecture.md#audit-ingestion-pipeline).
