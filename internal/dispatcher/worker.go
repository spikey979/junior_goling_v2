package dispatcher

import (
    "context"
    "encoding/json"
    "fmt"
    "net/http"
    "os"
    "time"
    "strings"
    "bytes"

    "github.com/local/aidispatcher/internal/ai"
    "github.com/local/aidispatcher/internal/limiter"
    mpkg "github.com/local/aidispatcher/internal/metrics"
    cfgpkg "github.com/local/aidispatcher/internal/config"
    redis "github.com/redis/go-redis/v9"
    "github.com/rs/zerolog/log"
)

type Queue interface {
    DequeueAI(ctx context.Context, consumer string, timeout time.Duration) (string, []byte, error)
    Ack(ctx context.Context, msgID string) error
    IsCancelled(ctx context.Context, jobID string) (bool, error)
    EnqueueDelayed(ctx context.Context, payload []byte, executeAt time.Time) error
    AddDLQ(ctx context.Context, payload []byte, reason string) error
    IsIdemDone(ctx context.Context, key string) (bool, error)
    MarkIdemDone(ctx context.Context, key string, ttl time.Duration) error
}

type Config struct {
    Concurrency int
}

type Worker struct {
    cfg     Config
    q       Queue
    stop    chan struct{}
    conf    cfgpkg.Config
    openai  ai.Client
    anthropic ai.Client
    lim     *limiter.Adaptive
    breaker *CircuitBreaker
    redis   *redis.Client
}

func New(cfg Config, q Queue) *Worker {
    if cfg.Concurrency <= 0 { cfg.Concurrency = 2 }
    conf := cfgpkg.FromEnv()
    lim, _ := limiter.New(limiter.Options{RedisURL: conf.Queue.RedisURL, MaxInflight: conf.Worker.MaxInflightPerModel, BaseBackoff: conf.Worker.BreakerBaseBackoff, MaxBackoff: conf.Worker.BreakerMaxBackoff})

    // Initialize Redis client for circuit breaker
    opt, _ := redis.ParseURL(conf.Queue.RedisURL)
    redisClient := redis.NewClient(opt)

    breaker := NewCircuitBreaker(redisClient, conf.Worker.BreakerBaseBackoff, conf.Worker.BreakerMaxBackoff)

    return &Worker{
        cfg:       cfg,
        q:         q,
        stop:      make(chan struct{}),
        conf:      conf,
        openai:    ai.NewOpenAIClient(),
        anthropic: ai.NewAnthropicClient(),
        lim:       lim,
        breaker:   breaker,
        redis:     redisClient,
    }
}

func (w *Worker) Start() {
    for i := 0; i < w.cfg.Concurrency; i++ {
        go w.loop(i)
    }
}

func (w *Worker) Stop(ctx context.Context) error {
    close(w.stop)
    return nil
}

