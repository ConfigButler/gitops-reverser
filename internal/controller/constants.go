/*
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

import "time"

const (
	// ConditionTypeReady indicates whether the resource is ready.
	ConditionTypeReady = "Ready"

	// RequeueShortInterval is the requeue interval for transient errors.
	RequeueShortInterval = 2 * time.Minute
	// RequeueMediumInterval is the requeue interval for auth/secret errors.
	RequeueMediumInterval = 5 * time.Minute
	// RequeueLongInterval is the requeue interval for periodic revalidation.
	RequeueLongInterval = 10 * time.Minute

	// RetryInitialDuration is the initial duration for exponential backoff retry.
	RetryInitialDuration = 100 * time.Millisecond
	// RetryBackoffFactor is the multiplicative factor for exponential backoff.
	RetryBackoffFactor = 2.0
	// RetryBackoffJitter is the jitter factor for retry backoff.
	RetryBackoffJitter = 0.1
	// RetryMaxSteps is the maximum number of retry attempts.
	RetryMaxSteps = 5
)
