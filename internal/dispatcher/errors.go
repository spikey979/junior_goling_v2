package dispatcher

import "fmt"

// RateLimitError represents a rate limit or timeout error
type RateLimitError struct {
	Provider string
	Model    string
	Reason   string
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("rate limit: %s/%s - %s", e.Provider, e.Model, e.Reason)
}

// HTTPError represents an HTTP status error from AI provider
type HTTPError struct {
	StatusCode int
	Body       string
	Provider   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("HTTP %d from %s: %s", e.StatusCode, e.Provider, e.Body)
}

// ValidationError represents a fatal validation error
type ValidationError struct {
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation error: %s", e.Message)
}
