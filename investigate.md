## Aggregated API Investigation

### Question

Why does a `Flunder` create from the aggregated API not give us enough data in the audit webhook path, even though the resource itself seems to behave normally?

### What We Confirmed

1. A normal core resource create, like `ConfigMap`, produces a rich audit event.

   The audit payload contains:
   - `objectRef.name`
   - `requestObject`
   - `responseObject`

   Evidence:
   - [configmap-create-audit.json](./.stamps/debug/configmap-create-audit.json)

2. An aggregated `Flunder` create produces a sparse audit event.

   The audit payload contains:
   - `objectRef.resource`
   - `objectRef.namespace`
   - `objectRef.apiGroup`
   - `objectRef.apiVersion`
   - collection-style `requestURI`

   The audit payload does not contain:
   - `objectRef.name`
   - `requestObject`
   - `responseObject`

   Evidence:
   - [flunder-create-audit.json](./.stamps/debug/flunder-create-audit.json)

3. This is not a general aggregated API serving problem.

   Direct kube-apiserver responses for `Flunder` are complete.

   We captured:
   - `GET` response
   - `LIST` response
   - `WATCH` event stream

   All of them contain the full object payload for `Flunder`, including `metadata` and `spec`.

   Evidence:
   - [flunder-get-response.json](./.stamps/debug/kubeapi/flunder-get-response.json)
   - [flunder-list-response.json](./.stamps/debug/kubeapi/flunder-list-response.json)
   - [flunder-watch-response.jsonl](./.stamps/debug/kubeapi/flunder-watch-response.jsonl)
   - [flunder-kubectl-get.yaml](./.stamps/debug/kubeapi/flunder-kubectl-get.yaml)

   For comparison, we also captured the same responses for `ConfigMap`:
   - [configmap-get-response.json](./.stamps/debug/kubeapi/configmap-get-response.json)
   - [configmap-list-response.json](./.stamps/debug/kubeapi/configmap-list-response.json)
   - [configmap-watch-response.jsonl](./.stamps/debug/kubeapi/configmap-watch-response.jsonl)
   - [configmap-kubectl-get.yaml](./.stamps/debug/kubeapi/configmap-kubectl-get.yaml)

### Conclusion

The problem appears to be specific to the audit event shape for aggregated `create` requests in this setup, not to normal object serving by kube-apiserver.

Put differently:

- kube-apiserver can return full `Flunder` objects on `GET`
- kube-apiserver can return full `Flunder` objects on `LIST`
- kube-apiserver can return full `Flunder` objects on `WATCH`
- but the audit event for aggregated `Flunder` `create` is missing the fields we need to identify and hydrate the object

### Why This Is Serious

With the current audit payload, we do not have a reliable way to map the aggregated `create` event to a specific object.

Specifically, the audit event is missing:

- object name
- full request body
- full response body
- UID

That means we cannot safely derive:

- which Git file path to write
- which watched object event belongs to this audit event
- whether a nearby watch event is definitely the same create request

Time-window matching would be fragile and unsafe.

### Important Follow-Up Observation

The repository already has a separate watch/informer path that can see full objects.

However, the current live ingestion design treats audit as authoritative for live mutations, so the live watch path is not being used to write these aggregated create events today.

This means:

- the data probably exists on the watch side
- the data is not currently available on the active live write path for aggregated creates

### About Returning Metadata From Our Server

This is a good instinct, but based on what we observed, it may not be enough by itself.

Reason:

- the direct `GET/LIST/WATCH` responses already include full `Flunder` metadata and spec
- yet the audit event for `create` still drops `objectRef.name`, `requestObject`, and `responseObject`

So the issue does not look like "the sample apiserver never returns metadata".

It looks more like:

- the aggregated create request is being served correctly
- but the audit machinery in this environment is not recording the aggregated create with the same richness as a core resource create

That means changing the server implementation might help only if it changes how the audit layer records aggregated creates. We do not have evidence for that yet.

### Best Current Hypothesis

This is likely one of:

1. A k3s-specific audit behavior for aggregated creates.
2. A broader kube audit behavior for aggregated API creates.
3. A behavior specific to the sample-apiserver aggregation path.

What it does not currently look like:

1. A broken `Flunder` object schema.
2. A broken `GET/LIST/WATCH` response path.
3. A bug in our audit JSON extraction logic.

### Practical Next Questions

1. Does upstream Kubernetes or `kind` produce richer audit events for the same aggregated create?
2. If not, should live aggregated creates be handled from the watch/object path instead of audit-only data?
3. Can we attach audit identity to a later watch object in a deterministic way, or is that too unsafe?

## Talk Implications

### What This Means For A Demo Or Talk

This finding does not mean the whole idea breaks.

It means the current design has two different strengths:

- state capture still looks viable
- audit-quality attribution is incomplete for this aggregated API case

So if this system is demonstrated with an aggregated API like `Flunder`, the honest claim is not:

- "we fully reconstruct every change including exact actor provenance"

The more accurate claim is:

- "we can still reconstruct cluster state into Git"
- "but for this aggregated API path we may not be able to preserve the original author from audit data alone"

### What Still Works

Based on the investigation so far, a talk can still confidently show:

- the aggregated API is installed and usable
- objects can be created and read normally
- kube-apiserver returns full objects on `GET`, `LIST`, and `WATCH`
- the system can likely mirror those objects from the watch or snapshot side

### What Cannot Be Claimed Cleanly

For this specific aggregated API setup, the current live audit path does not support a strong claim that:

- every create event can be mapped back to a specific object using audit alone
- the original end user can always be attached to the Git commit for aggregated creates
- aggregated API resources behave like built-in resources in the audit stream

### Possible Demo Positioning

There are a few honest ways to frame this in a talk.

#### 1. Watch-First Or State-First Mode

You can say that the system supports aggregated APIs through:

- discovery
- watch
- snapshot

Tradeoff:

- Git state can still be reconstructed
- original request author may be unavailable for some live mutations

This is likely the cleanest fallback if the focus is on reverse GitOps as a concept rather than strict provenance.

#### 2. Hybrid Provenance Story

You can say that:

- built-in resources currently have strong audit-backed provenance
- aggregated APIs may require degraded handling
- the system can fall back to watch/object data when audit data is incomplete

Tradeoff:

- not all resource types have the same attribution guarantees

This is actually a strong "what if..." message, because it shows a real platform boundary instead of hiding it.

#### 3. Strategic Recommendation Toward CRDs

You can also say that a next version could prefer CRDs when strong reverse-audit semantics are required.

The careful framing is:

- not "aggregated APIs are broken"
- but "our current ingestion model aligns better with CRDs and native-style resource behavior"

### Suggested Product Framing

A useful way to summarize the current situation is:

- reliable state mirroring: yes
- reliable author attribution for aggregated creates: no, not from current audit data alone
- production-ready without caveats for this API shape: not yet
- valuable as a real-world exploration for a "what if..." talk: yes

### Possible Mode Split

One practical way to present the roadmap is to describe two modes:

- `provenance=strict`
- `provenance=best-effort`

Where:

- `provenance=strict` only accepts event paths with strong audit identity
- `provenance=best-effort` allows watch or snapshot driven writes even when the original actor cannot be preserved

That gives a clean explanation for why the system still has value even if this aggregated API path is not perfect today.
