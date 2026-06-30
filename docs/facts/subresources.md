# Kubernetes Subresources

A factual reference on what subresources are in the Kubernetes API, which ones
exist, how they behave, and how `configbutler.ai` resources use them.

Primary source for the CRD facts below:
<https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definitions/#subresources>

## Two ways to extend the API

Kubernetes has two extension mechanisms. They look similar from the outside but
differ in what they can do.

**CustomResourceDefinitions (CRDs)** let you add new resource types served by the
main kube-apiserver:

```text
/apis/configbutler.ai/v1alpha3/namespaces/default/gittargets/example
```

A CRD supports exactly two built-in subresources, and no others:

```text
.../gittargets/example/status
.../gittargets/example/scale
```

The CRD documentation is explicit that custom resources support `/status` and
`/scale`, enabled in the CRD definition. CRDs cannot define arbitrary
subresources such as `/diff`, `/render`, `/logs`, `/restart`, or `/console`.

---

Scale subresource
When the scale subresource is enabled, the /scale subresource for the custom resource is exposed. The autoscaling/v1.Scale object is sent as the payload for /scale.

To enable the scale subresource, the following fields are defined in the CustomResourceDefinition.

specReplicasPath defines the JSONPath inside of a custom resource that corresponds to scale.spec.replicas.

It is a required value.
Only JSONPaths under .spec and with the dot notation are allowed.
If there is no value under the specReplicasPath in the custom resource, the /scale subresource will return an error on GET.
statusReplicasPath defines the JSONPath inside of a custom resource that corresponds to scale.status.replicas.

It is a required value.
Only JSONPaths under .status and with the dot notation are allowed.
If there is no value under the statusReplicasPath in the custom resource, the status replica value in the /scale subresource will default to 0.
labelSelectorPath defines the JSONPath inside of a custom resource that corresponds to Scale.Status.Selector.

It is an optional value.
It must be set to work with HPA and VPA.
Only JSONPaths under .status or .spec and with the dot notation are allowed.
If there is no value under the labelSelectorPath in the custom resource, the status selector value in the /scale subresource will default to the empty string.
The field pointed by this JSON path must be a string field (not a complex selector struct) which contains a serialized label selector in string form.
In the following example, both status and scale subresources are enabled.

Save the CustomResourceDefinition to resourcedefinition.yaml:

apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: crontabs.stable.example.com
spec:
  group: stable.example.com
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
              properties:
                cronSpec:
                  type: string
                image:
                  type: string
                replicas:
                  type: integer
            status:
              type: object
              properties:
                replicas:
                  type: integer
                labelSelector:
                  type: string
      # subresources describes the subresources for custom resources.
      subresources:
        # status enables the status subresource.
        status: {}
        # scale enables the scale subresource.
        scale:
          # specReplicasPath defines the JSONPath inside of a custom resource that corresponds to Scale.Spec.Replicas.
          specReplicasPath: .spec.replicas
          # statusReplicasPath defines the JSONPath inside of a custom resource that corresponds to Scale.Status.Replicas.
          statusReplicasPath: .status.replicas
          # labelSelectorPath defines the JSONPath inside of a custom resource that corresponds to Scale.Status.Selector.
          labelSelectorPath: .status.labelSelector
  scope: Namespaced
  names:
    plural: crontabs
    singular: crontab
    kind: CronTab
    shortNames:
    - ct

---


**Aggregated API servers** are needed for arbitrary subresources. The Kubernetes
aggregation layer lets you register an `APIService` that claims an API path; the
main kube-apiserver then proxies requests for that API group to your own API
server, which can implement any subresource and any behavior.

| | CRD | Aggregated API server |
| --- | --- | --- |
| New resource types | yes | yes |
| Subresources available | `/status`, `/scale` only | any |
| Custom API behavior | no | yes |
| Operational cost | low (served by kube-apiserver) | high (you run an API server) |

## What a subresource is

A subresource is not a child object. It is a **separate API surface** attached to
the same resource. Each subresource can have different verbs, different
request/response shapes, and different RBAC permissions.

For a Pod, all of these address the same object but mean different things:

