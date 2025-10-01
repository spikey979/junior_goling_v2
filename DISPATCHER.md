# AI Dispatcher - Detaljan Plan Implementacije

## Pregled

AI Dispatcher je servis koji procesira stranice dokumenata koristeći AI vision engines (OpenAI, Anthropic) sa robusnim failover mehanizmima, rate limiting-om i timeout zaštitom.

---

## 1. TIMEOUTS - Dvoslojni Timeout Sistem

### 1.1 Document-Level Timeout (Orchestrator)

**Postavka:** `DOCUMENT_PROCESSING_TIMEOUT=3m` (3 minute)

**Lokacija:** `ProcessJobForAI()` u `ai_pipeline.go`

**Logika:**
```go
func (o *Orchestrator) ProcessJobForAI(ctx context.Context, jobID, filePath, user, password string) error {
    // Create context with document-level timeout
    docCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
    defer cancel()

    // ... download, classify, prepare payloads ...

    // Enqueue all pages to AI
    for _, payload := range payloads {
        // ... enqueue to Redis ...
    }

    // Start background goroutine to monitor completion
    go o.monitorJobCompletion(docCtx, jobID, len(payloads))
}

func (o *Orchestrator) monitorJobCompletion(ctx context.Context, jobID string, totalPages int) {
    ticker := time.NewTicker(1 * time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            // Document timeout reached - finalize with partial results
            log.Warn().
                Str("job_id", jobID).
                Msg("document processing timeout - finalizing with partial results")

            o.finalizeJobWithPartialResults(jobID, totalPages)
            return

        case <-ticker.C:
            // Check if all pages are processed
            st, ok, _ := o.deps.Status.Get(context.Background(), jobID)
            if !ok {
                continue
            }

            pagesProcessed := intFromMeta(st.Metadata, "pages_done") + intFromMeta(st.Metadata, "pages_failed")
            if pagesProcessed >= totalPages {
                // All pages done - normal completion
                o.finalizeJobComplete(jobID, totalPages)
                return
            }
        }
    }
}
```

**Behaviour pri timeout-u:**
1. Prikupi sve AI rezultate koji su stigli (`pages_done`)
2. Za stranice koje nisu procesiran, koristi MuPDF text iz `pageTexts` mape
3. Agregira sve (AI + MuPDF fallback)
4. Označi job kao `success` sa warning metadata:
   ```json
   {
     "status": "success",
     "message": "Completed with partial AI results (timeout)",
     "metadata": {
       "ai_pages_completed": 15,
       "mupdf_fallback_pages": 5,
       "timeout_occurred": true
     }
   }
   ```

---

### 1.2 Request-Level Timeout (Dispatcher Worker)

**Postavka:** `REQUEST_TIMEOUT=60s` (1 minuta per AI API poziv)

**Lokacija:** `internal/dispatcher/worker.go`

**Logika:**
```go
func (w *Worker) callAIWithTimeout(ctx context.Context, provider, model string, request AIRequest) (*AIResponse, error) {
    // Create context with request timeout
    reqCtx, cancel := context.WithTimeout(ctx, w.cfg.RequestTimeout) // 60s
    defer cancel()

    // Call AI provider
    result, err := w.aiClient.Process(reqCtx, provider, model, request)

    if err != nil {
        // Check if it's a timeout
        if errors.Is(err, context.DeadlineExceeded) {
            log.Warn().
                Str("provider", provider).
                Str("model", model).
                Dur("timeout", w.cfg.RequestTimeout).
                Msg("AI request timeout - treating as 429 for failover")

            // Treat timeout as 429 (transient error) to trigger failover
            return nil, &RateLimitError{
                Provider: provider,
                Model:    model,
                Reason:   "timeout",
            }
        }

        return nil, err
    }

    return result, nil
}
```

**Behaviour pri timeout-u:**
- ✅ Tretira se kao **429 (rate limit)**
- ✅ Aktivira **failover na secondary model**
- ✅ Ako i secondary timeout → failover na **secondary provider**
- ✅ Workers **NIKAD NE STOJE** - odmah pokušavaju fallback

---

## 2. FAILOVER LOGIC - Revidirani Flow

### 2.1 Failover Sequence

