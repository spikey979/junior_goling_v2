package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// LoggingConfig holds logging-related configuration.
type LoggingConfig struct {
	Level      string
	Pretty     bool
	File       string
	MaxSizeMB  int
	MaxBackups int
	MaxAgeDays int
	Compress   bool
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
	Concurrency         int
	RequestTimeout      time.Duration
	OpenAITimeout       time.Duration
	AnthropicTimeout    time.Duration
	PageTotalTimeout    time.Duration
	JobMaxAttempts      int
	RetryBaseDelay      time.Duration
	RetryJitter         time.Duration
	RetryBackoffFactor  float64
	MaxInflightPerModel int
	BreakerBaseBackoff  time.Duration
	BreakerMaxBackoff   time.Duration
}

// QueueConfig defines queue connectivity and names.
type QueueConfig struct {
	RedisURL     string
	Stream       string
	Group        string
	PollInterval time.Duration
}

// ImageOptions defines image rendering options for AI processing
type ImageOptions struct {
	SendAllPages    bool   // false = only HAS_GRAPHICS pages get images
	DPI             int    // rendering DPI (100)
	Color           string // "rgb" or "gray"
	Format          string // "jpeg"
	JPEGQuality     int    // 70 (1-100)
	ContextRadius   int    // ±N pages for context (1)
	MaxContextBytes int    // max context text size in bytes (3000)
	IncludeBase64   bool   // convert to base64 for JSON transport
}

// TimeoutConfig holds timeout settings for requests and jobs
type TimeoutConfig struct {
	RequestTimeout time.Duration // Max time per AI API call (default: 120s)
	JobTimeout     time.Duration // Max time for entire document (default: 5m)
}

// SystemPromptConfig holds AI prompt configuration
type SystemPromptConfig struct {
	DefaultPrompt string
}

// Config is the top-level configuration.
type Config struct {
	Logging      LoggingConfig
	Axiom        AxiomConfig
	Providers    ProvidersConfig
	Worker       WorkerConfig
	Queue        QueueConfig
	Image        ImageOptions
	Timeouts     TimeoutConfig
	SystemPrompt SystemPromptConfig
}

// FromEnv loads configuration from environment with sensible defaults.
func FromEnv() Config {
	cfg := Config{}

	// Logging defaults
	cfg.Logging = LoggingConfig{
		Level:      getEnv("LOG_LEVEL", "info"),
		Pretty:     parseBool(getEnv("LOG_PRETTY", devDefaultPretty())),
		File:       getEnv("LOG_FILE", "logs/fileapi.log"),
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
		Concurrency:         parseInt(getEnv("WORKER_CONCURRENCY", "8"), 8),
		RequestTimeout:      parseDuration(getEnv("REQUEST_TIMEOUT", "60s"), 60*time.Second),
		OpenAITimeout:       parseDuration(getEnv("OPENAI_TIMEOUT", ""), 0),
		AnthropicTimeout:    parseDuration(getEnv("ANTHROPIC_TIMEOUT", ""), 0),
		PageTotalTimeout:    parseDuration(getEnv("PAGE_TOTAL_TIMEOUT", "120s"), 120*time.Second),
		JobMaxAttempts:      parseInt(getEnv("JOB_MAX_ATTEMPTS", "3"), 3),
		RetryBaseDelay:      parseDuration(getEnv("RETRY_BASE_DELAY", "2s"), 2*time.Second),
		RetryJitter:         parseDuration(getEnv("RETRY_JITTER", "200ms"), 200*time.Millisecond),
		RetryBackoffFactor:  parseFloat(getEnv("RETRY_BACKOFF_FACTOR", "2.0"), 2.0),
		MaxInflightPerModel: parseInt(getEnv("MAX_INFLIGHT_PER_MODEL", "2"), 2),
		BreakerBaseBackoff:  parseDuration(getEnv("BREAKER_BASE_BACKOFF", "30s"), 30*time.Second),
		BreakerMaxBackoff:   parseDuration(getEnv("BREAKER_MAX_BACKOFF", "5m"), 5*time.Minute),
	}
	if cfg.Worker.OpenAITimeout <= 0 {
		cfg.Worker.OpenAITimeout = cfg.Worker.RequestTimeout
	}
	if cfg.Worker.AnthropicTimeout <= 0 {
		cfg.Worker.AnthropicTimeout = cfg.Worker.RequestTimeout
	}

	// Queue defaults
	cfg.Queue = QueueConfig{
		RedisURL:     getEnv("REDIS_URL", "redis://localhost:6379"),
		Stream:       getEnv("QUEUE_STREAM", "jobs:ai:pages"),
		Group:        getEnv("QUEUE_GROUP", "workers:images"),
		PollInterval: parseDuration(getEnv("QUEUE_POLL_INTERVAL", "100ms"), 100*time.Millisecond),
	}

	// Image defaults
	cfg.Image = DefaultImageOptions()

	// Timeout configuration with backwards compatibility
	requestTimeout := getEnv("REQUEST_TIMEOUT", "")
	if requestTimeout == "" {
		requestTimeout = getEnv("PAGE_TOTAL_TIMEOUT", "120s")
	}

	jobTimeout := getEnv("JOB_TIMEOUT", "")
	if jobTimeout == "" {
		jobTimeout = getEnv("DOCUMENT_PROCESSING_TIMEOUT", "5m")
	}

	cfg.Timeouts = TimeoutConfig{
		RequestTimeout: parseDuration(requestTimeout, 120*time.Second),
		JobTimeout:     parseDuration(jobTimeout, 5*time.Minute),
	}

	// System prompt configuration
	cfg.SystemPrompt = SystemPromptConfig{
		DefaultPrompt: getEnv("AI_SYSTEM_PROMPT", DefaultSystemPrompt()),
	}

	return cfg
}

