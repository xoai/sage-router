package executor

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// RetryConfig controls the retry behaviour for upstream requests.
type RetryConfig struct {
	MaxRetries int
	BaseDelay  time.Duration
	MaxDelay   time.Duration
}

// DefaultRetryConfig returns a sensible default retry configuration.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries: 3,
		BaseDelay:  500 * time.Millisecond,
		MaxDelay:   10 * time.Second,
	}
}

// retryableStatusCodes is the set of HTTP status codes that should trigger
// an automatic retry on the same connection.
var retryableStatusCodes = map[int]bool{
	429: true, // Too Many Requests
	500: true, // Internal Server Error
	502: true, // Bad Gateway
	503: true, // Service Unavailable
	504: true, // Gateway Timeout
}

// IsRetryable reports whether a response with the given status code should be
// retried on the same connection.
func IsRetryable(statusCode int) bool {
	return retryableStatusCodes[statusCode]
}

// fallbackEligibleStatusCodes is the set of HTTP status codes that make the
// request eligible for fallback to a different connection or provider.
var fallbackEligibleStatusCodes = map[int]bool{
	401: true, // Unauthorized
	403: true, // Forbidden
	429: true, // Too Many Requests
	500: true, // Internal Server Error
	502: true, // Bad Gateway
	503: true, // Service Unavailable
	504: true, // Gateway Timeout
}

// IsFallbackEligible reports whether a response with the given status code
// should trigger fallback to another connection or provider.
func IsFallbackEligible(statusCode int) bool {
	return fallbackEligibleStatusCodes[statusCode]
}

// RetryExecutor wraps an Executor with automatic retry on transient errors.
type RetryExecutor struct {
	inner  Executor
	config RetryConfig
}

// NewRetryExecutor wraps an executor with retry logic.
func NewRetryExecutor(inner Executor, cfg RetryConfig) *RetryExecutor {
	return &RetryExecutor{inner: inner, config: cfg}
}

func (r *RetryExecutor) Provider() string {
	return r.inner.Provider()
}

func (r *RetryExecutor) Execute(ctx context.Context, req *ExecuteRequest) (*Result, error) {
	var lastResult *Result
	var lastErr error

	for attempt := 0; attempt <= r.config.MaxRetries; attempt++ {
		if attempt > 0 {
			delay := r.config.BaseDelay << (attempt - 1)
			if delay > r.config.MaxDelay {
				delay = r.config.MaxDelay
			}
			slog.Debug("retrying upstream request",
				"provider", r.inner.Provider(),
				"attempt", attempt,
				"delay", delay,
			)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		result, err := r.inner.Execute(ctx, req)
		if err != nil {
			lastErr = err
			continue
		}

		// Never retry streaming responses that already returned success
		if req.Stream && result.StatusCode < 400 {
			return result, nil
		}

		if !IsRetryable(result.StatusCode) {
			return result, nil
		}

		// Close body before retrying
		result.Body.Close()
		lastResult = result
		lastErr = fmt.Errorf("upstream returned %d", result.StatusCode)
	}

	// If we have a last result (retryable status), return it so the caller
	// can inspect the status code for fallback decisions
	if lastResult != nil {
		return lastResult, nil
	}
	return nil, fmt.Errorf("all %d retries exhausted: %w", r.config.MaxRetries, lastErr)
}
