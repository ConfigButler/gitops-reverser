# Designing Kubernetes Relationships: The Definitive Guide

Modeling Links, Lifecycle, and Security in Custom Resources

When designing a Kubernetes Operator, one of the first architectural challenges you face is "The Linking Problem." How should your Custom Resource (CR) reference other resources? Should it be a string? A struct? Can it cross namespaces?

This guide outlines the industry-standard patterns for modeling relationships, ranging from static configuration to runtime lifecycle management.

## Part 1: The Static Link (Configuration)

The most common relationship is configuration: Resource A needs to know about Resource B.

### 1. The Structure: Strings vs. Structs

**Anti-Pattern:** Using flat strings (e.g., `ingressName: "my-app"`).
Flat strings are brittle. They lack context (Group/Kind) and make validation difficult.

**Best Practice:** Use a "Typed Reference" Struct.
Even if you only support one Kind today, using a struct ensures your API is self-documenting and extensible.

#### A. The "Local" Reference (Same Namespace)

Use this when the target must reside in the same namespace (e.g., a Pod referencing a Secret).

```go
type LocalTargetReference struct {
    // API Group of the referent.
    // +kubebuilder:default="networking.k8s.io"
    Group string `json:"group,omitempty"`

    // Kind of the referent.
    // +kubebuilder:validation:Enum=Ingress;Gateway
    Kind string `json:"kind"`

    // Name of the referent.
    // +kubebuilder:validation:MinLength=1
    Name string `json:"name"`
}
```

#### B. The "Cross-Namespace" Reference

Use this only if strictly necessary. It includes a namespace field.

```go
type GlobalTargetReference struct {
    // ... group, kind, name ...

    // Namespace of the referent.
    // +optional
    Namespace string `json:"namespace,omitempty"`
}
```

**Pro Tip:** Avoid importing `corev1.ObjectReference`. It contains legacy fields (uid, resourceVersion) that confuse users. Define your own clean struct.

### 2. Deep Dive: Analysis of Standard Types

Understanding the history and limitations of existing Kubernetes types helps explain why we define custom structs.

#### A. `corev1.LocalObjectReference`

This is the standard, simple reference used by Pods (for Secrets/ConfigMaps).

*   **Status:** Safe to use, but limited (Name only).
*   **Use Case:** Strictly for "I need a name of something in the same namespace, and I already know exactly what Kind it is (e.g., it's definitely a Secret)."
*   **Source:** `k8s.io/api/core/v1/types.go`

```go
// LocalObjectReference contains enough information to let you locate the
// referenced object inside the same namespace.
type LocalObjectReference struct {
    // Name of the referent.
    // More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#names
    Name string `json:"name" protobuf:"bytes,1,opt,name=name"`
}
```

#### B. `corev1.ObjectReference`

This is the type users often accidentally reach for because it sounds right.

*   **Status:** **AVOID** in CRD Specs.
*   **Why:** It contains fields like `UID` and `ResourceVersion`. These are for identity (events), not configuration. If a user sets `resourceVersion` in your CRD, your controller will ignore it, creating a misleading API.
*   **Official Warning:** While the struct itself doesn't have a big "DEPRECATED" banner, the API conventions guide explicitly states that configuration references should only contain necessary fields (Group, Kind, Name).
*   **Source:** `k8s.io/api/core/v1/types.go`

```go
// ObjectReference contains enough information to let you inspect or modify the referred object.
type ObjectReference struct {
    Kind string `json:"kind,omitempty" ...`
    Namespace string `json:"namespace,omitempty" ...`
    Name string `json:"name,omitempty" ...`
    
    // WARNING: These fields make it bad for CRD Specs
    UID types.UID `json:"uid,omitempty" ...`
    APIVersion string `json:"apiVersion,omitempty" ...`
    ResourceVersion string `json:"resourceVersion,omitempty" ...`
    FieldPath string `json:"fieldPath,omitempty" ...`
}
```

#### C. The Modern Standard: Gateway API References

If you want to see where the industry is moving, look at the Gateway API (the newest official K8s API). They abandoned the core types and defined their own strict standards.

*   **Source:** `gateway-api/apis/v1/shared_types.go`

They explicitly created separate types for Local vs. Namespaced to solve the exact problems we discussed:

*   **`LocalObjectReference` (Gateway Style):** Explicitly supports Group and Kind to allow polymorphism.
*   **`SecretObjectReference`:** Restricts the Kind to "Secret" specifically.

