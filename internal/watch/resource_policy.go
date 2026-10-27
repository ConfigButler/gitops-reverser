// SPDX-License-Identifier: Apache-2.0

package watch

const defaultResourceExclusionReason = "excluded by GitOps Reverser default watch policy"

func allowedResource(group, resource string) (bool, string) {
	if isDefaultResourceExcluded(group, resource) {
		return false, defaultResourceExclusionReason
	}
	return true, ""
}

func isDefaultResourceExcluded(group, resource string) bool {
	switch groupResourceKey(group, resource) {
	case groupResourceKey("", "pods"),
		groupResourceKey("", "events"),
		groupResourceKey("events.k8s.io", "events"),
		groupResourceKey("", "endpoints"),
		groupResourceKey("discovery.k8s.io", "endpointslices"),
		groupResourceKey("coordination.k8s.io", "leases"),
		groupResourceKey("apps", "controllerrevisions"),
		groupResourceKey("flowcontrol.apiserver.k8s.io", "flowschemas"),
		groupResourceKey("flowcontrol.apiserver.k8s.io", "prioritylevelconfigurations"),
		groupResourceKey("batch", "jobs"),
		groupResourceKey("batch", "cronjobs"),
		// A save is a request to GitOps Reverser, never state to mirror (commit_requests.go).
		groupResourceKey("source.configbutler.ai", "remotesaverequests"):
		return true
	default:
		return false
	}
}
