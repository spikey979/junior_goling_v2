package dispatcher

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "net/http"
    "os"
    "time"

    "github.com/local/aidispatcher/internal/ai"
    "github.com/local/aidispatcher/internal/limiter"
    mpkg "github.com/local/aidispatcher/internal/metrics"
    cfgpkg "github.com/local/aidispatcher/internal/config"
    "github.com/rs/zerolog/log"
    "strings"
    "bytes"
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
    cfg   Config
    q     Queue
    stop  chan struct{}
    conf  cfgpkg.Config
    openai ai.Client
    anthropic ai.Client
    lim   *limiter.Adaptive
}

func New(cfg Config, q Queue) *Worker {
    if cfg.Concurrency <= 0 { cfg.Concurrency = 2 }
    conf := cfgpkg.FromEnv()
    lim, _ := limiter.New(limiter.Options{RedisURL: conf.Queue.RedisURL, MaxInflight: conf.Worker.MaxInflightPerModel, BaseBackoff: conf.Worker.BreakerBaseBackoff, MaxBackoff: conf.Worker.BreakerMaxBackoff})
    return &Worker{cfg: cfg, q: q, stop: make(chan struct{}), conf: conf, openai: ai.NewOpenAIClient(), anthropic: ai.NewAnthropicClient(), lim: lim}
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
                log.Warn().Int("worker", id).Str("job_id", jobID).Msg("job cancelled before processing; skipping")
                continue
            }
        }
        contentRef, _ := payload["content_ref"].(string)
        preferEngine, _ := payload["ai_engine"].(string)
        forceFast := boolFromAny(payload["force_fast"]) 

        overallCtx, cancelOverall := context.WithTimeout(context.Background(), w.conf.Worker.PageTotalTimeout)
        defer cancelOverall()

        // Idempotency: skip provider call if already done
        idemKey, _ := payload["idempotency_key"].(string)
        if done, _ := w.q.IsIdemDone(context.Background(), idemKey); done {
            _ = w.q.Ack(context.Background(), msgID)
            continue
        }

        ok, provider, model, text := w.processPage(overallCtx, jobID, pageID, contentRef, preferEngine, forceFast)
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
            } else {
                // requeue delayed with incremented attempt
                payload["attempt"] = attempt + 1
                b, _ := json.Marshal(payload)
                delay := backoffDelay(w.conf.Worker.RetryBaseDelay, w.conf.Worker.RetryBackoffFactor, attempt)
                _ = w.q.EnqueueDelayed(context.Background(), b, time.Now().Add(delay))
                _ = w.q.Ack(context.Background(), msgID)
                mpkg.IncRetry()
                mpkg.IncRetryAttr(source, forceFast)
            }
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

