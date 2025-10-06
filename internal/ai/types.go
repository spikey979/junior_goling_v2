package ai

import (
    "context"
    "errors"
    "time"
)

// Request represents a generic AI inference request for a page.
type Request struct {
    JobID         string
    PageID        int
    ContentRef    string
    Model         string
    Params        map[string]any
    Timeout       time.Duration
    // Vision fields
    ImageBase64   string // Base64 encoded image
    ImageMIME     string // Image MIME type (image/jpeg)
    SystemPrompt  string // System prompt for AI
    ContextText   string // Context from surrounding pages
    MuPDFText     string // Extracted MuPDF text
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
    ErrRateLimited    = errors.New("rate_limited")
    ErrContentRefused = errors.New("content_refused")
)

func IsRateLimited(err error) bool { return errors.Is(err, ErrRateLimited) }
func IsContentRefused(err error) bool { return errors.Is(err, ErrContentRefused) }

