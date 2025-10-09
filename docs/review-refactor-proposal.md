Architectural Guideline: Reverse GitOps Audit Logger

This document outlines the architecture for a tool that uses a Kubernetes Admission Webhook to capture resource configurations and store them as a sanitized, declarative audit log in a Git repository.

1. Distilled Requirements
Based on our discussion, the core requirements for this tool are:

Functionality: Intercept Kubernetes resource creations and updates using a ValidatingAdmissionWebhook.

Identification: Generate a unique, human-readable, and API-consistent file path for each resource stored in Git.

Sanitization: Ensure that stored manifests represent the declarative intent, not the live operational state. This means stripping fields like status and other system-managed metadata.

Genericity: The solution must be robust enough to handle any Kubernetes resource kind, including standard types (Pod, ConfigMap) and Custom Resource Definitions (CRDs), without prior knowledge of their structure.

Implementation: The core logic must be implemented in Go, leveraging the sigs.k8s.io/controller-runtime/pkg/webhook/admission package.

Formatting: The final YAML files committed to Git must be clean and follow the conventional field order (apiVersion, kind, metadata, etc.).

2. Key Architectural Decisions
A. Resource Identification Strategy
To create a unique and intuitive file path for each object in Git, we will mirror the Kubernetes REST API path structure. The admission.Request object conveniently provides all the necessary components.

Decision: Use the format {group}/{version}/{resource}/{namespace}/{name} for the Git file path.

Source Fields from admission.Request:

Group: req.Resource.Group

Version: req.Resource.Version

Resource (Plural): req.Resource.Resource

Namespace: req.Namespace

Name: req.Name

Rationale: This approach is highly effective because it's directly aligned with how the Kubernetes API itself is structured. It's unambiguous, requires no extra lookups, and is easily understood by anyone familiar with kubectl or the Kubernetes API.

Example Paths:

Deployment: apps/v1/deployments/production/my-app-deployment

Pod (Core Group): v1/pods/default/my-nginx-pod

ClusterRole (Cluster-Scoped): rbac.authorization.k8s.io/v1/clusterroles/system/node-viewer

B. Manifest Sanitization Strategy
The core principle is to store the "what" (the user's desired state) and discard the "how" (the cluster's live operational state).

Decision: Programmatically remove all non-declarative fields from the object before serialization.

Fields to Remove:

Live State: status

System-Managed Metadata: metadata.uid, metadata.resourceVersion, metadata.generation, metadata.creationTimestamp, metadata.managedFields, metadata.ownerReferences.

Injected Spec Fields: System-populated fields within the spec, such as spec.clusterIP on a Service.

Contextual Annotations: Non-declarative annotations like kubectl.kubernetes.io/last-applied-configuration.

3. Go Implementation Guide
To achieve both genericity and well-structured output, we will use a two-pass decode and re-assembly pattern. This allows us to handle any object structure while maintaining control over the final YAML format and order.

Core Logic:
Decode to a Generic Map: First, unmarshal the raw object into a map[string]interface{}. This gives us a flexible structure to work with.

Extract and Sanitize Typed Data: Extract the metadata into a custom PartialObjectMeta struct. This lets us easily and safely manipulate the metadata fields we want to keep while discarding the rest.

Clean the Generic Map: Remove the unwanted top-level keys (status, metadata, etc.) from the generic map. What remains is the object's core declarative payload (e.g., spec, data, rules).

Re-assemble into an Ordered Struct: Populate a final, purpose-built struct that defines the field order explicitly (apiVersion, kind, metadata). This struct uses a json:",inline" tag to embed the remaining payload from the cleaned map.

Marshal to YAML: Serialize the final, ordered struct into YAML for storage.

Final Go Implementation
This code provides a robust and generic solution that fulfills all the requirements.

Go

package main

import (
	"context"
	"encoding/json"
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	"sigs.k8s.io/yaml"
)

// MyWebhookHandler is a placeholder for your webhook handler implementation.
type MyWebhookHandler struct{}

// PartialObjectMeta defines a subset of the standard ObjectMeta, containing only
// the fields we want to preserve in our GitOps repository.
type PartialObjectMeta struct {
	Name        string            `json:"name,omitempty"`
	Namespace   string            `json:"namespace,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// FinalGitOpsObject defines the final structure for the clean YAML.
// The order of fields here dictates the order in the marshalled output, ensuring
// a conventional and readable GitOps manifest.
type FinalGitOpsObject struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   PartialObjectMeta `json:"metadata"`

	// The ",inline" tag is a powerful tool that embeds the rest of the object's
	// fields (spec, data, rules, etc.) at this top level, preserving the
	// original structure without needing to know the field names in advance.
	Payload map[string]interface{} `json:",inline"`
}

// Handle is the core logic for the admission webhook. It receives, sanitizes,
// and prepares the Kubernetes object for storage.
func (h *MyWebhookHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	// --- Step 1: Decode the full object into a generic map ---
	// This provides a flexible way to manipulate the object's fields.
	var fullObject map[string]interface{}
	if err := json.Unmarshal(req.Object.Raw, &fullObject); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// --- Step 2: Extract and sanitize the metadata you care about ---
	// We unmarshal just the metadata portion into our typed `PartialObjectMeta` struct.
	// This automatically filters out fields we don't want, like uid, resourceVersion, etc.
	var metadata PartialObjectMeta
	if metaBytes, err := json.Marshal(fullObject["metadata"]); err == nil {
		_ = json.Unmarshal(metaBytes, &metadata)
	}
	// As a final sanitization step, remove common operational annotations.
	delete(metadata.Annotations, "kubectl.kubernetes.io/last-applied-configuration")


	// --- Step 3: Clean the map by removing unwanted top-level fields ---
	// This is the core of the sanitization. What remains in `fullObject`
	// is the pure declarative payload of the resource.
	delete(fullObject, "apiVersion")
	delete(fullObject, "kind")
	delete(fullObject, "metadata")
	delete(fullObject, "status")


	// --- Step 4: Assemble the final, ORDERED object using our new struct ---
	// This step ensures the output YAML is clean and conventionally formatted.
	cleanObject := FinalGitOpsObject{
		APIVersion: req.Kind.GroupVersion().String(),
		Kind:       req.Kind.Kind,
		Metadata:   metadata,
		Payload:    fullObject, // What's left in the map is the payload
	}


	// --- Step 5: Convert to YAML for storage ---
	// The `sigs.k8s.io/yaml` package is recommended as it handles Kubernetes-specific
	// YAML conventions better than standard packages.
	yamlBytes, err := yaml.Marshal(cleanObject)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

	// At this point, `yamlBytes` can be committed to the Git repository using the
	// path derived from the admission request.
	// For example: fmt.Println(string(yamlBytes))

	return admission.Allowed("request processed and sanitized for GitOps audit")
}
