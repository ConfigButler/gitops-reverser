# Kubernetes API Discovery and Watching in GitOps Reverser

This document explains how GitOps Reverser decides what it can watch, and gives Kubernetes API background for
learning.

## TL;DR

- A watched resource does **not** need to be a CRD.
- GitOps Reverser can watch any discoverable Kubernetes resource that supports `list` and `watch`, including:
  - Built-in resources (for example `configmaps.v1`)
  - CRD-backed resources
  - Aggregated API resources served through `APIService`
- Practical rule: if the resource appears in API discovery and has `list/watch` verbs, it is a candidate.

## Kubernetes API Theory (Quick Primer)

Kubernetes APIs are addressed as **Group / Version / Resource (GVR)**:

- Group: `""` (core), `apps`, `rbac.authorization.k8s.io`, `metrics.k8s.io`, etc.
- Version: `v1`, `v1beta1`, etc.
- Resource: plural name such as `pods`, `deployments`, `customresources`.

Examples:

- Core API: `"" / v1 / configmaps`
- Apps API: `apps / v1 / deployments`
- Custom API: `shop.example.com / v1 / icecreamorders`

### Where resources come from

Kubernetes can serve resources from multiple backends:

- Built-in APIs (compiled into kube-apiserver)
- CRDs (registered through `CustomResourceDefinition`)
- Aggregated APIs (registered through `APIService`, then served by an extension API server)

From a client perspective, all of these are exposed through API discovery endpoints (`/api` and `/apis`).

## How GitOps Reverser Chooses Watch Targets

At runtime, the watch manager does this:

1. Build requested GVRs from active `WatchRule` and `ClusterWatchRule`.
2. Query API discovery (`ServerPreferredResources`).
3. Keep only resources that:
   - exist in discovery,
   - support both `list` and `watch`,
   - match the requested scope (`Namespaced` vs `Cluster`).
4. Start dynamic informers for the remaining GVRs.

This means the watch decision is based on **discoverability and verbs**, not on "is it a CRD?".

## CRD vs APIService: What This Means in Practice

### CRDs

CRDs are supported when:

- their GVR is in discovery,
- the resource supports `list` and `watch`,
- the rule points to a concrete GVR that matches scope.

### APIService-backed resources

APIService-backed resources are also supported under the same conditions:

- the `APIService` is healthy and available,
- the resource appears in discovery,
- the API server exposes `list` and `watch` for that resource.

So yes, you can watch resources served through `APIService`; CRD is not required.

## Current Product Constraints (Important)

- Informer planning currently needs concrete GVRs:
  - one concrete API group (not `*`)
  - one concrete API version (not `*`)
  - concrete resource name (not `*`, no subresource forms)
- Wildcard expansion across discovery is not implemented yet.
- A small built-in exclusion list skips noisy resources (for example `pods`, `events`, `leases`, `jobs`).
- RBAC still applies: the operator must be allowed to `get/list/watch` the target resources.
- Scope still applies:
  - `WatchRule` is namespace-scoped
  - `ClusterWatchRule` can watch cluster-scoped and namespaced resources (by rule scope)

## Quick Verification Commands

Use these to check if a resource is likely watchable:

```bash
# 1) Is the resource discoverable?
kubectl api-resources | grep -E 'icecreamorders|<your-resource>'

# 2) Check served API group/version directly (example)
kubectl get --raw /apis/metrics.k8s.io/v1beta1

# 3) Validate operator permissions
kubectl auth can-i list <resource> --all-namespaces
kubectl auth can-i watch <resource> --all-namespaces
```

If a resource is missing from discovery or does not support `watch`, GitOps Reverser will not start an informer for it.