```text
/api/v1/namespaces/default/pods/nginx
/api/v1/namespaces/default/pods/nginx/status
/api/v1/namespaces/default/pods/nginx/log
/api/v1/namespaces/default/pods/nginx/exec
```

RBAC treats subresources separately, using `resource/subresource` slash notation.
Granting access to a Pod's logs is distinct from granting access to the Pod:

```yaml
rules:
  - apiGroups: [""]
    resources:
      - pods
      - pods/log
    verbs: ["get"]
```

So a subresource is a separate contract and a separate security boundary, not
just a different URL for the same data.

## CRD subresource: `/status`

The `/status` subresource splits a resource into user-owned desired state
(`spec`) and controller-owned observed state (`status`):

```yaml
spec:
  # what the user wants
status:
  # what the controller has observed or done
```

When the status subresource is enabled, the API enforces this split
(per the CRD documentation):

- `PUT`/`POST`/`PATCH` to the main resource endpoint **ignore changes to the
  `status` stanza**.
- `PUT` to the `/status` endpoint **ignores changes to everything except
  `status`**.
- `.metadata.generation` is incremented for all changes **except** changes to
  `.metadata` or `.status`. So a status-only write does not bump the generation;
  a spec change does.

In a kubebuilder/controller-runtime project the subresource is enabled with a
marker on the type:

```go
// +kubebuilder:subresource:status
```

Every `configbutler.ai` CRD enables `/status`: `GitProvider`, `GitTarget`,
`WatchRule`, `ClusterWatchRule`, and `CommitRequest`. A real `GitTarget`:

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
metadata:
  name: platform
  namespace: default
spec:
  providerRef:
    name: platform-config
  branch: main
  path: clusters/dev
status:
  observedGeneration: 4
  lastReconcileTime: "2026-06-08T10:00:00Z"
  conditions:
    - type: Ready
      status: "True"
      reason: OK
```

The user owns `spec`; the controller owns `status`. `observedGeneration` is what
lets a reader tell whether the reported conditions describe the current `spec` or
a stale one — a `Ready: "True"` condition is only trustworthy when
`status.observedGeneration` equals `metadata.generation`.

## CRD subresource: `/scale`

`/scale` is a standardized scaling interface. When enabled, the `/scale`
endpoint exposes an `autoscaling/v1.Scale` view over the resource. The CRD
declares which JSONPaths back the Scale object:

```yaml
subresources:
  scale:
    specReplicasPath: .spec.replicas      # -> scale.spec.replicas (required, under .spec)
    statusReplicasPath: .status.replicas  # -> scale.status.replicas (required, under .status)
    labelSelectorPath: .status.selector   # -> scale.status.selector (optional; required for HPA/VPA)
```

The `/scale` endpoint sends and receives an `autoscaling/v1.Scale` payload, not
the full resource. This is what makes a custom resource work with the standard
tooling:

```bash
kubectl scale customsets.apps.example.com/foo --replicas=5
```

and with HorizontalPodAutoscaler / VerticalPodAutoscaler consumers. It is a
scaling contract specifically — it is only meaningful for resources that
represent a replicated, scalable workload, for example:

```yaml
apiVersion: apps.example.com/v1
kind: CustomSet
spec:
  replicas: 3
status:
  replicas: 2
  selector: app=my-custom-set
