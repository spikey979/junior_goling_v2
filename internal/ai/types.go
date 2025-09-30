package ai

import (
    "context"
    "errors"
    "time"
)

// Request represents a generic AI inference request for a page.
type Request struct {
    JobID      string
    PageID     int
    ContentRef string
    Model      string
    Params     map[string]any
    Timeout    time.Duration
}

type Response struct {
    Text   string
    TokensIn  int
    TokensOut int
}

// Client interface for providers like OpenAI, Anthropic.
type Client interface {
    Name() string
    Do(ctx context.Context, req Request) (Response, error)
}

var (
    ErrRateLimited = errors.New("rate_limited")
)

func IsRateLimited(err error) bool { return errors.Is(err, ErrRateLimited) }

