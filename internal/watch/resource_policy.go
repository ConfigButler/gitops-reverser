/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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
		groupResourceKey("batch", "cronjobs"):
		return true
	default:
		return false
	}
}