```

No `configbutler.ai` resource enables `/scale`: `GitProvider`, `GitTarget`,
`WatchRule`, `ClusterWatchRule`, and `CommitRequest` are not scalable workloads,
so a replica count has no meaning for them.

## Audit logs and the `/scale` subresource

Because a subresource is a separate API surface, a write to it produces a
*separate* audit event — and that has a direct consequence for anything that
reconstructs object state from the Kubernetes audit stream, as gitops-reverser
does.

When a user runs:

```bash
kubectl scale deployment/scale-audit-target --replicas=3
```

kubectl does **not** patch the main Deployment endpoint or send a full
Deployment object. It issues a `PATCH` against the Deployment's `scale`
subresource. The apiserver persists that as a real parent Deployment
`.spec.replicas` change, but it records exactly one audit event for the request
— keyed to the subresource and carrying an `autoscaling/v1.Scale` payload, not a
Deployment. See
[deployment-scale-subresource.json](../../internal/webhook/testdata/audit-events/deployment-scale-subresource.json)
(abridged):

```jsonc
{
  "verb": "patch",
  "requestURI": ".../deployments/scale-audit-target/scale",
  "objectRef": {
    "resource": "deployments",  // the parent resource...
    "subresource": "scale"      // ...addressed via its scale subresource
  },
  "requestObject":  { "spec": { "replicas": 3 } },
  "responseObject": {
    "kind": "Scale",
    "apiVersion": "autoscaling/v1",
    "metadata": {
      "resourceVersion": "6977"
    },
    "spec":   { "replicas": 3 },
    "status": { "replicas": 0, "selector": "app=scale-audit-target" }
  }
}
```

In this capture, a normal Deployment read after the scale showed the parent
Deployment at the same `metadata.resourceVersion` (`6977`) with
`.spec.replicas: 3`. That is the key local proof for gitops-reverser: the
`Scale` response is not a separate durable object to commit, but it does expose
the accepted desired-state change that was just persisted onto the parent
Deployment.

**The important fact: no "normal", complete PATCH/UPDATE audit event against the
Deployment itself ever arrives.** The apiserver does update the Deployment's
`.spec.replicas` internally, but in the audit stream that change is visible
*only* through the scale subresource event above. A consumer that watches the
parent resource and ignores subresource events (filtering on
`objectRef.subresource == ""`) silently misses every `kubectl scale` and every
HPA-driven replica change — the desired state of the workload would drift from
what is captured in Git with no event to explain it.

That is exactly why gitops-reverser supports the scale subresource explicitly. It
recognizes the `subresource: "scale"` event, reads the desired count from the
`Scale` payload's `spec.replicas` (here `3`) — **not** `status.replicas`, which
is the *observed* count and is `0` in this capture — and translates it into a
field patch on the parent Deployment's `.spec.replicas`, so the scaling change is
materialized into Git like any other update.

## Built-in subresources

The built-in Kubernetes API uses a richer set of subresources than CRDs can.
Pods expose the most:

```text
pods/status              # controlled mutation of observed state
pods/resize              # in-place resource resize
pods/ephemeralcontainers # add debug containers, not allowed at create time
pods/log                 # derived read representation
pods/exec                # streaming/interactive
pods/attach              # streaming/interactive
pods/portforward         # streaming/interactive
pods/proxy               # proxy
pods/eviction            # policy-aware action
```

Ephemeral containers illustrate why a field gets its own subresource: they
cannot be set on a normal Pod create/update and must be added through the
`pods/ephemeralcontainers` subresource.

The Pod eviction endpoint is a policy-aware action — it accepts an `Eviction`
object, not a full Pod, and applies PodDisruptionBudget policy:

```http
POST /api/v1/namespaces/{namespace}/pods/{name}/eviction
```

Services expose a proxy subresource that forwards through the apiserver toward
service endpoints:

```text
/api/v1/namespaces/default/services/my-service/proxy
/api/v1/namespaces/default/services/my-service/proxy/{path}
```

The common thread: Kubernetes uses a subresource when the operation is not a
normal CRUD operation on the parent object.

## Aggregated API server subresources

Arbitrary subresources require an aggregated API server.

**KubeVirt** is a clear example. Alongside its VM resources it serves a separate
subresource API group, `subresources.kubevirt.io`, for operations that cannot be
modeled as declarative object updates — console access, VNC, restart, and
similar. RBAC for these is granted on the subresource:

```yaml
rules:
  - apiGroups:
      - subresources.kubevirt.io
    resources:
      - virtualmachines/console
      - virtualmachines/vnc
    verbs:
      - get