**Design Note:** The Gateway API documentation notes: *"LocalObjectReference identifies an API object within the namespace of the referrer. The API object must be valid in the cluster; the Group and Kind must be registered."*

### 3. Naming Conventions

Kubernetes has strong conventions for field names based on intent:

*   `targetRef`: Used when the referenced object is the primary subject of your controller (e.g., a Policy applied to a Route).
*   `{Kind}Ref`: Used for helper/dependency objects (e.g., `secretRef`, `serviceAccountRef`).
*   `sourceRef` / `sinkRef`: Used for directional data flow (e.g., Backup tools, Event forwarders).
*   `selector`: Used when referencing a group of objects via labels (not by name).

## Part 2: The Contract (Validation & Immutability)

A link is a contract. You must enforce the terms of that contract using the API server, not just your controller code.

### 1. Enforcing Types (The Enum Pattern)

Don't let users guess which resources are supported. Use Kubebuilder markers to restrict the Kind.

```go
// +kubebuilder:validation:Enum=Deployment;StatefulSet
Kind string `json:"kind"`
```

### 2. Enforcing Stability (Immutability)

Changing a reference after creation (e.g., repointing a live database connection) is often dangerous and complex to reconcile. It is often safer to make the reference Immutable.

Use CEL (Common Expression Language) to enforce this at the API level:

```go
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="targetRef is immutable"
TargetRef LocalTargetReference `json:"targetRef"`
```

## Part 3: The Runtime Link (Lifecycle & Ownership)

While spec fields define the configuration, the `metadata.ownerReferences` field defines the lifecycle.

### 1. The Parent-Child Relationship (OwnerReferences)

If your CRD creates another resource (e.g., CronTab creates a Job), you must set an OwnerReference on the child.

*   **Effect:** When the parent (CronTab) is deleted, the Kubernetes Garbage Collector automatically deletes the child (Job).
*   **Constraint:** OwnerReferences cannot cross namespaces. Parents and children must live together.
*   **Blocking:** You can set `blockOwnerDeletion: true` to ensure the parent isn't fully removed until the child is gone (useful for cleanup hooks).

### 2. Watching Dependencies

Your controller must "watch" the referenced objects.

*   **Scenario:** Your CRD references a Secret.
*   **Requirement:** If the Secret changes, your Controller needs to wake up and re-reconcile your CRD.
*   **Implementation:** Use an Informer or Map Function in your controller builder to map Secret events back to your CRD.

## Part 4: The Danger Zone (Cross-Namespace Linking)

Allowing users to reference resources in other namespaces is the #1 security pitfall in Operator design.

### The Risk: "Confused Deputy"

Imagine a user in namespace `guest` creates a CRD that references a Secret in namespace `admin`.
If your Operator reads the admin Secret and uses it for the guest user, you have just breached tenancy isolation.

### Security Best Practices

#### 1. Disable by Default

If your operator supports cross-namespace linking, it should be disabled by default. Require a strict flag on the Operator binary (e.g., `--allow-cross-namespace-refs=true`) to enable it.

#### 2. The "Handshake" Pattern (ReferenceGrant)

If you must allow cross-namespace access, do not implicitly trust the reference. Use a "Handshake" mechanism, similar to the Gateway API's ReferenceGrant.

**The Flow:**
*   **Consumer (Namespace A):** Creates your CRD pointing to Secret in Namespace B.
*   **Producer (Namespace B):** Must create a ReferenceGrant explicitly saying: "I allow Namespace A to reference my Secrets."
*   **Controller:** Checks if both exist. If the Grant is missing, the reference is ignored (status: PermissionDenied).

#### 3. Strict RBAC

Ensure your Operator's ServiceAccount has the minimal necessary permissions.

*   **Bad:** `verbs: ["*"], resources: ["secrets"]` (Cluster-wide)
*   **Better:** Use a specific RoleBinding or dynamic client that checks if the user who created the CRD actually has access to the target resource (Impersonation).

## Summary Checklist for API Designers

| Feature | Recommendation |
| :--- | :--- |
| **Field Type** | Never use strings. Use a struct with Kind, Name, Group. |
| **Validation** | Use `+kubebuilder:validation:Enum` to restrict allowed Kinds. |
| **Immutability** | Use CEL rules (`self == oldSelf`) if hot-swapping targets is risky. |
| **Lifecycle** | Set OwnerReferences on generated child resources for automatic GC. |
| **Namespace** | Default to Local (same namespace). Avoid namespace fields in your spec unless absolutely necessary. |
| **Security** | If crossing namespaces, implement a ReferenceGrant handshake. |
