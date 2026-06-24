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

// Package e2e drives the mutation-capture lab against a live cluster: it runs
// each scenario, drains the lab's records API, and compares (or, with
// MUTATIONLAB_UPDATE=1, rewrites) the golden corpus.
//
// The driver is behind the `mutationlab_e2e` build tag so it never compiles into
// the default `go test ./...` unit lane (which has no cluster). This file is the
// untagged placeholder that keeps the package buildable when the tag is absent;
// `task lab-e2e` builds it with -tags mutationlab_e2e.
package e2e
