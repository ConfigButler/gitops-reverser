# Resource Matching Analysis

## Problem Statement

The GitOps Reverser currently only tracks ConfigMaps and Secrets correctly. Custom resources and other Kubernetes resources are not being picked up by WatchRules.

## Root Cause

The issue is in [`internal/rulestore/store.go:130-144`](../internal/rulestore/store.go#L130-L144). The `isPluralMatch` function uses a **hardcoded mapping** of only 5 resource types:

```go
pluralMappings := map[string]string{
    "configmaps":  "ConfigMap",
    "pods":        "Pod",
    "services":    "Service",
    "deployments": "Deployment",
    "secrets":     "Secret",
}
```

## Understanding Kubernetes Resource Identification: GVK vs GVR

This section provides a deep dive into how Kubernetes identifies resources and why understanding this is critical for building tools like the GitOps Reverser.

### The Two Identity Systems

Kubernetes uses **two parallel identification systems** for resources, each serving different purposes:

1. **GroupVersionKind (GVK)** - Used in manifests and type systems
2. **GroupVersionResource (GVR)** - Used in API paths and webhooks

Understanding the distinction between these two is the key to solving the resource matching problem.

---

## GroupVersionKind (GVK): The Type System

### Structure

```go
schema.GroupVersionKind{
    Group:   "apps",
    Version: "v1", 
    Kind:    "Deployment"  // Singular, PascalCase
}
```

### Where GVK is Used

1. **YAML Manifests** - What users write:
   ```yaml
   apiVersion: apps/v1      # Group + Version
   kind: Deployment         # Kind (Singular, PascalCase)
   metadata:
     name: my-deployment
   ```

2. **Go Type System** - Code generation and type definitions:
   ```go
   type Deployment struct {
       metav1.TypeMeta
       // ...
   }
   ```

3. **Object Metadata** - Runtime type information:
   ```go
   obj.GetObjectKind().GroupVersionKind()
   // Returns: {Group: "apps", Version: "v1", Kind: "Deployment"}
   ```

### GVK Components Explained

#### 1. Group
The API group that owns this resource type. Groups prevent naming conflicts and enable extensibility.

**Examples:**
- `""` (empty string) = **Core group** - original Kubernetes resources (Pod, Service, ConfigMap, Secret, Namespace)
- `"apps"` = Application resources (Deployment, StatefulSet, DaemonSet, ReplicaSet)
- `"batch"` = Batch processing (Job, CronJob)
- `"networking.k8s.io"` = Networking resources (Ingress, NetworkPolicy, IngressClass)
- `"example.com"` = Your custom resources

**Why Groups Matter:**
- Different teams can create resources without conflicts
- Each group has independent versioning
- Enables Kubernetes extensibility - anyone can add a group
- Groups are like namespaces for API types

#### 2. Version
The maturity level of the API within that group.

**Standard Versions:**
- `"v1"` = Stable, production-ready API
- `"v1beta1"`, `"v1beta2"` = Beta, may have breaking changes
- `"v1alpha1"`, `"v1alpha2"` = Alpha, experimental

**Why Versions Matter:**
- Allows API evolution without breaking existing users
- Multiple versions can coexist simultaneously
- Clear graduation path: alpha → beta → stable (GA)
- Each version can have different fields and behavior

#### 3. Kind
The **singular, PascalCase** name of the resource type.

**Examples:**
- `"Pod"` not "pod" or "pods"
- `"Deployment"` not "deployment" or "deployments"
- `"MyApp"` not "myapp" or "myapps"
- `"ConfigMap"` not "configmap" or "configmaps"

**Why Singular and PascalCase:**
- Represents a single instance (object-oriented thinking)
- Follows Go naming conventions
- Used in code generation and type definitions

---

## GroupVersionResource (GVR): The API System

### Structure

```go
metav1.GroupVersionResource{
    Group:    "apps",
    Version:  "v1",
    Resource: "deployments"  // Plural, lowercase
}
```

### Where GVR is Used

1. **REST API URLs** - How resources are accessed:
   ```
   GET /apis/apps/v1/namespaces/default/deployments
                                        ^^^^^^^^^^^ Resource (Plural)
   ```

2. **Admission Webhooks** - What webhooks receive:
   ```go
   req.Resource.Resource = "deployments"  // Plural, lowercase
   req.Resource.Group = "apps"
   req.Resource.Version = "v1"
   ```

3. **kubectl Commands** - What users type:
   ```bash
   kubectl get deployments
               ^^^^^^^^^^^ Resource (Plural)
   ```

4. **Dynamic Client** - Programmatic API access:
   ```go
   dynamicClient.Resource(gvr).Namespace("default").List(...)
   ```

### GVR Components Explained

#### 1. Group
Same as GVK - the API group.

#### 2. Version  
Same as GVK - the API version.

#### 3. Resource
The **plural, lowercase** name used in API paths.

**Examples:**
- `"pods"` not "Pod" or "pod"
- `"deployments"` not "Deployment" or "deployment"
- `"configmaps"` not "ConfigMap" or "configmap"
- `"myapps"` not "MyApp" or "myapp"

**For Custom Resources with Groups:**
- `"myapps.example.com"` - Full group-qualified name
- Format: `{plural}.{group}`

**Why Plural and Lowercase:**
- REST convention: collections are plural (`/users`, `/posts`)
- API paths are traditionally lowercase
- Represents multiple instances at an endpoint

---

## The Critical Flow: From YAML to Admission Webhook

This diagram shows how resources flow through Kubernetes and why both GVK and GVR exist:

```
┌─────────────────────────────────────────────────────────┐
│                   1. User's YAML                         │
│                                                          │
│  apiVersion: apps/v1          ← Group + Version         │
│  kind: Deployment             ← Kind (Singular)          │
│  metadata:                                               │
│    name: my-app                                          │
│                                                          │
│  Uses: GVK (GroupVersionKind)                           │
└────────────────────┬────────────────────────────────────┘
                     │
                     │ kubectl apply -f
                     ↓
┌─────────────────────────────────────────────────────────┐
│              2. API Server Request                       │
│                                                          │
│  POST /apis/apps/v1/namespaces/default/deployments      │
│                                        ^^^^^^^^^^        │
│                                    Resource (Plural)     │
│                                                          │
│  Uses: GVR (GroupVersionResource)                       │
└────────────────────┬────────────────────────────────────┘
                     │
                     │ Admission webhook configured
                     ↓
┌─────────────────────────────────────────────────────────┐
│            3. Admission Webhook Receives                 │
│                                                          │
│  admission.Request {                                     │
│    Kind: {                      ← GVK for type checking │
│      Group: "apps"                                       │
│      Version: "v1"                                       │
│      Kind: "Deployment"                                  │
│    }                                                     │
│    Resource: {                  ← GVR for API routing   │
│      Group: "apps"                                       │
│      Version: "v1"                                       │
│      Resource: "deployments"                             │
│    }                                                     │
│  }                                                       │
│                                                          │
│  Contains: BOTH GVK and GVR!                            │
└─────────────────────────────────────────────────────────┘
```

**Key Insight:** The admission request contains **both** GVK and GVR because:
- **GVK** tells you what type of object this is (for validation logic)
- **GVR** tells you what API endpoint was called (for routing logic)

---

## Real-World Example: Custom Resource Definition (CRD)

Let's trace a complete example with a custom resource to see GVK and GVR in action.

### Step 1: Define the CRD

```yaml
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: myapps.example.com  # ← Format: {plural}.{group}
spec:
  group: example.com          # ← API Group
  versions:
    - name: v1                # ← Version
      served: true
      storage: true
      schema: { ... }
  scope: Namespaced
  names:
    kind: MyApp               # ← Singular, PascalCase (for GVK)
    plural: myapps            # ← Plural, lowercase (for GVR)
    singular: myapp           # ← Singular, lowercase (for kubectl)
    listKind: MyAppList       # ← List type
```

**Important:** The CRD explicitly defines both singular (Kind) and plural (Resource) forms!

### Step 2: Create an Instance (User's YAML)

```yaml
apiVersion: example.com/v1   # ← Group + Version
kind: MyApp                  # ← Kind (GVK) - Singular, PascalCase
metadata:
  name: my-instance
  namespace: default
spec:
  replicas: 3
  image: myapp:v1.0
```

**This uses GVK** - the user writes what type of thing they want (a single `MyApp`).

### Step 3: kubectl Applies (API Call)

```bash
kubectl apply -f myapp.yaml
```

Translates to:

```
POST /apis/example.com/v1/namespaces/default/myapps
                                                ^^^^^^ Resource (GVR) - Plural
```

**This uses GVR** - the API endpoint uses the plural form.

### Step 4: Admission Webhook Receives

```go
// In your webhook's Handle function:
func (h *EventHandler) Handle(ctx context.Context, req admission.Request) {
    // Both forms are available:
    
    // GVK - For type checking
    fmt.Println(req.Kind.Group)    // "example.com"
    fmt.Println(req.Kind.Version)  // "v1"
    fmt.Println(req.Kind.Kind)     // "MyApp" (Singular)
    
    // GVR - For API routing and matching
    fmt.Println(req.Resource.Group)    // "example.com"
    fmt.Println(req.Resource.Version)  // "v1"
    fmt.Println(req.Resource.Resource) // "myapps.example.com" (Plural + Group)
}
```

**Both are provided** because they serve different purposes in the admission logic.

---

## The Matching Problem We Solved

### Original (Broken) Approach

```go
// WatchRule configuration (what users write):
rules:
  - resources: ["myapps.example.com"]  // GVR format - plural with group

// Old matching code:
kind := obj.GetObjectKind().GroupVersionKind().Kind  // Gets "MyApp" (GVK)
if rule.matches(kind) {  // Tries to match "myapps.example.com" with "MyApp"
    // This fails! Different formats!
}

// Attempted fix with hardcoded mapping:
pluralMappings := map[string]string{
    "myapps": "MyApp",  // Would need entry for EVERY resource type!
}
```

**Problems:**
1. Comparing apples (GVR) to oranges (GVK)
2. Hardcoded mappings don't scale
3. Custom resources not in mapping = never matched
4. Group-qualified names (`myapps.example.com`) can't be mapped to just `MyApp`

### New (Fixed) Approach

```go
// WatchRule configuration (unchanged):
rules:
  - resources: ["myapps.example.com"]  // GVR format

// New matching code:
resourcePlural := req.Resource.Resource  // Gets "myapps.example.com" (GVR)
if rule.matches(obj, resourcePlural) {   // Matches GVR to GVR!
    // This works! Same format on both sides!
}
```

**Advantages:**
1. Comparing like with like (GVR to GVR)
2. No hardcoded mappings needed
3. Works for ALL resources automatically
4. Handles group-qualified names correctly
5. Uses data already in the admission request

---

## Why Kubernetes Has Both GVK and GVR

### Different Concerns, Different Formats

| Aspect | GVK (GroupVersionKind) | GVR (GroupVersionResource) |
|--------|----------------------|--------------------------|
| **Purpose** | Type system, object identity | API routing, discovery |
| **Format** | Singular, PascalCase | Plural, lowercase |
| **Used In** | Manifests, Go types, codegen | URLs, kubectl, webhooks |
| **Example** | `Deployment` | `deployments` |
| **Think Of As** | "What is this thing?" | "Where does this thing live?" |
| **Audience** | Developers, type systems | API clients, REST consumers |

### Separation Enables Flexibility

1. **Different Pluralization Rules**
   - Some resources have irregular plurals: `Ingress` → `ingresses` (not "ingresss")
   - CRDs can specify custom plural forms
   - Languages have different pluralization rules

2. **Multiple Resources, Same Kind**
   - `Deployment` kind exists in both `apps/v1` and `extensions/v1beta1`
   - GVR disambiguates: `deployments.apps` vs `deployments.extensions`

3. **API Evolution**
   - Kind name stays stable (breaking change to rename)
   - Resource name can be aliased or changed more easily

4. **REST Conventions**
   - REST APIs use plural nouns for collections: `/users`, `/posts`
   - Object types use singular nouns: `User`, `Post`
   - Kubernetes maintains both conventions appropriately

---

## How Kubernetes Converts Between GVK and GVR

### The Discovery API

Kubernetes provides the **Discovery API** to map between GVK and GVR:

```go
// From GVK to GVR:
mapper, _ := apiutil.NewDiscoveryRESTMapper(config)
gvr, _ := mapper.RESTMapping(gvk)

// From GVR to GVK:
kinds, _ := mapper.KindsFor(gvr)
```

### CRD Definitions

When you create a CRD, you explicitly provide both:

```yaml
spec:
  names:
    kind: MyApp        # ← Used in manifests (GVK)
    plural: myapps     # ← Used in API paths (GVR)
```

### Why We Don't Use Discovery API

While the Discovery API can convert between GVK and GVR, we don't use it because:

1. **Already Have GVR** - Admission request provides `req.Resource.Resource`
2. **No Conversion Needed** - WatchRules already use GVR format
3. **Performance** - Discovery API calls add latency
4. **Permissions** - Would require extra RBAC permissions
5. **Simplicity** - Direct matching is simpler and faster

---

## The Deep Pattern: Universal Resource Identification

### The Kubernetes API Layer

At the API layer, **GVR is the universal identifier**:

```
User Space           Kubernetes API              Your Code
──────────────────   ─────────────────────────   ──────────────────
kind: Pod         →  /api/v1/pods             →  Match "pods"
kind: Deployment  →  /apis/apps/v1/           →  Match "deployments"
                     deployments
kind: MyApp       →  /apis/example.com/v1/    →  Match "myapps.
                     namespaces/x/myapps            example.com"
```

**Key Insight:** By matching against GVR (plural resource names), your code operates at the same abstraction level as:
- The Kubernetes API server
- kubectl commands
- API discovery
- Admission webhook routing

This is why it works universally for all resources!

### Group-Qualified Resource Names

For resources in non-core groups, the full GVR format is:

```
{resource}.{group}
```

**Examples:**
- Core resources: `pods`, `services`, `configmaps` (no group qualifier needed)
- Apps group: `deployments.apps`, `statefulsets.apps`
- Custom resources: `myapps.example.com`, `databases.db.example.com`

This fully-qualified format prevents any ambiguity.

---

## Implementation Details

### How the Fix Works

1. **Webhook Receives Admission Request**
   ```go
   func (h *EventHandler) Handle(ctx context.Context, req admission.Request) {
       // Extract the plural resource name from the admission request
       resourcePlural := req.Resource.Resource  // e.g., "myapps.example.com"
   ```

2. **Pass GVR to Matching Logic**
   ```go
       matchingRules := h.RuleStore.GetMatchingRules(obj, resourcePlural)
   ```

3. **Match GVR to GVR**
   ```go
   func (s *RuleStore) GetMatchingRules(obj client.Object, resourcePlural string) []CompiledRule {
       for _, rule := range s.rules {
           if rule.resourceMatches(resourcePlural) {  // GVR to GVR comparison!
               matchingRules = append(matchingRules, rule)
           }
       }
       return matchingRules
   }
   ```

4. **Simple String Matching**
   ```go
   func (r *CompiledRule) resourceMatches(resourcePlural string) bool {
       for _, ruleResource := range r.Resources {
           if strings.EqualFold(ruleResource, resourcePlural) {  // Case-insensitive
               return true
           }
       }
       return false
   }
   ```

### Wildcard Support

We also support wildcard patterns for flexible matching:

**Suffix Wildcards (`prefix*`):**
```yaml
resources: ["ingress*"]  # Matches: ingresses, ingressclasses, ingressroutes
```

**Prefix Wildcards (`*suffix`):**
```yaml
resources: ["*.example.com"]  # Matches: myapps.example.com, databases.example.com
```

---

## The Solution Summary

### What We Changed

1. **Added `resourcePlural` parameter** to matching functions
2. **Removed hardcoded GVK mappings** - no longer needed
3. **Match GVR to GVR directly** - simpler and universal
4. **Enhanced wildcard support** - both prefix and suffix wildcards

### Why It Works

- Uses the **same identification system** as Kubernetes API layer
- No conversion needed - data already available
- Works for **all resources automatically**:
  - ✅ Core resources (pods, services, configmaps)
  - ✅ Extended resources (deployments, statefulsets, ingresses)
  - ✅ Custom resources (myapps.example.com, any CRD)
  - ✅ Resources from any API group

### The Elegance

This solution embraces Kubernetes' design philosophy:
- **Separation of concerns**: GVK for types, GVR for API
- **No assumptions**: Works with any resource
- **Uses existing data**: No Discovery API calls needed
- **Simple and maintainable**: Direct string matching

By operating at the **GVR layer** (plural resource names), the GitOps Reverser now speaks the same language as the Kubernetes API itself!

---

## Recommended Implementation

Modify the matching logic to compare plural resource names directly:

1. **Event Handler**: Captures `req.Resource.Resource` from admission request ✅
2. **RuleStore Matching**: Uses plural resource for matching ✅
3. **Removed**: Hardcoded `isPluralMatch` mappings ✅

This approach:
- Uses existing data flow
- Works for all Kubernetes resources
- No hardcoded mappings needed
- Simpler and more maintainable

---

## Usage Examples

### Core Resources

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: WatchRule
metadata:
  name: core-resources-rule
spec:
  gitRepoConfigRef: my-git-config
  rules:
    - resources:
        - "pods"
        - "services"
        - "configmaps"
        - "secrets"
        - "deployments"
```

### Custom Resources

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: WatchRule
metadata:
  name: custom-resources-rule
spec:
  gitRepoConfigRef: my-git-config
  rules:
    - resources:
        - "myapps.example.com"           # Single CRD
        - "databases.db.example.com"     # Another CRD
        - "*.example.com"                # All CRDs in example.com group
```

### Mixed Resources

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: WatchRule
metadata:
  name: mixed-rule
spec:
  gitRepoConfigRef: my-git-config
  rules:
    - resources:
        - "configmaps"              # Core resource
        - "secrets"                 # Core resource
        - "myapps.example.com"      # Custom resource
        - "ingress*"                # Wildcard for all ingress-related
```

---

## Conclusion

The fix transforms the GitOps Reverser from a tool that only worked with 5 hardcoded resource types to a **universal Kubernetes resource tracker** that works with any resource type automatically.

The key insight: **match GVR to GVR** (plural resource names) instead of trying to convert between GVK (singular kind names) and GVR. This aligns with how Kubernetes itself identifies and routes resources at the API layer.

By understanding the distinction between GVK (type system) and GVR (API system), and using the appropriate identifier for each purpose, the code now works seamlessly with:
- All core Kubernetes resources
- All extended resources (apps, networking, batch, etc.)
- All custom resources from any group
- Any future resources added to Kubernetes

**This is the power of working with the Kubernetes API at the right abstraction level!**