Title: core.cozystack.io REST storage does not emit k8s.io/initial-events-end bookmark for SendInitialEvents=true watches

Summary

The aggregated apiserver's resources in group core.cozystack.io (tenantsecrets, tenantmodules, tenantnamespaces) do not honor the SendInitialEvents field of ListOptions. Watch clients that opt into the WatchList / streaming-list protocol (kube-apiserver ≥ 1.27, client-go WatchListClient feature gate, on by default since client-go v1.35) never receive the required k8s.io/initial-events-end bookmark, so reflectors stay stuck in their initial-stream phase and log hasn't received required bookmark event marking the end of initial events stream every ~10s. The apps.cozystack.io/Application storage already implements this contract correctly — the three core resources do not.

Reproduction

Any client running client-go ≥ v0.30 with the default WatchListClient=true informing over tenantsecrets, tenantmodules, or tenantnamespaces:


Warning: event bookmark expired err="reflector.go:343: hasn't received required bookmark event marking the end of initial events stream, received last event 59.998946102s ago"
The warning fires on every reflector restart and continuously while the cache is unsynced from the WatchList perspective.

Evidence (commit 1810263)

Reference implementation that does it correctly:
pkg/registry/apps/application/rest.go:684-807 — reads options.SendInitialEvents, tracks lastResourceVersion, emits a watch.Bookmark with metadata.annotations["k8s.io/initial-events-end"] = "true" once initial ADDED events have been delivered, and even handles the "underlying watcher closes during initial snapshot" edge case.

Missing in:

pkg/registry/core/tenantsecret/rest.go:454 — Watch() ignores opts.SendInitialEvents and only forwards bookmark events from the backing corev1.Secret watcher; those carry no k8s.io/initial-events-end annotation.
pkg/registry/core/tenantmodule/rest.go:311 — same pattern, forwards bookmarks from the backing helmv2.HelmRelease watcher.
pkg/registry/core/tenantnamespace/rest.go:158 — same pattern, forwards bookmarks from the backing corev1.Namespace watcher.
A grep for SendInitialEvents or initial-events-end across pkg/registry/core/ returns zero hits.

Proposed fix

Lift the sendInitialEventsEndBookmark helper and its caller sites from application/rest.go into the three core resources, adapting the bookmark object to the resource's own TypeMeta. The mechanical shape per file:

Near the top of Watch(): sendInitialEvents := opts.SendInitialEvents != nil && *opts.SendInitialEvents.
Track lastResourceVersion on every translated event from the backing watcher.
After delivering the initial Added events, emit a watch.Bookmark whose object is an empty TenantSecret / TenantModule / TenantNamespace carrying the right TypeMeta, ResourceVersion=lastResourceVersion, and annotations["k8s.io/initial-events-end"]="true".
Handle "underlying watcher closes before initial snapshot is complete" — emit the bookmark on the way out, same as application/rest.go:816-817.
Acceptance criteria

A test alongside rest_watch_test.go for each of the three resources asserting:
When SendInitialEvents=true, all existing objects are emitted as ADDED.
A Bookmark event follows with annotations["k8s.io/initial-events-end"] == "true" and a non-empty ResourceVersion.
Subsequent live events are delivered after the bookmark.
An informer using client-go's WatchListClient feature reaches HasSynced=true against these resources without the missing-bookmark warning.