// DefaultImageOptions returns default image rendering options
func DefaultImageOptions() ImageOptions {
	return ImageOptions{
		SendAllPages:    parseBool(getEnv("IMAGE_SEND_ALL_PAGES", "false")),
		DPI:             parseInt(getEnv("IMAGE_DPI", "100"), 100),
		Color:           getEnv("IMAGE_COLOR", "rgb"),
		Format:          getEnv("IMAGE_FORMAT", "jpeg"),
		JPEGQuality:     parseInt(getEnv("IMAGE_JPEG_QUALITY", "70"), 70),
		ContextRadius:   parseInt(getEnv("IMAGE_CONTEXT_RADIUS", "1"), 1),
		MaxContextBytes: parseInt(getEnv("IMAGE_MAX_CONTEXT_BYTES", "3000"), 3000),
		IncludeBase64:   parseBool(getEnv("IMAGE_INCLUDE_BASE64", "true")),
	}
}

// Helpers
func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseInt(s string, def int) int {
	if s == "" {
		return def
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func parseFloat(s string, def float64) float64 {
	if s == "" {
		return def
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return def
}

func parseBool(s string) bool {
	v := strings.ToLower(strings.TrimSpace(s))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func parseDuration(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	return def
}

func devDefaultPretty() string {
	env := strings.ToLower(os.Getenv("ENVIRONMENT"))
	if env == "dev" || env == "development" || env == "local" {
		return "true"
	}
	return "false"
}

// DefaultSystemPrompt returns the default AI system prompt
func DefaultSystemPrompt() string {
	return `You are an expert document analysis AI with advanced OCR and vision capabilities. Your task is to:

INPUT FORMAT:
You will receive:
1. CURRENT PAGE NUMBER - the page you are analyzing
2. CONTEXT (from surrounding pages) - text from pages before/after the current page
3. MUPDF EXTRACTED TEXT (from current page) - preliminary text extraction from current page
4. An image of the current page

RULES:
- PRIMARY: Extract ALL visible text from the CURRENT PAGE image with maximum accuracy
- **MANDATORY OUTPUT FORMAT: Always start your response with exactly "=== Page N ===" (where N is the current page number provided in the input) on the first line**
- Preserve reading order, structure, and layout (multi-column, tables, lists, headers, formulas, symbols)
- Maintain original wording EXACTLY (do not paraphrase or comment)
- Rewrite the extracted text in a **single-column flow**, ignoring any original multi-column or block layout.
- Include figure/image/diagram/table descriptions in reading order using this format:
  [Figure/Image/Table Name or Number]
  [Detailed technical description - USE CONTEXT TEXT to enrich the description if available]
- **CRITICAL: Describe ALL visual elements (figures, images, diagrams, charts, graphs, tables) in DETAIL. Include:**
  - **What it shows** (type of visualization, subject matter)
  - **All visible data** (axes labels, values, ranges, units, legends, colors, patterns)
  - **Key observations** (trends, patterns, comparisons, notable values)
  - **Context information** from surrounding pages (location, time period, what is being measured, legend explanations)
  - Be comprehensive but clear - imagine describing to someone who cannot see the image
- **When describing figures/images/diagrams, carefully read the CONTEXT TEXT from surrounding pages to find relevant information that describes what the figure shows (e.g., location, time period, what is being measured). Incorporate this information into your description.**
- **If CONTEXT TEXT mentions "the graph/figure/diagram on the previous/next/below/above page", that text is referring to the current page you are analyzing. Use that information in your description.**
- **IMPORTANT: If CONTEXT TEXT provides legend/color explanations (e.g., "orange indicates X, blue indicates Y"), those MUST be used instead of or in addition to any legend visible on the image itself. Always prioritize descriptive context from surrounding text over generic labels like "Period 1/2".**
- Use CONTEXT (text from surrounding pages) ONLY as supporting reference to better describe non-text elements (figures, charts, diagrams, tables) or resolve unclear symbols — never to change or override actual text from the current page
- Exclude page headers/footers, watermarks, or irrelevant artifacts
- **NEVER add meta-commentary or observations about the content**, such as:
  - "No additional text is present on the page"
  - "No images are visible"
  - "The page appears to be..."
  - "This seems to be..."
  - Any explanatory text that is NOT part of the original document
- Output must start immediately with "=== Page N ===" followed by the document text ONLY

GOAL:
Return the document content in a structured, accurate, and comprehensive way that preserves the original meaning and organization, while actively using CONTEXT TEXT to create rich, informative descriptions of graphical elements.`
}