func (w *Worker) processPage(ctx context.Context, jobID string, pageID int, contentRef, preferEngine string, forceFast bool) (bool, string, string, string) {
    // Determine providers and models from config
    primaryProv := w.conf.Providers.PrimaryEngine
    secondaryProv := w.conf.Providers.SecondaryEngine
    if preferEngine != "" { primaryProv = strings.ToLower(preferEngine) }

    // Helper to call provider/model with per-request timeout + metrics
    call := func(provider, model string) (ai.Response, error) {
        timeout := w.conf.Worker.RequestTimeout
        if provider == "openai" { timeout = w.conf.Worker.OpenAITimeout }
        if provider == "anthropic" { timeout = w.conf.Worker.AnthropicTimeout }
        if timeout <= 0 { timeout = w.conf.Worker.RequestTimeout }

        req := ai.Request{JobID: jobID, PageID: pageID, ContentRef: contentRef, Model: model, Timeout: timeout}
        cctx, cancel := context.WithTimeout(ctx, timeout)
        defer cancel()
        var client ai.Client
        switch provider {
        case "openai": client = w.openai
        case "anthropic": client = w.anthropic
        default: client = w.openai
        }
        start := time.Now()
        resp, err := client.Do(cctx, req)
        dur := time.Since(start)
        if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
            mpkg.ObserveProvider(provider, model, "timeout", dur)
            return ai.Response{}, context.DeadlineExceeded
        }
        // classify
        result := "success"
        if err != nil {
            if ai.IsRateLimited(err) { result = "rate_limited" } else if isTransient(err) { result = "transient" } else { result = "fatal" }
        }
        mpkg.ObserveProvider(provider, model, result, dur)
        return resp, err
    }

    // Fast-path: only fast models if requested
    if forceFast {
        fModel := w.fastModel(primaryProv)
        if fModel != "" && !w.lim.IsOpen(ctx, primaryProv, fModel) {
            relF, okF := w.lim.Allow(primaryProv, fModel)
            if okF {
                if resp, err := call(primaryProv, fModel); err == nil { relF(); w.lim.Close(ctx, primaryProv, fModel); return true, primaryProv, fModel, resp.Text }
                relF()
            }
        }
        sfModel := w.fastModel(secondaryProv)
        if sfModel != "" && !w.lim.IsOpen(ctx, secondaryProv, sfModel) {
            relSF, okSF := w.lim.Allow(secondaryProv, sfModel)
            if okSF {
                if resp2, err2 := call(secondaryProv, sfModel); err2 == nil { relSF(); w.lim.Close(ctx, secondaryProv, sfModel); return true, secondaryProv, sfModel, resp2.Text }
                relSF()
            }
        }
        return false, "", "", ""
    }

    // Try primary provider primary model
    var err error
    var resp ai.Response
    pModel := w.primaryModel(primaryProv)
    sModel := w.secondaryModel(primaryProv)

    if !w.lim.IsOpen(ctx, primaryProv, pModel) {
        rel, ok := w.lim.Allow(primaryProv, pModel)
        if ok {
            resp, err = call(primaryProv, pModel)
            rel()
            if err == nil { w.lim.Close(ctx, primaryProv, pModel); mpkg.BreakerClosed(primaryProv, pModel); return true, primaryProv, pModel, resp.Text }
            if ai.IsRateLimited(err) || isTransient(err) { w.lim.Open(ctx, primaryProv, pModel); mpkg.BreakerOpened(primaryProv, pModel) }
        }
    }

    // On 429 or transient → try secondary model of same provider
    if (ai.IsRateLimited(err) || isTransient(err)) && sModel != "" {
        if !w.lim.IsOpen(ctx, primaryProv, sModel) {
            rel2, ok2 := w.lim.Allow(primaryProv, sModel)
            if ok2 {
                if resp2, err2 := call(primaryProv, sModel); err2 == nil { rel2(); w.lim.Close(ctx, primaryProv, sModel); mpkg.BreakerClosed(primaryProv, sModel); return true, primaryProv, sModel, resp2.Text }
                rel2()
            }
        }
    }

    // Try secondary provider primary model (and its secondary on transient/429)
    spModel := w.primaryModel(secondaryProv)
    if !w.lim.IsOpen(ctx, secondaryProv, spModel) {
        rel3, ok3 := w.lim.Allow(secondaryProv, spModel)
        if ok3 {
            if resp3, err2 := call(secondaryProv, spModel); err2 == nil { rel3(); w.lim.Close(ctx, secondaryProv, spModel); mpkg.BreakerClosed(secondaryProv, spModel); return true, secondaryProv, spModel, resp3.Text }
            rel3()
            if ai.IsRateLimited(err) || isTransient(err) {
                ssModel := w.secondaryModel(secondaryProv)
                if ssModel != "" && !w.lim.IsOpen(ctx, secondaryProv, ssModel) {
                    rel4, ok4 := w.lim.Allow(secondaryProv, ssModel)
                    if ok4 {
                        if resp4, err4 := call(secondaryProv, ssModel); err4 == nil { rel4(); w.lim.Close(ctx, secondaryProv, ssModel); mpkg.BreakerClosed(secondaryProv, ssModel); return true, secondaryProv, ssModel, resp4.Text }
                        rel4()
                    }
                }
            }
        }
    }
    return false, "", "", ""
}

func (w *Worker) primaryModel(provider string) string {
    switch provider {
    case "openai": return w.conf.Providers.OpenAI.Primary
    case "anthropic": return w.conf.Providers.Anthropic.Primary
    default: return w.conf.Providers.OpenAI.Primary
    }
}

func (w *Worker) secondaryModel(provider string) string {
    switch provider {
    case "openai": return w.conf.Providers.OpenAI.Secondary
    case "anthropic": return w.conf.Providers.Anthropic.Secondary
    default: return ""
    }
}

func (w *Worker) fastModel(provider string) string {
    switch provider {
    case "openai": return w.conf.Providers.OpenAI.Fast
    case "anthropic": return w.conf.Providers.Anthropic.Fast
    default: return ""
    }
}

// isTransient: timeouts and 5xx treated as transient; 4xx (except 429) as fatal.
func isTransient(err error) bool {
    if err == nil { return false }
    if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) { return true }
    es := err.Error()
    // quick parse of status code from provider error strings
    if strings.Contains(es, "status ") {
        // treat any 5xx as transient
        for code := 500; code <= 599; code++ {
            if strings.Contains(es, fmt.Sprintf("%d", code)) { return true }
        }
        // treat 4xx (except 429) as fatal
        for code := 400; code <= 499; code++ {
            if code == 429 { continue }
            if strings.Contains(es, fmt.Sprintf("%d", code)) { return false }
        }
    }
    // default safe side: transient
    return true
}
