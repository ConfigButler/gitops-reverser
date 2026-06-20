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

package auditutil

import (
	"testing"

	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"

	configv1alpha2 "github.com/ConfigButler/gitops-reverser/api/v1alpha2"
)

func TestSplitAPIVersion(t *testing.T) {
	tests := []struct {
		name       string
		apiVersion string
		group      string
		version    string
	}{
		{name: "core", apiVersion: "v1", version: "v1"},
		{name: "group", apiVersion: "apps/v1", group: "apps", version: "v1"},
		{name: "empty", apiVersion: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			group, version := SplitAPIVersion(tt.apiVersion)
			if group != tt.group || version != tt.version {
				t.Fatalf("SplitAPIVersion(%q) = %q/%q, want %q/%q",
					tt.apiVersion, group, version, tt.group, tt.version)
			}
		})
	}
}

func TestVerbToOperation(t *testing.T) {
	tests := []struct {
		verb string
		op   configv1alpha2.OperationType
		ok   bool
	}{
		{verb: "create", op: configv1alpha2.OperationCreate, ok: true},
		{verb: "CREATE", op: configv1alpha2.OperationCreate, ok: true},
		{verb: "update", op: configv1alpha2.OperationUpdate, ok: true},
		{verb: "patch", op: configv1alpha2.OperationUpdate, ok: true},
		{verb: "delete", op: configv1alpha2.OperationDelete, ok: true},
		{verb: "deletecollection", op: configv1alpha2.OperationDelete, ok: true},
		{verb: "get", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.verb, func(t *testing.T) {
			op, ok := VerbToOperation(tt.verb)
			if op != tt.op || ok != tt.ok {
				t.Fatalf("VerbToOperation(%q) = %q, %t; want %q, %t", tt.verb, op, ok, tt.op, tt.ok)
			}
		})
	}
}

func TestObjectRefGVR(t *testing.T) {
	ref := &auditv1.ObjectReference{APIGroup: "wardle.example.com", APIVersion: "v1alpha1", Resource: "flunders"}
	gvr, ok := ObjectRefGVR(ref)
	if !ok {
		t.Fatal("ObjectRefGVR should parse a resource reference")
	}
	if gvr.Group != "wardle.example.com" || gvr.Version != "v1alpha1" || gvr.Resource != "flunders" {
		t.Fatalf("ObjectRefGVR = %#v", gvr)
	}
	if _, ok := ObjectRefGVR(&auditv1.ObjectReference{APIVersion: "v1"}); ok {
		t.Fatal("ObjectRefGVR should reject references without resources")
	}
}
