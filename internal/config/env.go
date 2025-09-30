package config

import (
    "os"
    "strconv"
    "strings"
    "time"
)

// LoggingConfig holds logging-related configuration.
type LoggingConfig struct {
    Level        string
    Pretty       bool
    File         string
    MaxSizeMB    int
    MaxBackups   int
    MaxAgeDays   int
    Compress     bool
}

// AxiomConfig holds Axiom logging configuration.
type AxiomConfig struct {
    Send          bool
    APIKey        string
    OrgID         string
    Dataset       string
    FlushInterval time.Duration
}

// ProviderModels defines model triplet for a provider.
type ProviderModels struct {
    Primary   string
    Secondary string
    Fast      string
}

// ProvidersConfig defines engines and models per provider.
type ProvidersConfig struct {
    PrimaryEngine   string // "openai"|"anthropic"
    SecondaryEngine string // "anthropic"|"openai"
    OpenAI          ProviderModels
    Anthropic       ProviderModels
}

// WorkerConfig defines worker behavior and limits.
type WorkerConfig struct {
    Concurrency          int
    RequestTimeout       time.Duration
    OpenAITimeout        time.Duration
    AnthropicTimeout     time.Duration
    PageTotalTimeout     time.Duration
    JobMaxAttempts       int
    RetryBaseDelay       time.Duration
    RetryJitter          time.Duration
    RetryBackoffFactor   float64
    MaxInflightPerModel  int
    BreakerBaseBackoff   time.Duration
    BreakerMaxBackoff    time.Duration
}

// QueueConfig defines queue connectivity and names.
type QueueConfig struct {
    RedisURL     string
    Stream       string
    Group        string
    PollInterval time.Duration
}

// Config is the top-level configuration.
type Config struct {
    Logging   LoggingConfig
    Axiom     AxiomConfig
    Providers ProvidersConfig
    Worker    WorkerConfig
    Queue     QueueConfig
}

// FromEnv loads configuration from environment with sensible defaults.
func FromEnv() Config {
    cfg := Config{}

    // Logging defaults
    cfg.Logging = LoggingConfig{
        Level:      getEnv("LOG_LEVEL", "info"),
        Pretty:     parseBool(getEnv("LOG_PRETTY", devDefaultPretty())),
        File:       getEnv("LOG_FILE", "logs/aidispathcher.log"),
        MaxSizeMB:  parseInt(getEnv("LOG_MAX_SIZE_MB", "100"), 100),
        MaxBackups: parseInt(getEnv("LOG_MAX_BACKUPS", "10"), 10),
        MaxAgeDays: parseInt(getEnv("LOG_MAX_AGE_DAYS", "30"), 30),
        Compress:   parseBool(getEnv("LOG_COMPRESS", "true")),
    }

    // Axiom defaults
    baseDataset := getEnv("AXIOM_DATASET", "dev")
    cfg.Axiom = AxiomConfig{
        Send:          parseBool(getEnv("SEND_LOGS_TO_AXIOM", "0")),
        APIKey:        getEnv("AXIOM_API_KEY", ""),
        OrgID:         getEnv("AXIOM_ORG_ID", ""),
        Dataset:       baseDataset + "_aidispatcher",
        FlushInterval: parseDuration(getEnv("AXIOM_FLUSH_INTERVAL", "10s"), 10*time.Second),
    }

    // Providers defaults
    cfg.Providers = ProvidersConfig{
        PrimaryEngine:   getEnv("PRIMARY_ENGINE", "openai"),
        SecondaryEngine: getEnv("SECONDARY_ENGINE", "anthropic"),
        OpenAI: ProviderModels{
            Primary:   getEnv("OPENAI_PRIMARY_MODEL", "gpt-4.1"),
            Secondary: getEnv("OPENAI_SECONDARY_MODEL", "gpt-4o"),
            Fast:      getEnv("OPENAI_FAST_MODEL", "gpt-4.1-mini"),
        },
        Anthropic: ProviderModels{
            Primary:   getEnv("ANTHROPIC_PRIMARY_MODEL", "claude-3-5-sonnet"),
            Secondary: getEnv("ANTHROPIC_SECONDARY_MODEL", "claude-3-opus"),
            Fast:      getEnv("ANTHROPIC_FAST_MODEL", "claude-3-haiku"),
        },
    }

    // Worker defaults
    cfg.Worker = WorkerConfig{
        Concurrency:        parseInt(getEnv("WORKER_CONCURRENCY", "8"), 8),
        RequestTimeout:     parseDuration(getEnv("REQUEST_TIMEOUT", "60s"), 60*time.Second),
        OpenAITimeout:      parseDuration(getEnv("OPENAI_TIMEOUT", ""), 0),
        AnthropicTimeout:   parseDuration(getEnv("ANTHROPIC_TIMEOUT", ""), 0),
        PageTotalTimeout:   parseDuration(getEnv("PAGE_TOTAL_TIMEOUT", "120s"), 120*time.Second),
        JobMaxAttempts:     parseInt(getEnv("JOB_MAX_ATTEMPTS", "3"), 3),
        RetryBaseDelay:     parseDuration(getEnv("RETRY_BASE_DELAY", "2s"), 2*time.Second),
        RetryJitter:        parseDuration(getEnv("RETRY_JITTER", "200ms"), 200*time.Millisecond),
        RetryBackoffFactor: parseFloat(getEnv("RETRY_BACKOFF_FACTOR", "2.0"), 2.0),
        MaxInflightPerModel: parseInt(getEnv("MAX_INFLIGHT_PER_MODEL", "2"), 2),
        BreakerBaseBackoff:  parseDuration(getEnv("BREAKER_BASE_BACKOFF", "30s"), 30*time.Second),
        BreakerMaxBackoff:   parseDuration(getEnv("BREAKER_MAX_BACKOFF", "5m"), 5*time.Minute),
    }
    if cfg.Worker.OpenAITimeout <= 0 { cfg.Worker.OpenAITimeout = cfg.Worker.RequestTimeout }
    if cfg.Worker.AnthropicTimeout <= 0 { cfg.Worker.AnthropicTimeout = cfg.Worker.RequestTimeout }

    // Queue defaults
    cfg.Queue = QueueConfig{
        RedisURL:     getEnv("REDIS_URL", "redis://localhost:6379"),
        Stream:       getEnv("QUEUE_STREAM", "jobs:ai:pages"),
        Group:        getEnv("QUEUE_GROUP", "workers:images"),
        PollInterval: parseDuration(getEnv("QUEUE_POLL_INTERVAL", "100ms"), 100*time.Millisecond),
    }

    return cfg
}

// Helpers
func getEnv(key, def string) string {
    if v := os.Getenv(key); v != "" {
        return v
    }
    return def
}

func parseInt(s string, def int) int {
    if s == "" { return def }
    if n, err := strconv.Atoi(s); err == nil { return n }
    return def
}

func parseFloat(s string, def float64) float64 {
    if s == "" { return def }
    if f, err := strconv.ParseFloat(s, 64); err == nil { return f }
    return def
}

func parseBool(s string) bool {
    v := strings.ToLower(strings.TrimSpace(s))
    return v == "1" || v == "true" || v == "yes" || v == "on"
}

func parseDuration(s string, def time.Duration) time.Duration {
    if s == "" { return def }
    if d, err := time.ParseDuration(s); err == nil { return d }
    return def
}

func devDefaultPretty() string {
    env := strings.ToLower(os.Getenv("ENVIRONMENT"))
    if env == "dev" || env == "development" || env == "local" { return "true" }
    return "false"
}
