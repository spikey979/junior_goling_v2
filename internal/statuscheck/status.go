package statuscheck

import (
    "context"
    "errors"
    "fmt"
    "net/http"
    "os/exec"
    "strings"
    "time"

    awscfg "github.com/aws/aws-sdk-go-v2/config"
    "github.com/aws/aws-sdk-go-v2/service/s3"
)

// RedisPinger models the minimal Redis capability we need for status checks.
type RedisPinger interface {
    Ping(ctx context.Context) error
}

// Checker aggregates health checks for external dependencies used by the dashboard.
type Checker struct {
    redis        RedisPinger
    s3Bucket     string
    httpClient   *http.Client
    openAIKey    string
    anthropicKey string
}

// Options configures the Checker.
type Options struct {
    Redis      RedisPinger
    S3Bucket   string
    HTTPClient *http.Client
    OpenAIKey  string
    AnthropicKey string
}

// Status represents the readiness of a subsystem.
type Status struct {
    OK      bool   `json:"ok"`
    Message string `json:"message"`
}

// Summary bundles all subsystem statuses for the dashboard.
type Summary struct {
    Redis       Status `json:"redis"`
    S3          Status `json:"s3"`
    LibreOffice Status `json:"libreoffice"`
    OpenAI      Status `json:"openai"`
    Anthropic   Status `json:"anthropic"`
    MuPDF       Status `json:"mupdf"`
}

// New creates a new Checker with the provided options.
func New(opts Options) *Checker {
    client := opts.HTTPClient
    if client == nil {
        client = &http.Client{Timeout: 5 * time.Second}
    }
    return &Checker{
        redis:        opts.Redis,
        s3Bucket:     opts.S3Bucket,
        httpClient:   client,
        openAIKey:    strings.TrimSpace(opts.OpenAIKey),
        anthropicKey: strings.TrimSpace(opts.AnthropicKey),
    }
}

// Summary returns the current status snapshot.
func (c *Checker) Summary(ctx context.Context) Summary {
    return Summary{
        Redis:       c.checkRedis(ctx),
        S3:          c.checkS3(ctx),
        LibreOffice: c.checkLibreOffice(),
        OpenAI:      c.checkOpenAI(ctx),
        Anthropic:   c.checkAnthropic(ctx),
        MuPDF:       c.checkMuPDF(),
    }
}

func (c *Checker) checkRedis(ctx context.Context) Status {
    if c.redis == nil {
        return Status{OK: false, Message: "client unavailable"}
    }
    ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
    defer cancel()
    if err := c.redis.Ping(ctx); err != nil {
        return Status{OK: false, Message: err.Error()}
    }
    return Status{OK: true, Message: "Connected"}
}

func (c *Checker) checkS3(ctx context.Context) Status {
    if c.s3Bucket == "" {
        return Status{OK: false, Message: "Bucket not configured"}
    }
    ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
    defer cancel()
    cfg, err := awscfg.LoadDefaultConfig(ctx)
    if err != nil {
        return Status{OK: false, Message: err.Error()}
    }
    cli := s3.NewFromConfig(cfg)
    _, err = cli.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &c.s3Bucket})
    if err != nil {
        return Status{OK: false, Message: trimError(err)}
    }
    return Status{OK: true, Message: "Connected"}
}

func (c *Checker) checkLibreOffice() Status {
    if _, err := exec.LookPath("soffice"); err != nil {
        return Status{OK: false, Message: "Binary not found"}
    }
    return Status{OK: true, Message: "Running"}
}

func (c *Checker) checkOpenAI(ctx context.Context) Status {
    if c.openAIKey == "" {
        return Status{OK: false, Message: "API key missing"}
    }
    req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.openai.com/v1/models?limit=1", nil)
    req.Header.Set("Authorization", "Bearer "+c.openAIKey)
    resp, err := c.httpClient.Do(req)
    if err != nil {
        return Status{OK: false, Message: trimError(err)}
    }
    defer resp.Body.Close()
    if resp.StatusCode >= 400 {
        return Status{OK: false, Message: fmt.Sprintf("HTTP %d", resp.StatusCode)}
    }
    return Status{OK: true, Message: "Available"}
}

func (c *Checker) checkAnthropic(ctx context.Context) Status {
    if c.anthropicKey == "" {
        return Status{OK: false, Message: "API key missing"}
    }
    req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.anthropic.com/v1/models", nil)
    req.Header.Set("x-api-key", c.anthropicKey)
    req.Header.Set("anthropic-version", "2023-06-01")
    resp, err := c.httpClient.Do(req)
    if err != nil {
        return Status{OK: false, Message: trimError(err)}
    }
    defer resp.Body.Close()
    if resp.StatusCode >= 400 {
        return Status{OK: false, Message: fmt.Sprintf("HTTP %d", resp.StatusCode)}
    }
    return Status{OK: true, Message: "Available"}
}

func (c *Checker) checkMuPDF() Status {
    if _, err := exec.LookPath("mutool"); err != nil {
        return Status{OK: false, Message: "Binary not found"}
    }
    return Status{OK: true, Message: "Available"}
}

func trimError(err error) string {
    if err == nil {
        return ""
    }
    var netErr interface{ Timeout() bool }
    if errors.As(err, &netErr) && netErr.Timeout() {
        return "timeout"
    }
    msg := err.Error()
    if len(msg) > 120 {
        return msg[:120]
    }
    return msg
}