**STARI (iz plana):**
```
429 → Try secondary model same provider
Still 429/5xx → Try secondary provider
All fail → Retry with backoff (3x max)
```

**NOVI (prilagođen):**
```
429/5xx/TIMEOUT → Try secondary model same provider
Still 429/5xx/TIMEOUT → Try secondary provider (with ITS primary model)
All fail → POST /internal/page_failed (NO RETRY)
```

**Ključna promjena:**
- ❌ Nema retry-a sa backoff-om
- ✅ Direktno označi stranicu kao failed
- ✅ Orchestrator koristi MuPDF fallback

---

### 2.2 Detaljni Failover Algoritam

```go
func (w *Worker) processPageWithFailover(ctx context.Context, payload AIJobPayload) error {
    // Determine primary provider and model
    primaryProvider := payload.AIProvider // "openai" ili "anthropic"
    primaryModel := w.getModelForTier(primaryProvider, payload.ModelTier)

    // Determine secondary provider
    secondaryProvider := w.getSecondaryProvider(primaryProvider)

    // === ATTEMPT 1: Primary Provider, Primary/Secondary/Fast Model ===
    if !w.isCircuitOpen(primaryProvider, primaryModel) {
        log.Info().
            Str("job_id", payload.JobID).
            Int("page", payload.PageID).
            Str("provider", primaryProvider).
            Str("model", primaryModel).
            Msg("attempting AI processing")

        result, err := w.callAIWithTimeout(ctx, primaryProvider, primaryModel, buildRequest(payload))

        if err == nil {
            // SUCCESS
            w.closeCircuitBreaker(primaryProvider, primaryModel)
            w.notifyPageDone(payload.JobID, payload.PageID, result.Text, result.Usage)
            return nil
        }

        // Handle error
        if isRateLimitError(err) || isServerError(err) || isTimeoutError(err) {
            // Open circuit breaker
            w.openCircuitBreaker(primaryProvider, primaryModel, 30*time.Second)

            log.Warn().
                Err(err).
                Str("job_id", payload.JobID).
                Int("page", payload.PageID).
                Str("provider", primaryProvider).
                Str("model", primaryModel).
                Msg("transient error - trying secondary model same provider")
        } else if isFatalError(err) {
            // Fatal error (4xx validation) - no retry
            log.Error().
                Err(err).
                Str("job_id", payload.JobID).
                Int("page", payload.PageID).
                Msg("fatal error - marking page as failed")

            w.notifyPageFailed(payload.JobID, payload.PageID, err)
            return err
        }
    }

    // === ATTEMPT 2: Primary Provider, Secondary Model ===
    secondaryModel := w.getModelForTier(primaryProvider, "secondary")
    if secondaryModel != primaryModel && !w.isCircuitOpen(primaryProvider, secondaryModel) {
        log.Info().
            Str("job_id", payload.JobID).
            Int("page", payload.PageID).
            Str("provider", primaryProvider).
            Str("model", secondaryModel).
            Msg("trying secondary model same provider")

        result, err := w.callAIWithTimeout(ctx, primaryProvider, secondaryModel, buildRequest(payload))

        if err == nil {
            w.closeCircuitBreaker(primaryProvider, secondaryModel)
            w.notifyPageDone(payload.JobID, payload.PageID, result.Text, result.Usage)
            return nil
        }

        if isRateLimitError(err) || isServerError(err) || isTimeoutError(err) {
            w.openCircuitBreaker(primaryProvider, secondaryModel, 30*time.Second)
        }
    }

    // === ATTEMPT 3: Secondary Provider, Primary Model ===
    secondaryProviderModel := w.getModelForTier(secondaryProvider, payload.ModelTier)
    if !w.isCircuitOpen(secondaryProvider, secondaryProviderModel) {
        log.Info().
            Str("job_id", payload.JobID).
            Int("page", payload.PageID).
            Str("provider", secondaryProvider).
            Str("model", secondaryProviderModel).
            Msg("trying secondary provider")

        result, err := w.callAIWithTimeout(ctx, secondaryProvider, secondaryProviderModel, buildRequest(payload))

        if err == nil {
            w.closeCircuitBreaker(secondaryProvider, secondaryProviderModel)
            w.notifyPageDone(payload.JobID, payload.PageID, result.Text, result.Usage)
            return nil
        }

        if isRateLimitError(err) || isServerError(err) || isTimeoutError(err) {
            w.openCircuitBreaker(secondaryProvider, secondaryProviderModel, 30*time.Second)
        }
    }

    // === ALL ATTEMPTS FAILED ===
    log.Error().
        Str("job_id", payload.JobID).
        Int("page", payload.PageID).
        Msg("all AI providers/models failed - marking page as failed")

    w.notifyPageFailed(payload.JobID, payload.PageID, fmt.Errorf("all providers exhausted"))
    return fmt.Errorf("all providers failed")
}
```

