package dispatcher

import (
	"context"
	"errors"
	"strings"

	"github.com/local/aidispatcher/internal/ai"
)

// isTransientError checks if error is transient and should trigger failover
func isTransientError(err error) bool {
	if err == nil {
		return false
	}

	// Content refusal - try alternative models
	if ai.IsContentRefused(err) {
		return true
	}

	// Timeout errors
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	// RateLimitError
	var rateLimitErr *RateLimitError
	if errors.As(err, &rateLimitErr) {
		return true
	}

	// HTTP errors
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		// 5xx server errors are transient
		if httpErr.StatusCode >= 500 && httpErr.StatusCode < 600 {
			return true
		}
		// 429 rate limit is transient
		if httpErr.StatusCode == 429 {
			return true
		}
	}

	// Network errors (connection issues, timeouts)
	errStr := strings.ToLower(err.Error())
	if strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "timeout") ||
		strings.Contains(errStr, "network") ||
		strings.Contains(errStr, "eof") {
		return true
	}

	return false
}

// isFatalError checks if error is fatal and should not be retried
func isFatalError(err error) bool {
	if err == nil {
		return false
	}

	// ValidationError
	var valErr *ValidationError
	if errors.As(err, &valErr) {
		return true
	}

	// HTTP 4xx errors (except 429)
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		if httpErr.StatusCode >= 400 && httpErr.StatusCode < 500 && httpErr.StatusCode != 429 {
			return true
		}
	}

	// Validation keywords in error message
	errStr := strings.ToLower(err.Error())
	if strings.Contains(errStr, "invalid request") ||
		strings.Contains(errStr, "validation failed") ||
		strings.Contains(errStr, "bad request") ||
		strings.Contains(errStr, "malformed") {
		return true
	}

	return false
}

// isTimeoutError checks if error is specifically a timeout
func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "timeout") || strings.Contains(errStr, "deadline exceeded")
}