func (w *Worker) loop(id int) {
    log.Info().Int("worker", id).Msg("dispatcher worker started")
    port := getenv("PORT", "8080")
    for {
        select {
        case <-w.stop:
            log.Info().Int("worker", id).Msg("dispatcher worker stopped")
            return
        default:
        }

        msgID, data, err := w.q.DequeueAI(context.Background(), fmt.Sprintf("w-%d", id), 2*time.Second)
        if err != nil {
            log.Error().Err(err).Msg("queue dequeue error")
            time.Sleep(500 * time.Millisecond)
            continue
        }
        if data == nil { continue }

        // Process job with timeouts and failover
        var payload map[string]any
        _ = json.Unmarshal(data, &payload)
        jobID, _ := payload["job_id"].(string)
        pageID := intFromAny(payload["page_id"]) 
        if jobID != "" {
            if cancelled, _ := w.q.IsCancelled(context.Background(), jobID); cancelled {
                log.Warn().Int("worker", id).Str("job_id", jobID).Int("page_id", pageID).Msg("job cancelled before processing; skipping")
                continue
            }
        }
        contentRef, _ := payload["content_ref"].(string)
        preferEngine, _ := payload["ai_engine"].(string)
        forceFast := boolFromAny(payload["force_fast"])

        // Extract vision fields from payload
        imageB64, _ := payload["image_b64"].(string)
        imageMIME, _ := payload["image_mime"].(string)
        systemPrompt, _ := payload["system_prompt"].(string)
        contextText, _ := payload["context_text"].(string)
        mupdfText, _ := payload["mupdf_text"].(string)

        // Use default system prompt if not provided
        if systemPrompt == "" {
            systemPrompt = w.conf.SystemPrompt.DefaultPrompt
        }

        // Use REQUEST_TIMEOUT from new Timeouts config (default 120s)
        overallCtx, cancelOverall := context.WithTimeout(context.Background(), w.conf.Timeouts.RequestTimeout)

        attempt := intFromAny(payload["attempt"])
        if attempt <= 0 { attempt = 1 }
        log.Info().Int("worker", id).Str("job_id", jobID).Int("page_id", pageID).Int("attempt", attempt).
            Str("preferred_engine", preferEngine).Str("content_ref", contentRef).Msg("dispatcher picked page")

        // Idempotency: skip provider call if already done
        idemKey, _ := payload["idempotency_key"].(string)
        if done, _ := w.q.IsIdemDone(context.Background(), idemKey); done {
            _ = w.q.Ack(context.Background(), msgID)
            cancelOverall()
            log.Info().Int("worker", id).Str("job_id", jobID).Int("page_id", pageID).Str("idempotency_key", idemKey).Msg("skipping already processed page")
            continue
        }

        ok, provider, model, text, perr := w.processPageWithFailover(overallCtx, jobID, pageID, contentRef, preferEngine, forceFast, imageB64, imageMIME, systemPrompt, contextText, mupdfText)
        source, _ := payload["source"].(string)
        if source == "" { source = "api" }
        if ok {
            url := fmt.Sprintf("http://127.0.0.1:%s/internal/page_done?job_id=%s&page_id=%d", port, jobID, pageID)
            body := map[string]any{"text": text, "provider": provider, "model": model}
            b, _ := json.Marshal(body)
            _, _ = http.Post(url, "application/json", bytes.NewReader(b))
            // mark idempotency done (24h)
            _ = w.q.MarkIdemDone(context.Background(), idemKey, 24*time.Hour)
            _ = w.q.Ack(context.Background(), msgID)
            mpkg.IncProcessed("success")
            mpkg.IncProcessedAttr("success", source, forceFast)
            cancelOverall()
            log.Info().Int("worker", id).Str("job_id", jobID).Int("page_id", pageID).Str("provider", provider).
                Str("model", model).Int("attempt", attempt).Int("text_len", len(text)).Msg("page processed successfully")
        } else {
            // retry with backoff or DLQ
            attempt := intFromAny(payload["attempt"]) 
            if attempt <= 0 { attempt = 1 }
            if attempt >= w.conf.Worker.JobMaxAttempts {
                // push to DLQ
                _ = w.q.AddDLQ(context.Background(), data, "max_attempts")
                _ = w.q.Ack(context.Background(), msgID)
                // Inform orchestrator for MuPDF fallback on final failure
                url := fmt.Sprintf("http://127.0.0.1:%s/internal/page_failed?job_id=%s&page_id=%d", port, jobID, pageID)
                _, _ = http.Post(url, "text/plain", nil)
                mpkg.IncProcessed("dlq")
                mpkg.IncProcessedAttr("dlq", source, forceFast)
                log.Error().Int("worker", id).Str("job_id", jobID).Int("page_id", pageID).Int("attempt", attempt).
                    Err(perr).Msg("page failed max attempts; sent to DLQ")
            } else {
                // requeue delayed with incremented attempt
                payload["attempt"] = attempt + 1
                b, _ := json.Marshal(payload)
                delay := backoffDelay(w.conf.Worker.RetryBaseDelay, w.conf.Worker.RetryBackoffFactor, attempt)
                _ = w.q.EnqueueDelayed(context.Background(), b, time.Now().Add(delay))
                _ = w.q.Ack(context.Background(), msgID)
                mpkg.IncRetry()
                mpkg.IncRetryAttr(source, forceFast)
                log.Warn().Int("worker", id).Str("job_id", jobID).Int("page_id", pageID).Int("attempt", attempt).
                    Dur("retry_in", delay).Err(perr).Msg("page processing failed; scheduled retry")
            }
            cancelOverall()
        }
    }
}

func getenv(k, d string) string { if v := os.Getenv(k); v != "" { return v }; return d }

func intFromAny(v any) int {
    switch t := v.(type) {
    case float64:
        return int(t)
    case int:
        return t
    default:
        return 0
    }
}

func boolFromAny(v any) bool {
    switch t := v.(type) {
    case bool:
        return t
    case string:
        s := strings.ToLower(strings.TrimSpace(t))
        return s == "1" || s == "true" || s == "on" || s == "yes"
    default:
        return false
    }
}

func backoffDelay(base time.Duration, factor float64, attempt int) time.Duration {
    if attempt < 1 { attempt = 1 }
    d := float64(base)
    for i := 1; i < attempt; i++ {
        d *= factor
    }
    max := 5 * time.Minute
    if time.Duration(d) > max { return max }
    return time.Duration(d)
}