---

## 3. CIRCUIT BREAKER - Non-Blocking Cooldown

### 3.1 Redis Keys

**Format:**
```
cb:{provider}:{model} = {"state": "open", "retry_at": 1234567890, "failures": 3}
```

**States:**
- `closed` - normalan rad
- `open` - cooldown period (ne koristi taj model)
- `half_open` - probni request dozvoljen

### 3.2 Behaviour

**Na 429/5xx/timeout:**
```go
func (w *Worker) openCircuitBreaker(provider, model string, cooldown time.Duration) {
    key := fmt.Sprintf("cb:%s:%s", provider, model)

    // Get current failures count
    failures := w.getCircuitFailures(key) + 1

    // Exponential backoff: 30s, 60s, 120s, 240s (max 5 min)
    backoff := min(cooldown * time.Duration(1<<(failures-1)), 5*time.Minute)

    retryAt := time.Now().Add(backoff).Unix()

    data := map[string]interface{}{
        "state":    "open",
        "retry_at": retryAt,
        "failures": failures,
    }

    w.redis.HSet(ctx, key, data)
    w.redis.Expire(ctx, key, 10*time.Minute)

    log.Warn().
        Str("provider", provider).
        Str("model", model).
        Dur("cooldown", backoff).
        Int("failures", failures).
        Msg("circuit breaker OPENED")
}
```

**Provjera:**
```go
func (w *Worker) isCircuitOpen(provider, model string) bool {
    key := fmt.Sprintf("cb:%s:%s", provider, model)

    state, _ := w.redis.HGet(ctx, key, "state")
    if state != "open" {
        return false // Circuit is closed or half-open
    }

    // Check if cooldown period expired
    retryAt, _ := w.redis.HGet(ctx, key, "retry_at")
    retryTimestamp, _ := strconv.ParseInt(retryAt, 10, 64)

    if time.Now().Unix() >= retryTimestamp {
        // Cooldown expired - move to half-open
        w.redis.HSet(ctx, key, "state", "half_open")
        log.Info().
            Str("provider", provider).
            Str("model", model).
            Msg("circuit breaker moved to HALF-OPEN")
        return false
    }

    // Still in cooldown
    return true
}
```

**VAŽNO:** Kada je circuit OPEN, worker **NE ČEKA** - odmah pokušava drugi model/provider!

---

## 4. WORKER POOL - Ne Blokirajući Workers

### 4.1 Concurrency Control

**Per-Model Inflight Limit:**
```
MAX_INFLIGHT_PER_MODEL=2
```

**Implementacija (semaphore pattern):**
```go
type InflightTracker struct {
    mu      sync.Mutex
    inflight map[string]int // key: "provider:model", value: current count
    max     int             // MAX_INFLIGHT_PER_MODEL
}

func (t *InflightTracker) Acquire(provider, model string) bool {
    t.mu.Lock()
    defer t.mu.Unlock()

    key := fmt.Sprintf("%s:%s", provider, model)
    current := t.inflight[key]

    if current >= t.max {
        return false // Cannot acquire - limit reached
    }

    t.inflight[key] = current + 1
    return true
}

func (t *InflightTracker) Release(provider, model string) {
    t.mu.Lock()
    defer t.mu.Unlock()

    key := fmt.Sprintf("%s:%s", provider, model)
    if t.inflight[key] > 0 {
        t.inflight[key]--
    }
}
```

