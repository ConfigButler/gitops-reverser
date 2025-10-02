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
