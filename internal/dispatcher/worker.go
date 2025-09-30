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
    cfgpkg "github.com/local/aidispatcher/internal/config"
    "github.com/rs/zerolog/log"
    "strings"
    "bytes"
)

type Queue interface {
    DequeueAI(ctx context.Context, timeout time.Duration) ([]byte, error)
    IsCancelled(ctx context.Context, jobID string) (bool, error)
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
}

func New(cfg Config, q Queue) *Worker {
    if cfg.Concurrency <= 0 { cfg.Concurrency = 2 }
    return &Worker{cfg: cfg, q: q, stop: make(chan struct{}), conf: cfgpkg.FromEnv(), openai: ai.NewOpenAIClient(), anthropic: ai.NewAnthropicClient()}
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

        data, err := w.q.DequeueAI(context.Background(), 2*time.Second)
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

        overallCtx, cancelOverall := context.WithTimeout(context.Background(), w.conf.Worker.PageTotalTimeout)
        defer cancelOverall()

    ok, provider, model, text := w.processPage(overallCtx, jobID, pageID, contentRef, preferEngine)
    if ok {
        url := fmt.Sprintf("http://127.0.0.1:%s/internal/page_done?job_id=%s&page_id=%d", port, jobID, pageID)
        body := map[string]any{"text": text, "provider": provider, "model": model}
        b, _ := json.Marshal(body)
        _, _ = http.Post(url, "application/json", bytes.NewReader(b))
    } else {
        url := fmt.Sprintf("http://127.0.0.1:%s/internal/page_failed?job_id=%s&page_id=%d", port, jobID, pageID)
        _, _ = http.Post(url, "text/plain", nil)
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

func (w *Worker) processPage(ctx context.Context, jobID string, pageID int, contentRef, preferEngine string) (bool, string, string, string) {
    // Determine providers and models from config
    primaryProv := w.conf.Providers.PrimaryEngine
    secondaryProv := w.conf.Providers.SecondaryEngine
    if preferEngine != "" { primaryProv = strings.ToLower(preferEngine) }

    // Helper to call provider/model with per-request timeout
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
        resp, err := client.Do(cctx, req)
        if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
            return ai.Response{}, context.DeadlineExceeded
        }
        return resp, err
    }

    // Try primary provider primary model
    var err error
    var resp ai.Response
    pModel := w.primaryModel(primaryProv)
    sModel := w.secondaryModel(primaryProv)

    resp, err = call(primaryProv, pModel)
    if err == nil { return true, primaryProv, pModel, resp.Text }

    // If 429 → try secondary model of same provider immediately
    if ai.IsRateLimited(err) {
        if sModel != "" {
            if resp2, err2 := call(primaryProv, sModel); err2 == nil { return true, primaryProv, sModel, resp2.Text }
            // any error here → move to secondary provider
        }
    }

    // For any non-429 error on primary, skip within-provider secondary and go to secondary provider
    // Try secondary provider primary model
    spModel := w.primaryModel(secondaryProv)
    if resp3, err2 := call(secondaryProv, spModel); err2 == nil { return true, secondaryProv, spModel, resp3.Text }
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