**Ako ne može acquire:**
- ❌ NE čeka
- ✅ Pokušava drugi model ili provider
- ✅ Ako ni jedan nije available → skip i worker uzima sljedeći job iz queue-a

---

### 4.2 Worker Loop (Non-Blocking)

```go
func (w *Worker) Run(ctx context.Context) {
    for {
        select {
        case <-ctx.Done():
            return
        default:
        }

        // Read message from Redis Stream (blocking read, 1s timeout)
        messages, err := w.queue.ReadMessages(ctx, 1*time.Second)
        if err != nil || len(messages) == 0 {
            continue
        }

        for _, msg := range messages {
            // Decode payload
            var payload AIJobPayload
            json.Unmarshal(msg.Data, &payload)

            // Check idempotency
            if w.isAlreadyProcessed(payload.IdempotencyKey) {
                log.Info().
                    Str("job_id", payload.JobID).
                    Int("page", payload.PageID).
                    Msg("skipping already processed page")
                w.queue.Ack(ctx, msg.ID)
                continue
            }

            // Process page with failover (NON-BLOCKING)
            err := w.processPageWithFailover(ctx, payload)

            // Always ACK message (we handle failures via /internal/page_failed)
            w.queue.Ack(ctx, msg.ID)

            // Mark as processed
            w.markProcessed(payload.IdempotencyKey)
        }
    }
}
```

**Ključno:** Worker NIKAD ne blokira! Ako sve opcije su exhausted, označi page kao failed i uzme sljedeći job.

---

## 5. ORCHESTRATOR - Aggregation sa MuPDF Fallback

### 5.1 Page Tracking

**Redis struktura za job status:**
```go
Metadata: {
    "total_pages": 20,
    "pages_done": 15,        // AI uspješno procesirao
    "pages_failed": 5,       // AI failao nakon svih pokušaja
    "ai_pages": 12,          // Stranice sa slikama poslane na AI
    "text_only_pages": 8,    // TEXT_ONLY stranice (samo MuPDF)
}
```

**Tracking po stranicama:**
```
page:{jobID}:{pageNum}:text = "AI extracted text" ili "MuPDF fallback text"
page:{jobID}:{pageNum}:source = "ai" ili "mupdf_fallback"
```

---

### 5.2 Callback Handlers

#### `/internal/page_done` (AI Success)

```go
func (o *Orchestrator) handlePageDone(w http.ResponseWriter, r *http.Request) {
    jobID := r.URL.Query().Get("job_id")
    pageIDStr := r.URL.Query().Get("page_id")
    pageNum, _ := strconv.Atoi(pageIDStr)

    // Parse AI response
    var body struct {
        Text     string `json:"text"`
        Provider string `json:"provider"`
        Model    string `json:"model"`
        Tokens   int    `json:"tokens_used"`
    }
    json.NewDecoder(r.Body).Decode(&body)

    // Save AI text to Redis
    o.deps.Pages.SavePageText(r.Context(), jobID, pageNum, body.Text, "ai", body.Provider, body.Model)

    // Update status
    st, _ := o.deps.Status.Get(r.Context(), jobID)
    st.Metadata["pages_done"] = intFromMeta(st.Metadata, "pages_done") + 1
    st.Progress = calculateProgress(st.Metadata)
    o.deps.Status.Set(r.Context(), jobID, st)

    // Check if all pages done
    o.checkAndFinalizeJob(r.Context(), jobID)
}
```

#### `/internal/page_failed` (AI Failed)

```go
func (o *Orchestrator) handlePageFailed(w http.ResponseWriter, r *http.Request) {
    jobID := r.URL.Query().Get("job_id")
    pageIDStr := r.URL.Query().Get("page_id")
    pageNum, _ := strconv.Atoi(pageIDStr)

    log.Warn().
        Str("job_id", jobID).
        Int("page", pageNum).
        Msg("AI processing failed for page - using MuPDF fallback")

    // Get MuPDF text from initial extraction (stored in job metadata or separate structure)
    mupdfText := o.getMuPDFTextForPage(jobID, pageNum)

    // Save MuPDF fallback text
    o.deps.Pages.SavePageText(r.Context(), jobID, pageNum, mupdfText, "mupdf_fallback", "", "")

    // Update status
    st, _ := o.deps.Status.Get(r.Context(), jobID)
    st.Metadata["pages_failed"] = intFromMeta(st.Metadata, "pages_failed") + 1
    st.Progress = calculateProgress(st.Metadata)
    o.deps.Status.Set(r.Context(), jobID, st)

    log.Info().
        Str("job_id", jobID).
        Int("page", pageNum).
        Int("mupdf_chars", len(mupdfText)).
        Msg("applied MuPDF fallback for failed page")

    // Check if all pages done
    o.checkAndFinalizeJob(r.Context(), jobID)
}
```