```

Concrete operations from that API group:

```http
GET /apis/subresources.kubevirt.io/v1/namespaces/{ns}/virtualmachineinstances/{name}/console
GET /apis/subresources.kubevirt.io/v1/namespaces/{ns}/virtualmachineinstances/{name}/vnc
PUT /apis/subresources.kubevirt.io/v1/namespaces/{ns}/virtualmachines/{name}/restart
```

**metrics-server** also uses aggregation, but differently: it serves an entire
extra API (`metrics.k8s.io`) through the aggregation layer rather than adding a
`resource/subresource` to an existing type.

**sample-apiserver** is the reference implementation for building an extension
API server on the `k8s.io/apiserver` library. Its own README notes that CRDs are
simpler when all you need is a new resource type.

`configbutler.ai` does not run an aggregated API server and exposes no custom
subresources; everything is served by the main kube-apiserver as CRDs.

## Subresource categories

Across built-in APIs and CRDs, subresources fall into a few categories.

| Category | Mechanism | Examples |
| --- | --- | --- |
| Controller-owned observed state | `/status` | `pods/status`, `deployments/status`, `gittargets/status` |
| Standardized cross-type view | `/scale` | `deployments/scale`, `statefulsets/scale` |
| Streaming / interactive | built-in / aggregated API | `pods/exec`, `pods/attach`, `virtualmachineinstances/console` |
| Alternate read representation | built-in / aggregated API | `pods/log` |
| Policy-aware action | built-in / aggregated API | `pods/eviction`, `virtualmachines/restart` |
| Special controlled mutation | built-in | `pods/ephemeralcontainers`, `pods/resize` |

A few of these are only available on built-in types or through an aggregated API
server; a plain CRD cannot add them.

## Request resources vs. imperative subresources

For an imperative operation, Kubernetes API design has two options: a custom
subresource (requires an aggregated API server), or a **request resource** — a
normal CRD that represents "please do X", carrying its outcome in `status`.

`configbutler.ai` uses the request-resource pattern with **`CommitRequest`**.
Instead of a hypothetical `POST /gittargets/{name}/commit` subresource, creating
a `CommitRequest` object finalizes the open commit window for the referenced
`GitTarget` and reports the result back in status:

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: CommitRequest
metadata:
  name: save-now
  namespace: default
spec:
  gitTargetRef:
    name: platform
  message: "Manual save"
status:
  conditions:
    - type: Ready        # summary: True once it reached a non-error terminal outcome
      status: "True"
      reason: Committed  # Committed | NoWindowInGrace | WindowMismatch | AlreadyPresent | FinalizeFailed
    - type: Attributed   # True immediately when attribution is not required (committer-only)
      status: "True"
      reason: AttributedFromAuditEvent
    - type: Pushed       # True once the commit is in the remote repository
      status: "True"
      reason: Pushed
  branch: main
  sha: abc123def
```

Because it is an ordinary resource, a request CRD automatically gets
`kubectl get`/`describe`/`wait`, watch support, status conditions, RBAC,
an audit trail, and durable retry/reconciliation — none of which a one-shot
subresource call provides. This is why it is the GitOps-friendly way to model an
action: the request itself is a durable, inspectable object.

## Choosing a mechanism

| Need | Mechanism |
| --- | --- |
| Durable declarative intent | CRD |
| Controller reports observed state | CRD + `/status` |
| HPA / `kubectl scale` compatibility | CRD + `/scale` |
| One-off declarative job/request | Request CRD (e.g. `CommitRequest`) |
| Derived synchronous or streaming output | Aggregated API subresource |
| Proxy / tunnel / console / session | Aggregated API subresource |
| Imperative action | Request CRD, or aggregated API subresource if it must be synchronous/streaming |

The deciding question is whether the thing is **state** or an **operation**:

- Desired state → a resource (`spec`).
- Observed state → `status`.
- A live operation, stream, derived view, or policy-aware action → a subresource
  (built-in or aggregated API).

## Summary

The pattern Kubernetes follows:

| Intent | Mechanism |
| --- | --- |
| Normal state | resource |
| Controller-owned observation | `/status` |
| Common cross-type protocol | `/scale` |
| Live / dynamic / interactive behavior | subresource (aggregated API for custom types) |
| Arbitrary imperative operation | request resource, or aggregated API subresource |

`configbutler.ai` stays entirely within CRDs: every type is served by the main
kube-apiserver, every type uses `/status`, none use `/scale`, and imperative
actions are modeled as request resources (`CommitRequest`) rather than custom
subresources.
