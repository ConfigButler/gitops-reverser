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

// Package controller contains shared constants for all controllers.
package controller

import (
	"context"
	"time"

	configv1alpha2 "github.com/ConfigButler/gitops-reverser/api/v1alpha2"
)

// WatchManagerInterface defines the interface for watch manager reconciliation.
// This allows for easier testing by enabling mock implementations.
type WatchManagerInterface interface {
	ReconcileForRuleChange(ctx context.Context) error
	ResolveWatchRuleResources(ctx context.Context, rule configv1alpha2.WatchRule) (bool, string)
	ResolveClusterWatchRuleResources(ctx context.Context, rule configv1alpha2.ClusterWatchRule) (bool, string)
}

const (
	// ConditionTypeReady indicates whether the resource is ready.
	ConditionTypeReady = "Ready"
	// ConditionTypeResourcesResolved indicates whether rule resources resolved to concrete GVRs.
	ConditionTypeResourcesResolved = "ResourcesResolved"

	// MsgSnapshotCompleted is returned as the condition message when the initial
	// cluster snapshot has been successfully committed to Git.
	MsgSnapshotCompleted = "Initial snapshot reconciliation completed"

	// RequeueShortInterval is the requeue interval for transient errors.
	RequeueShortInterval = 2 * time.Minute
	// RequeueMediumInterval is the requeue interval for auth/secret errors.
	RequeueMediumInterval = 5 * time.Minute
	// RequeueLongInterval is the requeue interval for periodic revalidation.
	RequeueLongInterval = 10 * time.Minute
	// RequeueMaterializationSettleInterval is the requeue interval while a Ready GitTarget
	// still has claimed types pending their checkpoint sync. The materialization roll-up in
	// status is only computed during reconcile, so a Ready target would otherwise report a
	// stale pending state for up to RequeueLongInterval; pending phases settle within
	// seconds, so this keeps status.materialization honest while it converges.
	RequeueMaterializationSettleInterval = 10 * time.Second

	// RetryInitialDuration is the initial duration for exponential backoff retry.
	RetryInitialDuration = 100 * time.Millisecond
	// RetryBackoffFactor is the multiplicative factor for exponential backoff.
	RetryBackoffFactor = 2.0
	// RetryBackoffJitter is the jitter factor for retry backoff.
	RetryBackoffJitter = 0.1
	// RetryMaxSteps is the maximum number of retry attempts.
	RetryMaxSteps = 5

	// ReasonChecking indicates that the controller is checking the resource status.
	ReasonChecking = "Checking"
	// ReasonSecretNotFound indicates that the referenced secret was not found.
	ReasonSecretNotFound = "SecretNotFound"
	// ReasonSecretMalformed indicates that the referenced secret is invalid.
	ReasonSecretMalformed = "SecretMalformed"
	// ReasonConnectionFailed indicates that the connection to the provider failed.
	ReasonConnectionFailed = "ConnectionFailed"
	// ReasonCommitConfigInvalid indicates the commit configuration is invalid.
	ReasonCommitConfigInvalid = "CommitConfigInvalid"
	// ReasonEncryptionConfigInvalid indicates encryption configuration is invalid.
	ReasonEncryptionConfigInvalid = "EncryptionConfigInvalid"
)