---

### 5.3 MuPDF Text Storage

**Problem:** Kako sačuvati MuPDF text za kasnije fallback?

**Rješenje:** Spremiti u Redis PRIJE slanja na AI:

```go
// In ProcessJobForAI() after extractPageTexts()
for pageNum, text := range pageTexts {
    // Store MuPDF text for potential fallback
    key := fmt.Sprintf("job:%s:mupdf:%d", jobID, pageNum)
    o.redis.Set(ctx, key, text, 24*time.Hour) // TTL 24h
}
```

**Retrieve za fallback:**
```go
func (o *Orchestrator) getMuPDFTextForPage(jobID string, pageNum int) string {
    key := fmt.Sprintf("job:%s:mupdf:%d", jobID, pageNum)
    text, err := o.redis.Get(ctx, key)
    if err != nil {
        return "[MuPDF text not available]"
    }
    return text
}
```

---

## 6. SYSTEM PROMPT - Config Integration

### 6.1 Config Structure (`internal/config/env.go`)

```go
// SystemPromptConfig holds AI prompt configuration
type SystemPromptConfig struct {
    DefaultPrompt string
}

// Config top-level
type Config struct {
    Logging      LoggingConfig
    Axiom        AxiomConfig
    Providers    ProvidersConfig
    Worker       WorkerConfig
    Queue        QueueConfig
    Image        ImageOptions
    SystemPrompt SystemPromptConfig  // NEW
}

// In FromEnv()
cfg.SystemPrompt = SystemPromptConfig{
    DefaultPrompt: getEnv("AI_SYSTEM_PROMPT", DefaultSystemPrompt()),
}

func DefaultSystemPrompt() string {
    return `You are an expert document analysis AI with advanced OCR and vision capabilities. Your task is to:

RULES:
- PRIMARY: Extract ALL visible text from the CURRENT PAGE image with maximum accuracy
- Preserve reading order, structure, and layout (multi-column, tables, lists, headers, formulas, symbols)
- Maintain original wording EXACTLY (do not paraphrase or comment)
- Rewrite the extracted text in a **single-column flow**, ignoring any original multi-column or block layout.
- Include figure/image/diagram descriptions in reading order using this format:
[Figure/Image/Table Name or Number]
[Objective technical description] - Use CONTEXT (text from surrounding pages) ONLY as supporting reference to better describe non-text elements (figures, charts, diagrams, tables) or resolve unclear symbols — never to change or override actual text from the current page
- Exclude page headers/footers, watermarks, or irrelevant artifacts
- Do NOT add introductions, explanations, or meta-commentary
- Output must start immediately with the first line of document text

GOAL:
Return the document content in a structured, accurate, and comprehensive way that preserves the original meaning and organization, while using CONTEXT to enhance understanding of graphical or ambiguous content.`
}
```

---

## 7. MODEL TIER SELECTION - Explicit Request Support

### 7.1 Request Schema Extension

```go
type processReq struct {
    FilePath   string                 `json:"file_path"`
    FileURL    string                 `json:"file_url"`
    UserName   string                 `json:"user_name"`
    UserID     string                 `json:"user_id"`
    Password   string                 `json:"password"`
    AIPrompt   string                 `json:"ai_prompt"`
    AIEngine   string                 `json:"ai_engine"`
    ModelTier  string                 `json:"model_tier"`   // NEW: "primary"|"secondary"|"fast"
    TextOnly   bool                   `json:"text_only"`
    FastUpload bool                   `json:"fast_upload"`
    Options    map[string]interface{} `json:"options"`
    Source     string                 `json:"source"`
}
```

### 7.2 Model Selection Logic

