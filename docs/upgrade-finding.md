# gitops-reverser WatchList Bookmark Report

Date: 2026-05-25

## Summary

`apiservice-audit-proxy` 0.5.0 appears to have addressed the original
transport-level watch-stream problem, but the remaining production warning in
`gitops-reverser` points to a separate client-side watch-list behavior.

`gitops-reverser` is built with `k8s.io/client-go v0.36.1`. In that version,
the `WatchListClient` feature defaults to enabled. Dynamic informers created by
`gitops-reverser` therefore use Kubernetes streaming-list semantics:

```text
watch=true
sendInitialEvents=true
allowWatchBookmarks=true
resourceVersionMatch=NotOlderThan
```

client-go then waits for a special bookmark event annotated as the end of the
initial events stream. The production warning means that bookmark is not being
received.

## Production Symptom

Representative log line:

```text
Warning: event bookmark expired err="pkg/mod/k8s.io/client-go@v0.36.1/tools/cache/reflector.go:343: hasn't received required bookmark event marking the end of initial events stream, received last event 59.998946102s ago"
```

This is not a generic watch timeout. It specifically means client-go is waiting
for the initial-events-end bookmark required by streaming-list mode.

## Evidence

`gitops-reverser` creates dynamic shared informer factories for watched
resources:

- `external-resources/gitops-reverser/internal/watch/manager.go`
- `external-resources/gitops-reverser/internal/watch/manager_catalog.go`

The relevant call sites are:

```go
dynamicinformer.NewDynamicSharedInformerFactory(client, 0)
dynamicinformer.NewFilteredDynamicSharedInformerFactory(client, 0, ns, nil)
```

and:

```go
dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, 0)
```

In `client-go v0.36.1`, dynamic informers wrap their list/watcher with
watch-list support:

```go
cache.ToListWatcherWithWatchListSemantics(&cache.ListWatch{...})
```

The reflector enables watch-list mode from the client-go feature gate:

```go
r.useWatchList = clientfeatures.FeatureGates().Enabled(clientfeatures.WatchListClient)
```

`WatchListClient` defaults to true as of the v1.35 client-go feature defaults,
which are present in `v0.36.1`.

When enabled, the reflector starts the initial stream with:

```go
metav1.ListOptions{
    ResourceVersion:      lastKnownRV,
    AllowWatchBookmarks:  true,
    SendInitialEvents:    ptr.To(true),
    ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
    TimeoutSeconds:       &timeoutSeconds,
}
```

It only considers the initial stream complete when it receives a bookmark with:

```go
meta.GetAnnotations()[metav1.InitialEventsAnnotationKey] == "true"
```

The repeated production warning is emitted by client-go while waiting for that
bookmark.

## Interpretation

The proxy can pass watch events through, but it does not and should not
synthesize Kubernetes streaming-list bookmarks. The initial-events-end bookmark
must be produced by the Kubernetes API server or aggregated backend serving the
watched resource.

So the remaining failure is most likely:

1. `gitops-reverser` accidentally opted into streaming-list mode through the
   client-go upgrade.
2. One or more watched API paths, especially aggregated APIs, do not produce
   the required initial-events-end bookmark.
3. client-go keeps the reflector in its initial watch-list phase and logs the
   missing-bookmark warning every 10 seconds.

The design documents in `gitops-reverser` discuss future use of
`sendInitialEvents=true`, but the current implementation uses ordinary dynamic
informers. That makes this behavior an implicit dependency-default change,
rather than a fully implemented and tested watch-list design.

## Immediate Workaround

Disable the client-go `WatchListClient` feature in the `gitops-reverser` pod:

```yaml
env:
  - name: KUBE_FEATURE_WatchListClient
    value: "false"
```

The Helm chart already exposes `.Values.env`, so this can be applied as a values
override.

Example values fragment:

```yaml
env:
  - name: KUBE_FEATURE_WatchListClient
    value: "false"
```

After rollout, the dynamic informers should return to the older LIST-then-WATCH
behavior and stop requiring the initial-events-end bookmark.

## Recommended Follow-Up

For `gitops-reverser`, make the choice explicit instead of relying on the
client-go default:

- Disable `WatchListClient` by default in the manager process, or document and
  set the environment override in the chart defaults.
- Add a regression test or e2e lane that watches resources through the
  aggregated API path and confirms informer sync completes.
- Revisit streaming-list support later as an intentional feature, with explicit
  tests for `sendInitialEvents=true`, `allowWatchBookmarks=true`, and the
  `k8s.io/initial-events-end` bookmark.

For `apiservice-audit-proxy`, the existing watch-stream work remains useful:
the proxy still must keep watch responses open, flush promptly, avoid audit
body spooling for watches, and avoid imposing write deadlines. This report is
about a different layer: client-go reflector semantics in the downstream
consumer.