```go
func (w *Worker) getModelForTier(provider, tier string) string {
    switch provider {
    case "openai":
        switch tier {
        case "primary":
            return w.cfg.Providers.OpenAI.Primary // "gpt-4.1"
        case "secondary":
            return w.cfg.Providers.OpenAI.Secondary // "gpt-4o"
        case "fast":
            return w.cfg.Providers.OpenAI.Fast // "gpt-4.1-mini"
        default:
            return w.cfg.Providers.OpenAI.Primary
        }
    case "anthropic":
        switch tier {
        case "primary":
            return w.cfg.Providers.Anthropic.Primary // "claude-3-5-sonnet-20241022"
        case "secondary":
            return w.cfg.Providers.Anthropic.Secondary // "claude-3-opus"
        case "fast":
            return w.cfg.Providers.Anthropic.Fast // "claude-3-haiku"
        default:
            return w.cfg.Providers.Anthropic.Primary
        }
    }
    return ""
}

func (w *Worker) getSecondaryProvider(primaryProvider string) string {
    if primaryProvider == "openai" {
        return "anthropic"
    }
    return "openai"
}
```

---

## 8. ENV VARIABLES - Kompletna Lista

### Timeouts
```bash
DOCUMENT_PROCESSING_TIMEOUT=3m      # Max vrijeme za cijeli dokument
REQUEST_TIMEOUT=60s                  # Max vrijeme per AI API poziv
OPENAI_TIMEOUT=60s                   # Override za OpenAI
ANTHROPIC_TIMEOUT=60s                # Override za Anthropic
```

### Circuit Breaker
```bash
BREAKER_BASE_BACKOFF=30s            # Početni cooldown
BREAKER_MAX_BACKOFF=5m              # Maksimalni cooldown
```

### Concurrency
```bash
WORKER_CONCURRENCY=8                # Broj worker goroutina
MAX_INFLIGHT_PER_MODEL=2           # Max paralelnih requestova po modelu
```

### Image Rendering
```bash
IMAGE_SEND_ALL_PAGES=false         # false = samo HAS_GRAPHICS
IMAGE_DPI=100
IMAGE_COLOR=rgb                     # "rgb" ili "gray"
IMAGE_FORMAT=jpeg
IMAGE_JPEG_QUALITY=70              # 1-100
IMAGE_CONTEXT_RADIUS=1             # ±N stranica
IMAGE_MAX_CONTEXT_BYTES=3000       # Max 3KB konteksta
IMAGE_INCLUDE_BASE64=true
```

### Model Configuration
```bash
# Primary/Secondary Engines
PRIMARY_ENGINE=openai
SECONDARY_ENGINE=anthropic

# OpenAI Models
OPENAI_PRIMARY_MODEL=gpt-4.1
OPENAI_SECONDARY_MODEL=gpt-4o
OPENAI_FAST_MODEL=gpt-4.1-mini

# Anthropic Models
ANTHROPIC_PRIMARY_MODEL=claude-3-5-sonnet-20241022
ANTHROPIC_SECONDARY_MODEL=claude-3-opus
ANTHROPIC_FAST_MODEL=claude-3-haiku
```

### System Prompt
```bash
AI_SYSTEM_PROMPT="<custom prompt>"  # Override default
```

---

## 9. PAYLOAD SCHEMA - Final Format

### AIJobPayload (Redis Queue)

```json
{
  "job_id": "uuid",
  "document_id": "file_id_without_original",
  "page_id": 3,

  "image_b64": "base64_encoded_jpeg...",
  "image_mime": "image/jpeg",
  "width_px": 850,
  "height_px": 1100,

  "mupdf_text": "Text extracted from page 3...",
  "context_text": "=== Page 2 (context) ===\n...\n=== Page 3 (current) ===\n...",

  "system_prompt": "You are an expert...",
  "ai_provider": "openai",
  "model_tier": "primary",

  "classification": "HAS_GRAPHICS",
  "idempotency_key": "doc:uuid:page:3",
  "attempt": 1,

  "user": "user@example.com",
  "source": "api"
}
```

**Veličina payloada:**
- Bez slike (TEXT_ONLY): ~5-10 KB
- Sa slikom (HAS_GRAPHICS): ~70-200 KB (base64 + context)

---

## 10. AGGREGATION - Finalizacija Joba

### 10.1 Provjera Completion

```go
func (o *Orchestrator) checkAndFinalizeJob(ctx context.Context, jobID string) {
    st, ok, _ := o.deps.Status.Get(ctx, jobID)
    if !ok {
        return
    }

    totalPages := intFromMeta(st.Metadata, "total_pages")
    pagesProcessed := intFromMeta(st.Metadata, "pages_done") + intFromMeta(st.Metadata, "pages_failed")

    if pagesProcessed >= totalPages {
        // All pages accounted for - finalize
        o.finalizeJobComplete(ctx, jobID, totalPages)
    }
}
```

### 10.2 Finalizacija

```go
func (o *Orchestrator) finalizeJobComplete(ctx context.Context, jobID string, totalPages int) {
    log.Info().Str("job_id", jobID).Int("total_pages", totalPages).Msg("finalizing job")

    // Aggregate all page texts (AI + MuPDF fallback)
    var allText strings.Builder

    for i := 1; i <= totalPages; i++ {
        pageText, source := o.deps.Pages.GetPageText(ctx, jobID, i)

        if i > 1 {
            allText.WriteString("\n\n")
        }
        allText.WriteString(fmt.Sprintf("=== Page %d ===\n", i))
        allText.WriteString(pageText)

        if source == "mupdf_fallback" {
            log.Debug().
                Str("job_id", jobID).
                Int("page", i).
                Msg("using MuPDF fallback text")
        }
    }

    aggregatedText := allText.String()

    // Save to S3 (or local for upload jobs)
    st, _ := o.deps.Status.Get(ctx, jobID)

    if src, _ := st.Metadata["source"].(string); src == "upload" {
        // Save locally
        localPath, _ := SaveAggregatedTextToLocal(ctx, jobID, aggregatedText)
        st.Metadata["result_local_path"] = localPath
    } else {
        // Save to S3
        filePath, _ := st.Metadata["file_path"].(string)
        password, _ := st.Metadata["password"].(string)
        s3URL, _ := SaveAggregatedTextToS3(ctx, filePath, jobID, aggregatedText, password)
        st.Metadata["result_s3_url"] = s3URL
    }

    // Mark as success
    endTime := time.Now()
    st.Status = "success"
    st.Progress = 100
    st.End = &endTime
    st.Metadata["result_text_len"] = len(aggregatedText)
    st.Metadata["ai_pages_succeeded"] = intFromMeta(st.Metadata, "pages_done")
    st.Metadata["mupdf_fallback_count"] = intFromMeta(st.Metadata, "pages_failed")

    o.deps.Status.Set(ctx, jobID, st)

    log.Info().
        Str("job_id", jobID).
        Int("total_chars", len(aggregatedText)).
        Int("ai_pages", intFromMeta(st.Metadata, "pages_done")).
        Int("fallback_pages", intFromMeta(st.Metadata, "pages_failed")).
        Msg("job finalized successfully")
}
```

---

## 11. TIMEOUT HANDLING - Document Level

### 11.1 Partial Results na Timeout

```go
func (o *Orchestrator) finalizeJobWithPartialResults(ctx context.Context, jobID string, totalPages int) {
    log.Warn().
        Str("job_id", jobID).
        Msg("document timeout - finalizing with partial AI results")

    st, _ := o.deps.Status.Get(ctx, jobID)

    pagesProcessed := intFromMeta(st.Metadata, "pages_done") + intFromMeta(st.Metadata, "pages_failed")
    missingPages := totalPages - pagesProcessed

    // For missing pages, use MuPDF fallback
    for i := 1; i <= totalPages; i++ {
        // Check if page already processed
        _, exists := o.deps.Pages.GetPageText(ctx, jobID, i)
        if !exists {
            // Use MuPDF fallback
            mupdfText := o.getMuPDFTextForPage(jobID, i)
            o.deps.Pages.SavePageText(ctx, jobID, i, mupdfText, "mupdf_timeout_fallback", "", "")

            log.Debug().
                Str("job_id", jobID).
                Int("page", i).
                Msg("timeout - using MuPDF fallback")
        }
    }

    // Aggregate and save (same as normal finalization)
    // ... (isti kod kao u finalizeJobComplete) ...

    // Mark as success with warning
    st.Status = "success"
    st.Message = "Completed with partial AI results (timeout)"
    st.Metadata["timeout_occurred"] = true
    st.Metadata["missing_pages"] = missingPages

    o.deps.Status.Set(ctx, jobID, st)
}
```

---

## 12. IMPLEMENTATION CHECKLIST

### Faza 1: Config & Types (ova session)
- [x] Dodati `SystemPromptConfig` i `DefaultSystemPrompt()` u `config/env.go`
- [x] Dodati `DOCUMENT_PROCESSING_TIMEOUT` u config
- [x] Kreirati `AIJobPayload` tip u `orchestrator/types.go`
- [x] Dodati `ModelTier` polje u `processReq`
- [x] Helper funkcije: `getSystemPrompt()`, `mapEngineToProvider()`, `determineModelTier()`

### Faza 2: Enqueuing Logic
- [ ] Refaktorirati `ProcessJobForAI()` da sprema MuPDF text u Redis
- [ ] Dodati enqueuing loop za kreiranje `AIJobPayload` i slanje u Redis
- [ ] Implementirati `monitorJobCompletion()` sa document timeout
- [ ] Dodati `finalizeJobWithPartialResults()` i `finalizeJobComplete()`

### Faza 3: Dispatcher Worker
- [ ] Refaktorirati `worker.go` da prima `AIJobPayload`
- [ ] Implementirati `processPageWithFailover()` sa novom logikom
- [ ] Implementirati `InflightTracker` (per-model semaphore)
- [ ] Circuit breaker (Redis-based) sa exponential backoff
- [ ] Helper: `isCircuitOpen()`, `openCircuitBreaker()`, `closeCircuitBreaker()`

### Faza 4: AI Client Integration
- [ ] OpenAI vision API integration (base64 image + system prompt + context)
- [ ] Anthropic vision API integration
- [ ] Error classification (`isRateLimitError`, `isServerError`, `isFatalError`, `isTimeoutError`)
- [ ] Usage tracking (tokens used)

### Faza 5: Orchestrator Callbacks
- [ ] Refaktorirati `handlePageDone()` za AI results
- [ ] Refaktorirati `handlePageFailed()` za MuPDF fallback
- [ ] Implementirati `getMuPDFTextForPage()` (Redis lookup)
- [ ] Implementirati `checkAndFinalizeJob()` (completion check)

### Faza 6: Testing & Metrics
- [ ] E2E test sa混合 TEXT_ONLY + HAS_GRAPHICS stranicama
- [ ] Test timeout scenarija (document i request level)
- [ ] Test failover-a (simulate 429/5xx)
- [ ] Prometheus metrics (success rate, latency, failovers, timeouts)

---

## 13. KEY BEHAVIOURS - Summary

### ✅ Document Timeout (3 min)
- Ako nisu sve stranice procesiran → finalizira sa partial results
- Nedostajuće stranice → MuPDF fallback
- Job status: `success` sa `timeout_occurred=true` metadata

### ✅ Request Timeout (60s)
- Tretira se kao 429 (transient)
- Aktivira failover na secondary model/provider
- Workers NE ČEKAJU

### ✅ Circuit Breaker Cooldown
- Na 429/5xx → OPEN (30s-5min cooldown)
- Worker odmah pokušava drugi model
- **Workers NIKAD NE STOJE**

### ✅ Failover Sequence
1. Primary provider, requested tier model
2. Primary provider, secondary model
3. Secondary provider, same tier model
4. **NO MORE RETRIES** → `/internal/page_failed`

### ✅ MuPDF Fallback
- AI failed page → orchestrator koristi MuPDF text
- Spremljeno u Redis pri početku processinga
- Finalni agregat: AI + MuPDF mixed (seamless)

### ✅ No Disk Storage
- Slike u RAM-u (ImageBytes, ImageBase64)
- Samo PDF fajlovi na disk (sa cleanup-om)
- GC automatski čisti memoriju

---

**DA LI JE PLAN KOMPLETAN I JASAN?**
