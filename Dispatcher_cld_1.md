# AI Dispatcher – Finalni Plan Integracije (CLD v1)

## Pregled

AI Dispatcher je worker servis koji procesira stranice dokumenata kroz AI vision engines (OpenAI, Anthropic) sa robusnim failover mehanizmima, timeout zaštitom i deterministički garantovanim završetkom obrade svakog dokumenta.

---

## 1. TIMEOUT SISTEM – Dvoslojni SLA

### 1.1 Request-Level Timeout

**Postavka:** `REQUEST_TIMEOUT=120s` (2 minute po AI API pozivu)

**Lokacija:** `internal/dispatcher/worker.go` – `callAIWithTimeout()`

**Ponašanje:**
- Svaki AI poziv (OpenAI/Anthropic) dobiva `context.WithTimeout(120s)`
- Na `context.DeadlineExceeded` → tretira se kao **429 (transient error)**
- Aktivira **failover** na secondary model/provider
- Worker **NIKAD NE ČEKA** – odmah prelazi na alternativu

**Implementacija:**
```go
func (w *Worker) callAIWithTimeout(ctx context.Context, provider, model string, request AIRequest) (*AIResponse, error) {
    // Request timeout iz konfiga (default: 120s)
    reqCtx, cancel := context.WithTimeout(ctx, w.cfg.RequestTimeout)
    defer cancel()

    result, err := w.aiClient.Process(reqCtx, provider, model, request)

    if err != nil {
        if errors.Is(err, context.DeadlineExceeded) {
            log.Warn().
                Str("provider", provider).
                Str("model", model).
                Dur("timeout", w.cfg.RequestTimeout).
                Msg("AI request timeout - triggering failover")

            // Tretirati kao rate limit za failover aktivaciju
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

---

### 1.2 Job-Level Timeout (Document SLA)

**Postavka:** `JOB_TIMEOUT=5m` (5 minuta za cijeli dokument)

**Lokacija:** `internal/orchestrator/ai_pipeline.go` – `ProcessJobForAI()`

**Ponašanje:**
- Orchestrator kreira context sa 5-minutnim timeoutom pri startu obrade dokumenta
- Pokreće `monitorJobCompletion()` goroutinu koja prati:
  - Da li su sve stranice obrađene (success path)
  - Da li je istekao job timeout (SLA enforcement)
- Na timeout:
  1. **Prekida sve aktivne worker pokušaje** kroz `CancelJob(jobID)` u Redis
  2. **Prikuplja sve uspješne AI rezultate** koji su stigli
  3. **Dopunjava nedostajuće stranice MuPDF tekstom** (fallback)
  4. **Agregira finalni rezultat** (AI + MuPDF mix)
  5. **Označava job kao `success`** sa metadata `timeout_occurred=true`

**Implementacija:**
```go
func (o *Orchestrator) ProcessJobForAI(ctx context.Context, jobID, filePath, user, password string) error {
    // Job-level timeout context (5 minuta)
    jobCtx, cancel := context.WithTimeout(ctx, o.cfg.JobTimeout)
    defer cancel()

    // ... download, classify, prepare payloads ...

    // Enqueue sve AI stranice u Redis
    for _, payload := range aiPayloads {
        o.deps.Queue.Enqueue(jobCtx, payload)
    }

    // Pokrenuti monitor za job completion
    go o.monitorJobCompletion(jobCtx, jobID, totalPages, aiPageCount)

    return nil
}
```

**Monitor implementacija:**
```go
func (o *Orchestrator) monitorJobCompletion(ctx context.Context, jobID string, totalPages, aiPages int) {
    ticker := time.NewTicker(2 * time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            // Job timeout dostignut - finalizacija sa parcijalnim rezultatima
            log.Warn().
                Str("job_id", jobID).
                Dur("timeout", o.cfg.JobTimeout).
                Msg("job timeout reached - finalizing with partial results")

            // 1. Cancel job u Redis da workeri prestanu obrađivati
            o.deps.Queue.CancelJob(context.Background(), jobID)

            // 2. Finalizacija sa MuPDF fallback za nedostajuće stranice
            o.finalizeJobWithPartialResults(context.Background(), jobID, totalPages)
            return

        case <-ticker.C:
            // Provjera da li su sve stranice obrađene
            st, ok, _ := o.deps.Status.Get(context.Background(), jobID)
            if !ok {
                continue
            }

            // Provjeriti status otkazivanja
            if st.Status == "cancelled" {
                log.Info().Str("job_id", jobID).Msg("job cancelled externally")
                return
            }

            // Broj obrađenih stranica (uspješno + failed)
            pagesDone := intFromMeta(st.Metadata, "pages_done")
            pagesFailed := intFromMeta(st.Metadata, "pages_failed")
            pagesProcessed := pagesDone + pagesFailed

            if pagesProcessed >= aiPages {
                // Sve AI stranice obrađene - normalan završetak
                log.Info().
                    Str("job_id", jobID).
                    Int("ai_pages", aiPages).
                    Int("pages_done", pagesDone).
                    Int("pages_failed", pagesFailed).
                    Msg("all AI pages processed - finalizing job")

                o.finalizeJobComplete(context.Background(), jobID, totalPages)
                return
            }
        }
    }
}
```

---

## 2. KONFIGURACIJA – ENV Variables

### 2.1 Config Structure (`internal/config/env.go`)

```go
// TimeoutConfig drži sve timeout postavke
type TimeoutConfig struct {
    RequestTimeout time.Duration // Max vrijeme po AI API pozivu
    JobTimeout     time.Duration // Max vrijeme za cijeli dokument
}

// Config - top level struktura
type Config struct {
    Logging      LoggingConfig
    Axiom        AxiomConfig
    Providers    ProvidersConfig
    Worker       WorkerConfig
    Queue        QueueConfig
    Image        ImageOptions
    SystemPrompt SystemPromptConfig
    Timeouts     TimeoutConfig      // NOVO
}

// FromEnv() - parsiranje iz environment varijabli
func FromEnv() *Config {
    cfg := &Config{}

    // ... existing config ...

    // Timeout konfiguracija
    cfg.Timeouts = TimeoutConfig{
        RequestTimeout: parseDuration(getEnv("REQUEST_TIMEOUT", "120s")),
        JobTimeout:     parseDuration(getEnv("JOB_TIMEOUT", "5m")),
    }

    return cfg
}

// Helper za parsiranje duration stringa
func parseDuration(s string) time.Duration {
    d, err := time.ParseDuration(s)
    if err != nil {
        log.Warn().
            Str("input", s).
            Err(err).
            Msg("invalid duration format - using default")
        return 0
    }
    return d
}
```

### 2.2 Environment Variables

```bash
# Timeout postavke
REQUEST_TIMEOUT=120s      # Max trajanje jednog AI API poziva (default: 120s)
JOB_TIMEOUT=5m            # Max trajanje cijelog dokumenta (default: 5m)

# Legacy varijable (zadržane za kompatibilnost, ali mapirane na nove)
PAGE_TOTAL_TIMEOUT=120s   # (deprecated, koristi se REQUEST_TIMEOUT)
DOCUMENT_PROCESSING_TIMEOUT=5m  # (deprecated, koristi se JOB_TIMEOUT)
```

**Backwards Compatibility:**
```go
func FromEnv() *Config {
    // ...

    // Request timeout - prioritet: REQUEST_TIMEOUT > PAGE_TOTAL_TIMEOUT > default
    requestTimeout := getEnv("REQUEST_TIMEOUT", "")
    if requestTimeout == "" {
        requestTimeout = getEnv("PAGE_TOTAL_TIMEOUT", "120s")
    }

    // Job timeout - prioritet: JOB_TIMEOUT > DOCUMENT_PROCESSING_TIMEOUT > default
    jobTimeout := getEnv("JOB_TIMEOUT", "")
    if jobTimeout == "" {
        jobTimeout = getEnv("DOCUMENT_PROCESSING_TIMEOUT", "5m")
    }

    cfg.Timeouts = TimeoutConfig{
        RequestTimeout: parseDuration(requestTimeout),
        JobTimeout:     parseDuration(jobTimeout),
    }

    // ...
}
```

---

## 3. FAILOVER LOGIC – Revidirani Flow

### 3.1 Failover Sekvenca

```
Request Attempt 1: Primary Provider + Primary/Fast/Secondary Model
    ↓ (429/5xx/TIMEOUT)
Request Attempt 2: Primary Provider + Secondary Model (ako nije već pokušan)
    ↓ (429/5xx/TIMEOUT)
Request Attempt 3: Secondary Provider + Primary/Fast Model
    ↓ (429/5xx/TIMEOUT)
Request Attempt 4: Secondary Provider + Secondary Model
    ↓ (ALL FAILED)
POST /internal/page_failed → MuPDF Fallback
```

**Ključne karakteristike:**
- ❌ **Nema odgođenih retry-a** (ZSET delayed queue se ne koristi za ove greške)
- ✅ **Inline failover** – sve pokušaje u istoj worker iteraciji
- ✅ **Svaki pokušaj ima svoj REQUEST_TIMEOUT (120s)**
- ✅ **Circuit breaker provjera prije svakog pokušaja**
- ✅ **Tretiranje timeout-a kao transient error** → failover

---

### 3.2 Detaljni Worker Flow

```go
func (w *Worker) processPageWithFailover(ctx context.Context, payload AIJobPayload) error {
    // Check if job is cancelled
    if w.isJobCancelled(payload.JobID) {
        log.Info().
            Str("job_id", payload.JobID).
            Int("page", payload.PageID).
            Msg("skipping page - job cancelled")
        return fmt.Errorf("job cancelled")
    }

    // Idempotency check
    if w.isAlreadyProcessed(payload.IdempotencyKey) {
        log.Debug().
            Str("job_id", payload.JobID).
            Int("page", payload.PageID).
            Msg("page already processed - skipping")
        return nil
    }

    // Determine providers and models
    primaryProvider := payload.AIProvider  // iz payload-a (openai/anthropic)
    primaryModel := w.getModelForTier(primaryProvider, payload.ModelTier)

    secondaryProvider := w.getSecondaryProvider(primaryProvider)
    secondaryProviderModel := w.getModelForTier(secondaryProvider, payload.ModelTier)

    // Build AI request
    request := w.buildAIRequest(payload)

    // === ATTEMPT 1: Primary Provider, Primary/Fast Model ===
    if !w.isCircuitOpen(primaryProvider, primaryModel) {
        log.Info().
            Str("job_id", payload.JobID).
            Int("page", payload.PageID).
            Str("provider", primaryProvider).
            Str("model", primaryModel).
            Msg("attempting AI processing [1/4]")

        result, err := w.callAIWithTimeout(ctx, primaryProvider, primaryModel, request)

        if err == nil {
            // SUCCESS - zatvori breaker i vrati rezultat
            w.closeCircuitBreaker(primaryProvider, primaryModel)
            w.recordMetrics(primaryProvider, primaryModel, "success", result.LatencyMs)
            w.notifyPageDone(payload.JobID, payload.PageID, result)
            w.markProcessed(payload.IdempotencyKey)
            return nil
        }

        // Handle error
        if w.isTransientError(err) {
            // 429, 5xx, timeout → otvori breaker i pokušaj fallback
            w.openCircuitBreaker(primaryProvider, primaryModel, w.cfg.BreakerBaseBackoff)
            w.recordMetrics(primaryProvider, primaryModel, "transient", 0)

            log.Warn().
                Err(err).
                Str("job_id", payload.JobID).
                Int("page", payload.PageID).
                Str("provider", primaryProvider).
                Str("model", primaryModel).
                Msg("transient error - trying fallback")
        } else if w.isFatalError(err) {
            // 4xx validation error → bez retry
            w.recordMetrics(primaryProvider, primaryModel, "fatal", 0)
            w.notifyPageFailed(payload.JobID, payload.PageID, err)
            return err
        }
    } else {
        log.Debug().
            Str("provider", primaryProvider).
            Str("model", primaryModel).
            Msg("circuit breaker OPEN - skipping primary attempt")
    }

    // === ATTEMPT 2: Primary Provider, Secondary Model ===
    secondaryModel := w.getModelForTier(primaryProvider, "secondary")

    // Samo ako je različit od primary modela i nije već pokušan
    if secondaryModel != primaryModel && !w.isCircuitOpen(primaryProvider, secondaryModel) {
        log.Info().
            Str("job_id", payload.JobID).
            Int("page", payload.PageID).
            Str("provider", primaryProvider).
            Str("model", secondaryModel).
            Msg("attempting secondary model same provider [2/4]")

        result, err := w.callAIWithTimeout(ctx, primaryProvider, secondaryModel, request)

        if err == nil {
            w.closeCircuitBreaker(primaryProvider, secondaryModel)
            w.recordMetrics(primaryProvider, secondaryModel, "success", result.LatencyMs)
            w.notifyPageDone(payload.JobID, payload.PageID, result)
            w.markProcessed(payload.IdempotencyKey)
            return nil
        }

        if w.isTransientError(err) {
            w.openCircuitBreaker(primaryProvider, secondaryModel, w.cfg.BreakerBaseBackoff)
            w.recordMetrics(primaryProvider, secondaryModel, "transient", 0)
        } else if w.isFatalError(err) {
            w.recordMetrics(primaryProvider, secondaryModel, "fatal", 0)
            w.notifyPageFailed(payload.JobID, payload.PageID, err)
            return err
        }
    }

    // === ATTEMPT 3: Secondary Provider, Primary/Fast Model ===
    if !w.isCircuitOpen(secondaryProvider, secondaryProviderModel) {
        log.Info().
            Str("job_id", payload.JobID).
            Int("page", payload.PageID).
            Str("provider", secondaryProvider).
            Str("model", secondaryProviderModel).
            Msg("attempting secondary provider [3/4]")

        result, err := w.callAIWithTimeout(ctx, secondaryProvider, secondaryProviderModel, request)

        if err == nil {
            w.closeCircuitBreaker(secondaryProvider, secondaryProviderModel)
            w.recordMetrics(secondaryProvider, secondaryProviderModel, "success", result.LatencyMs)
            w.notifyPageDone(payload.JobID, payload.PageID, result)
            w.markProcessed(payload.IdempotencyKey)
            return nil
        }

        if w.isTransientError(err) {
            w.openCircuitBreaker(secondaryProvider, secondaryProviderModel, w.cfg.BreakerBaseBackoff)
            w.recordMetrics(secondaryProvider, secondaryProviderModel, "transient", 0)
        } else if w.isFatalError(err) {
            w.recordMetrics(secondaryProvider, secondaryProviderModel, "fatal", 0)
            w.notifyPageFailed(payload.JobID, payload.PageID, err)
            return err
        }
    }

    // === ATTEMPT 4: Secondary Provider, Secondary Model ===
    secondaryProviderSecondaryModel := w.getModelForTier(secondaryProvider, "secondary")

    if secondaryProviderSecondaryModel != secondaryProviderModel &&
       !w.isCircuitOpen(secondaryProvider, secondaryProviderSecondaryModel) {
        log.Info().
            Str("job_id", payload.JobID).
            Int("page", payload.PageID).
            Str("provider", secondaryProvider).
            Str("model", secondaryProviderSecondaryModel).
            Msg("attempting secondary provider secondary model [4/4]")

        result, err := w.callAIWithTimeout(ctx, secondaryProvider, secondaryProviderSecondaryModel, request)

        if err == nil {
            w.closeCircuitBreaker(secondaryProvider, secondaryProviderSecondaryModel)
            w.recordMetrics(secondaryProvider, secondaryProviderSecondaryModel, "success", result.LatencyMs)
            w.notifyPageDone(payload.JobID, payload.PageID, result)
            w.markProcessed(payload.IdempotencyKey)
            return nil
        }

        if w.isTransientError(err) {
            w.openCircuitBreaker(secondaryProvider, secondaryProviderSecondaryModel, w.cfg.BreakerBaseBackoff)
            w.recordMetrics(secondaryProvider, secondaryProviderSecondaryModel, "transient", 0)
        }
    }

    // === ALL ATTEMPTS EXHAUSTED ===
    log.Error().
        Str("job_id", payload.JobID).
        Int("page", payload.PageID).
        Msg("all AI providers/models exhausted - marking page as failed")

    w.recordMetrics("all", "all", "exhausted", 0)
    w.notifyPageFailed(payload.JobID, payload.PageID, fmt.Errorf("all providers exhausted"))

    return fmt.Errorf("all failover attempts failed")
}
```

---

## 4. CIRCUIT BREAKER – Non-Blocking Cooldown

### 4.1 Redis Keys i State Machine

**Format:**
```
cb:{provider}:{model} → Hash {
    "state": "open|closed|half_open",
    "retry_at": unix_timestamp,
    "failures": count,
    "opened_at": unix_timestamp
}

TTL: 10 minuta (auto cleanup)
```

**States:**
- `closed` – normalan rad, breaker nije aktivan
- `open` – cooldown period, model se ne koristi
- `half_open` – probni request dozvoljen (prvi nakon cooldown-a)

---

### 4.2 Circuit Breaker Operations

#### Open Breaker (na transient error)

```go
func (w *Worker) openCircuitBreaker(provider, model string, baseCooldown time.Duration) {
    key := fmt.Sprintf("cb:%s:%s", provider, model)
    ctx := context.Background()

    // Get current failure count
    failuresStr, _ := w.redis.HGet(ctx, key, "failures").Result()
    failures, _ := strconv.Atoi(failuresStr)
    failures++

    // Exponential backoff: 30s, 60s, 120s, 240s, max 5m
    backoff := min(
        baseCooldown * time.Duration(1<<uint(failures-1)),
        5*time.Minute,
    )

    retryAt := time.Now().Add(backoff).Unix()
    openedAt := time.Now().Unix()

    // Write to Redis
    w.redis.HSet(ctx, key, map[string]interface{}{
        "state":     "open",
        "retry_at":  retryAt,
        "failures":  failures,
        "opened_at": openedAt,
    })
    w.redis.Expire(ctx, key, 10*time.Minute)

    log.Warn().
        Str("provider", provider).
        Str("model", model).
        Dur("cooldown", backoff).
        Int("failures", failures).
        Time("retry_at", time.Unix(retryAt, 0)).
        Msg("circuit breaker OPENED")

    // Metrics
    w.metrics.BreakerEvents.WithLabelValues(provider, model, "opened").Inc()
}
```

#### Check Breaker Status

```go
func (w *Worker) isCircuitOpen(provider, model string) bool {
    key := fmt.Sprintf("cb:%s:%s", provider, model)
    ctx := context.Background()

    // Get state
    state, err := w.redis.HGet(ctx, key, "state").Result()
    if err != nil || state == "" {
        // No breaker record → closed by default
        return false
    }

    if state != "open" {
        // Already closed or half-open
        return false
    }

    // Check if cooldown expired
    retryAtStr, _ := w.redis.HGet(ctx, key, "retry_at").Result()
    retryAt, _ := strconv.ParseInt(retryAtStr, 10, 64)

    if time.Now().Unix() >= retryAt {
        // Cooldown expired → move to half-open
        w.redis.HSet(ctx, key, "state", "half_open")

        log.Info().
            Str("provider", provider).
            Str("model", model).
            Msg("circuit breaker moved to HALF-OPEN")

        w.metrics.BreakerEvents.WithLabelValues(provider, model, "half_open").Inc()

        return false // Allow probni request
    }

    // Still in cooldown
    return true
}
```

#### Close Breaker (na success)

```go
func (w *Worker) closeCircuitBreaker(provider, model string) {
    key := fmt.Sprintf("cb:%s:%s", provider, model)
    ctx := context.Background()

    // Get current state
    state, _ := w.redis.HGet(ctx, key, "state").Result()

    if state == "" || state == "closed" {
        // Already closed
        return
    }

    // Reset breaker
    w.redis.Del(ctx, key)

    log.Info().
        Str("provider", provider).
        Str("model", model).
        Msg("circuit breaker CLOSED (reset)")

    w.metrics.BreakerEvents.WithLabelValues(provider, model, "closed").Inc()
}
```

---

## 5. JOB CANCELLATION – Redis Cancel Set

### 5.1 Cancel Mechanism

**Redis Key:**
```
cancelled_jobs → Set {job_id1, job_id2, ...}

TTL per job: 24h nakon dodavanja
```

**Operacije:**

#### Cancel Job (Orchestrator)
```go
func (q *RedisQueue) CancelJob(ctx context.Context, jobID string) error {
    key := "cancelled_jobs"

    // Add to set
    err := q.redis.SAdd(ctx, key, jobID).Err()
    if err != nil {
        return err
    }

    // Set expiry na 24h
    q.redis.Expire(ctx, key, 24*time.Hour)

    log.Info().Str("job_id", jobID).Msg("job marked as cancelled in Redis")

    return nil
}
```

#### Check Cancellation (Worker)
```go
func (w *Worker) isJobCancelled(jobID string) bool {
    key := "cancelled_jobs"

    isMember, err := w.redis.SIsMember(context.Background(), key, jobID).Result()
    if err != nil {
        log.Warn().Err(err).Str("job_id", jobID).Msg("failed to check cancel status")
        return false
    }

    return isMember
}
```

**Integracija u Worker Loop:**
```go
func (w *Worker) processPageWithFailover(ctx context.Context, payload AIJobPayload) error {
    // PRVA STVAR - provjera cancellation-a
    if w.isJobCancelled(payload.JobID) {
        log.Info().
            Str("job_id", payload.JobID).
            Int("page", payload.PageID).
            Msg("skipping page - job cancelled")
        return fmt.Errorf("job cancelled")
    }

    // ... rest of processing ...
}
```

---

## 6. FINALIZACIJA – Normalan i Timeout Flow

### 6.1 Normalan Završetak (Sve stranice obrađene)

```go
func (o *Orchestrator) finalizeJobComplete(ctx context.Context, jobID string, totalPages int) {
    log.Info().
        Str("job_id", jobID).
        Int("total_pages", totalPages).
        Msg("finalizing job - all pages processed")

    // Aggregate sve stranice
    aggregatedText, sources := o.aggregateAllPages(ctx, jobID, totalPages)

    // Spremi rezultat (S3 ili lokalno)
    st, _ := o.deps.Status.Get(ctx, jobID)

    if src, _ := st.Metadata["source"].(string); src == "upload" {
        localPath, err := o.SaveAggregatedTextToLocal(ctx, jobID, aggregatedText)
        if err != nil {
            log.Error().Err(err).Str("job_id", jobID).Msg("failed to save result locally")
            st.Status = "failed"
            st.Message = fmt.Sprintf("Failed to save result: %v", err)
            o.deps.Status.Set(ctx, jobID, st)
            return
        }
        st.Metadata["result_local_path"] = localPath
    } else {
        filePath, _ := st.Metadata["file_path"].(string)
        password, _ := st.Metadata["password"].(string)
        s3URL, err := o.SaveAggregatedTextToS3(ctx, filePath, jobID, aggregatedText, password)
        if err != nil {
            log.Error().Err(err).Str("job_id", jobID).Msg("failed to save result to S3")
            st.Status = "failed"
            st.Message = fmt.Sprintf("Failed to save result: %v", err)
            o.deps.Status.Set(ctx, jobID, st)
            return
        }
        st.Metadata["result_s3_url"] = s3URL
    }

    // Update status
    endTime := time.Now()
    st.Status = "success"
    st.Progress = 100
    st.End = &endTime
    st.Message = "Processing completed successfully"
    st.Metadata["result_text_len"] = len(aggregatedText)
    st.Metadata["ai_pages_succeeded"] = intFromMeta(st.Metadata, "pages_done")
    st.Metadata["mupdf_fallback_count"] = intFromMeta(st.Metadata, "pages_failed")
    st.Metadata["timeout_occurred"] = false

    // Source breakdown
    aiCount := 0
    mupdfCount := 0
    for _, src := range sources {
        if src == "ai" {
            aiCount++
        } else {
            mupdfCount++
        }
    }
    st.Metadata["final_ai_pages"] = aiCount
    st.Metadata["final_mupdf_pages"] = mupdfCount

    o.deps.Status.Set(ctx, jobID, st)

    log.Info().
        Str("job_id", jobID).
        Int("total_chars", len(aggregatedText)).
        Int("ai_pages", aiCount).
        Int("mupdf_pages", mupdfCount).
        Msg("job finalized successfully")
}
```

---

### 6.2 Timeout Finalizacija (Parcijalni rezultati)

```go
func (o *Orchestrator) finalizeJobWithPartialResults(ctx context.Context, jobID string, totalPages int) {
    log.Warn().
        Str("job_id", jobID).
        Dur("timeout", o.cfg.JobTimeout).
        Msg("job timeout - finalizing with partial results")

    st, ok, _ := o.deps.Status.Get(ctx, jobID)
    if !ok {
        log.Error().Str("job_id", jobID).Msg("status not found for timeout finalization")
        return
    }

    // Identifikacija nedostajućih stranica
    pagesDone := intFromMeta(st.Metadata, "pages_done")
    pagesFailed := intFromMeta(st.Metadata, "pages_failed")
    pagesProcessed := pagesDone + pagesFailed

    missingPages := []int{}

    // Get lista AI stranica
    aiPagesInterface, _ := st.Metadata["ai_page_numbers"].([]interface{})
    aiPageNumbers := []int{}
    for _, p := range aiPagesInterface {
        if pNum, ok := p.(int); ok {
            aiPageNumbers = append(aiPageNumbers, pNum)
        } else if pNum, ok := p.(float64); ok {
            aiPageNumbers = append(aiPageNumbers, int(pNum))
        }
    }

    // Provjeri koja AI stranica nedostaje
    for _, pageNum := range aiPageNumbers {
        _, exists := o.deps.Pages.GetPageText(ctx, jobID, pageNum)
        if !exists {
            missingPages = append(missingPages, pageNum)
        }
    }

    log.Warn().
        Str("job_id", jobID).
        Ints("missing_pages", missingPages).
        Int("count", len(missingPages)).
        Msg("applying MuPDF fallback for timeout pages")

    // Dopuni nedostajuće stranice MuPDF tekstom
    for _, pageNum := range missingPages {
        mupdfText := o.getMuPDFTextForPage(ctx, jobID, pageNum)

        o.deps.Pages.SavePageText(
            ctx,
            jobID,
            pageNum,
            mupdfText,
            "mupdf_timeout_fallback",
            "",
            "",
        )

        log.Debug().
            Str("job_id", jobID).
            Int("page", pageNum).
            Int("text_len", len(mupdfText)).
            Msg("applied MuPDF timeout fallback")
    }

    // Agregacija sa svim stranicama (AI + MuPDF + timeout fallback)
    aggregatedText, sources := o.aggregateAllPages(ctx, jobID, totalPages)

    // Spremi rezultat
    if src, _ := st.Metadata["source"].(string); src == "upload" {
        localPath, _ := o.SaveAggregatedTextToLocal(ctx, jobID, aggregatedText)
        st.Metadata["result_local_path"] = localPath
    } else {
        filePath, _ := st.Metadata["file_path"].(string)
        password, _ := st.Metadata["password"].(string)
        s3URL, _ := o.SaveAggregatedTextToS3(ctx, filePath, jobID, aggregatedText, password)
        st.Metadata["result_s3_url"] = s3URL
    }

    // Update status
    endTime := time.Now()
    st.Status = "success"
    st.Progress = 100
    st.End = &endTime
    st.Message = "Completed with partial AI results (timeout)"
    st.Metadata["result_text_len"] = len(aggregatedText)
    st.Metadata["timeout_occurred"] = true
    st.Metadata["missing_pages"] = len(missingPages)
    st.Metadata["timeout_fallback_pages"] = len(missingPages)

    // Source breakdown
    aiCount := 0
    mupdfCount := 0
    timeoutFallbackCount := 0
    for _, src := range sources {
        if src == "ai" {
            aiCount++
        } else if src == "mupdf_timeout_fallback" {
            timeoutFallbackCount++
        } else {
            mupdfCount++
        }
    }
    st.Metadata["final_ai_pages"] = aiCount
    st.Metadata["final_mupdf_pages"] = mupdfCount
    st.Metadata["final_timeout_fallback_pages"] = timeoutFallbackCount

    o.deps.Status.Set(ctx, jobID, st)

    log.Warn().
        Str("job_id", jobID).
        Int("total_chars", len(aggregatedText)).
        Int("ai_pages", aiCount).
        Int("mupdf_pages", mupdfCount).
        Int("timeout_fallback", timeoutFallbackCount).
        Msg("job finalized with timeout fallback")
}
```

---

### 6.3 Helper: Get MuPDF Text for Page

```go
func (o *Orchestrator) getMuPDFTextForPage(ctx context.Context, jobID string, pageNum int) string {
    // Prvo pokušaj iz page store (možda je već spreman)
    existingText, _ := o.deps.Pages.GetPageText(ctx, jobID, pageNum)
    if existingText != "" {
        return existingText
    }

    // Pokušaj iz MuPDF cache (spremljeno pri inicijalnoj klasifikaciji)
    key := fmt.Sprintf("job:%s:mupdf:%d", jobID, pageNum)
    mupdfText, err := o.deps.Redis.Get(ctx, key).Result()
    if err == nil && mupdfText != "" {
        return mupdfText
    }

    // Fallback - vrati placeholder
    log.Warn().
        Str("job_id", jobID).
        Int("page", pageNum).
        Msg("MuPDF text not available for fallback - using placeholder")

    return fmt.Sprintf("[Page %d - text not available]", pageNum)
}
```

---

### 6.4 Helper: Aggregate All Pages

```go
func (o *Orchestrator) aggregateAllPages(ctx context.Context, jobID string, totalPages int) (string, []string) {
    var builder strings.Builder
    sources := make([]string, totalPages)

    for i := 1; i <= totalPages; i++ {
        pageText, source := o.deps.Pages.GetPageText(ctx, jobID, i)

        if i > 1 {
            builder.WriteString("\n\n")
        }

        builder.WriteString(fmt.Sprintf("=== Page %d ===\n", i))
        builder.WriteString(pageText)

        sources[i-1] = source

        log.Debug().
            Str("job_id", jobID).
            Int("page", i).
            Str("source", source).
            Int("length", len(pageText)).
            Msg("aggregated page")
    }

    return builder.String(), sources
}
```

---

## 7. MUPDF FALLBACK STORAGE – Pre-Storage Strategy

### 7.1 Problem

Pri timeout-u ili AI fail-u trebamo MuPDF tekst za fallback. Gdje ga držati?

### 7.2 Rješenje: Redis Pre-Storage

**U `ProcessJobForAI()` nakon klasifikacije:**

```go
func (o *Orchestrator) ProcessJobForAI(ctx context.Context, jobID, filePath, user, password string) error {
    // ... download, classify stranice ...

    // Extract MuPDF tekst za sve stranice
    pageTexts := o.extractAllPageTexts(ctx, pdfPath)

    // PRE-STORAGE: spremi MuPDF tekst u Redis ZA SVE stranice
    for pageNum, text := range pageTexts {
        key := fmt.Sprintf("job:%s:mupdf:%d", jobID, pageNum)
        err := o.deps.Redis.Set(ctx, key, text, 24*time.Hour).Err()
        if err != nil {
            log.Warn().
                Err(err).
                Str("job_id", jobID).
                Int("page", pageNum).
                Msg("failed to pre-store MuPDF text")
        }
    }

    log.Info().
        Str("job_id", jobID).
        Int("pages", len(pageTexts)).
        Msg("pre-stored MuPDF text for all pages")

    // ... classify, build payloads, enqueue ...
}
```

**TTL:** 24 sata (dovoljno za završetak i cleanup)

---

## 8. ERROR CLASSIFICATION

### 8.1 Error Types

```go
// Transient errors → failover
func (w *Worker) isTransientError(err error) bool {
    if err == nil {
        return false
    }

    // Timeout
    if errors.Is(err, context.DeadlineExceeded) {
        return true
    }

    // RateLimitError
    var rateLimitErr *RateLimitError
    if errors.As(err, &rateLimitErr) {
        return true
    }

    // HTTP 5xx server errors
    var httpErr *HTTPError
    if errors.As(err, &httpErr) {
        return httpErr.StatusCode >= 500 && httpErr.StatusCode < 600
    }

    // HTTP 429
    if errors.As(err, &httpErr) {
        return httpErr.StatusCode == 429
    }

    // Network errors
    if strings.Contains(err.Error(), "connection refused") ||
       strings.Contains(err.Error(), "connection reset") ||
       strings.Contains(err.Error(), "timeout") {
        return true
    }

    return false
}

// Fatal errors → ne retry, označi page kao failed
func (w *Worker) isFatalError(err error) bool {
    if err == nil {
        return false
    }

    // HTTP 4xx (osim 429)
    var httpErr *HTTPError
    if errors.As(err, &httpErr) {
        return httpErr.StatusCode >= 400 && httpErr.StatusCode < 500 && httpErr.StatusCode != 429
    }

    // Validation errors
    if strings.Contains(err.Error(), "invalid request") ||
       strings.Contains(err.Error(), "validation failed") {
        return true
    }

    return false
}
```

### 8.2 Custom Error Types

```go
// RateLimitError - 429 ili timeout
type RateLimitError struct {
    Provider string
    Model    string
    Reason   string
}

func (e *RateLimitError) Error() string {
    return fmt.Sprintf("rate limit: %s/%s - %s", e.Provider, e.Model, e.Reason)
}

// HTTPError - HTTP status errors
type HTTPError struct {
    StatusCode int
    Body       string
}

func (e *HTTPError) Error() string {
    return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Body)
}
```

---

## 9. METRICS – Prometheus Integration

### 9.1 Metrics Definition

```go
type Metrics struct {
    // Provider requests
    ProviderRequests *prometheus.CounterVec // {provider, model, result}

    // Latency
    ProviderLatency *prometheus.HistogramVec // {provider, model}

    // Pages processed
    PagesProcessed *prometheus.CounterVec // {result}

    // Failovers
    FailoverEvents *prometheus.CounterVec // {from_provider, to_provider}

    // Circuit breaker
    BreakerEvents *prometheus.CounterVec // {provider, model, action}

    // Timeouts
    TimeoutEvents *prometheus.CounterVec // {type} (request, job)

    // Queue depth
    QueueDepth prometheus.Gauge
}

func NewMetrics() *Metrics {
    m := &Metrics{
        ProviderRequests: prometheus.NewCounterVec(
            prometheus.CounterOpts{
                Name: "aidispatcher_provider_requests_total",
                Help: "Total AI provider requests",
            },
            []string{"provider", "model", "result"},
        ),
        ProviderLatency: prometheus.NewHistogramVec(
            prometheus.HistogramOpts{
                Name:    "aidispatcher_provider_latency_seconds",
                Help:    "AI provider request latency",
                Buckets: []float64{1, 5, 10, 30, 60, 120, 180},
            },
            []string{"provider", "model"},
        ),
        PagesProcessed: prometheus.NewCounterVec(
            prometheus.CounterOpts{
                Name: "aidispatcher_pages_processed_total",
                Help: "Total pages processed",
            },
            []string{"result"}, // success, failed, timeout
        ),
        FailoverEvents: prometheus.NewCounterVec(
            prometheus.CounterOpts{
                Name: "aidispatcher_failover_events_total",
                Help: "Failover events between providers",
            },
            []string{"from_provider", "to_provider"},
        ),
        BreakerEvents: prometheus.NewCounterVec(
            prometheus.CounterOpts{
                Name: "aidispatcher_breaker_events_total",
                Help: "Circuit breaker state changes",
            },
            []string{"provider", "model", "action"}, // opened, closed, half_open
        ),
        TimeoutEvents: prometheus.NewCounterVec(
            prometheus.CounterOpts{
                Name: "aidispatcher_timeout_events_total",
                Help: "Timeout events",
            },
            []string{"type"}, // request, job
        ),
        QueueDepth: prometheus.NewGauge(
            prometheus.GaugeOpts{
                Name: "aidispatcher_queue_depth",
                Help: "Current queue depth",
            },
        ),
    }

    // Register all metrics
    prometheus.MustRegister(
        m.ProviderRequests,
        m.ProviderLatency,
        m.PagesProcessed,
        m.FailoverEvents,
        m.BreakerEvents,
        m.TimeoutEvents,
        m.QueueDepth,
    )

    return m
}
```

### 9.2 Recording Metrics

```go
// U worker-u nakon AI poziva
func (w *Worker) recordMetrics(provider, model, result string, latencyMs int64) {
    w.metrics.ProviderRequests.WithLabelValues(provider, model, result).Inc()

    if latencyMs > 0 {
        w.metrics.ProviderLatency.WithLabelValues(provider, model).Observe(float64(latencyMs) / 1000.0)
    }
}

// Na timeout event
func (w *Worker) recordTimeout(timeoutType string) {
    w.metrics.TimeoutEvents.WithLabelValues(timeoutType).Inc()
}

// Na page completion
func (w *Worker) notifyPageDone(jobID string, pageID int, result *AIResponse) error {
    // ... existing logic ...

    w.metrics.PagesProcessed.WithLabelValues("success").Inc()

    // ...
}

// Na page failure
func (w *Worker) notifyPageFailed(jobID string, pageID int, err error) error {
    // ... existing logic ...

    w.metrics.PagesProcessed.WithLabelValues("failed").Inc()

    // ...
}
```

---

## 10. IMPLEMENTATION CHECKLIST

### Faza 1: Config & Timeouts ✅
- [x] Dodati `TimeoutConfig` u `internal/config/env.go`
- [x] Parsiranje `REQUEST_TIMEOUT` i `JOB_TIMEOUT` iz ENV
- [x] Backwards compatibility za legacy ENV varijable
- [x] Helper funkcija `parseDuration()`

### Faza 2: Job Monitor & Cancellation
- [ ] Implementirati `monitorJobCompletion()` u `orchestrator/ai_pipeline.go`
- [ ] Dodati `CancelJob()` metodu u `queue/redis.go`
- [ ] Dodati `isJobCancelled()` check u `dispatcher/worker.go`
- [ ] Integrirati monitor u `ProcessJobForAI()` (goroutine)

### Faza 3: MuPDF Pre-Storage
- [ ] Dodati pre-storage loop u `ProcessJobForAI()` za sve stranice
- [ ] Redis key format: `job:{jobID}:mupdf:{pageNum}`
- [ ] TTL: 24h
- [ ] Implementirati `getMuPDFTextForPage()` helper

### Faza 4: Worker Failover Refactoring
- [ ] Refaktorirati `processPageWithFailover()` sa 4-step failover
- [ ] Dodati cancellation check na početak
- [ ] Implementirati `callAIWithTimeout()` sa REQUEST_TIMEOUT
- [ ] Error classification: `isTransientError()`, `isFatalError()`
- [ ] Inline failover bez ZSET delay

### Faza 5: Circuit Breaker
- [ ] Implementirati `openCircuitBreaker()` sa exponential backoff
- [ ] Implementirati `isCircuitOpen()` sa half-open logikom
- [ ] Implementirati `closeCircuitBreaker()` na success
- [ ] Redis hash structure sa TTL

### Faza 6: Finalization
- [ ] Implementirati `finalizeJobComplete()` (normalan završetak)
- [ ] Implementirati `finalizeJobWithPartialResults()` (timeout)
- [ ] Implementirati `aggregateAllPages()` helper
- [ ] Update `handlePageDone()` i `handlePageFailed()` za `checkAndFinalizeJob()`

### Faza 7: Metrics
- [ ] Definirati `Metrics` struct sa Prometheus counterima/histogramima
- [ ] Integrirati u worker (`recordMetrics()`)
- [ ] Integrirati u orchestrator (timeout events)
- [ ] Expose `/metrics` endpoint

### Faza 8: Testing
- [ ] E2E test sa normalnim završetkom
- [ ] E2E test sa job timeout-om (parcijalni rezultati)
- [ ] E2E test sa request timeout-om (failover)
- [ ] Test circuit breaker-a (simulirani 429)
- [ ] Test cancellation-a (cancel job mid-processing)

---

## 11. KEY BEHAVIORS – Summary

### ✅ Request Timeout (120s)
- Primjenjuje se na **svaki pojedini AI API poziv**
- Na timeout → tretira se kao **429 (transient)**
- **Aktivira failover** na sljedeći model/provider
- Worker **NE ČEKA** – odmah nastavlja

### ✅ Job Timeout (5 minuta)
- Primjenjuje se na **cijeli dokument** (sve stranice)
- Monitor provjerava completion svakih 2s
- Na timeout:
  1. **Cancel job u Redis** (`cancelled_jobs` set)
  2. **Prikupi sve uspješne AI rezultate**
  3. **Dopuni nedostajuće MuPDF tekstom**
  4. **Finalizira kao `success`** sa `timeout_occurred=true`
- **Garantuje završetak** unutar SLA

### ✅ Circuit Breaker
- **Exponential backoff**: 30s, 60s, 120s, 240s, max 5m
- **State machine**: closed → open → half_open → closed
- **Non-blocking**: worker odmah pokušava drugi model
- **Auto-recovery**: half-open nakon cooldown

### ✅ Failover Sekvenca (4 koraka)
1. Primary provider + primary/fast model
2. Primary provider + secondary model
3. Secondary provider + primary/fast model
4. Secondary provider + secondary model
**→ Ako sve padne: `/internal/page_failed` (MuPDF fallback)**

### ✅ MuPDF Fallback
- **Pre-storage**: MuPDF tekst za sve stranice u Redis pri početku
- **On-demand**: iz cache-a kada AI failuje
- **Seamless mix**: finalni agregat kombinuje AI + MuPDF
- **Transparent**: user dobije kompletan dokument

### ✅ Determinističko Dovršavanje
- **Job timeout enforces SLA** (5 min max)
- **Nema beskrajnog čekanja** na providere
- **Uvijek postoji rezultat** (AI + MuPDF mix)
- **Status = success** (sa metadata o timeout-u/fallback-u)

---

## 12. ENV VARIABLES – Kompletna Lista

```bash
# ========================================
# TIMEOUTS
# ========================================
REQUEST_TIMEOUT=120s           # Max vrijeme po AI API pozivu
JOB_TIMEOUT=5m                 # Max vrijeme za cijeli dokument

# ========================================
# PROVIDERS & MODELS
# ========================================
PRIMARY_ENGINE=openai          # Primary AI provider
SECONDARY_ENGINE=anthropic     # Secondary AI provider (failover)

# OpenAI modeli
OPENAI_PRIMARY_MODEL=gpt-4o
OPENAI_SECONDARY_MODEL=gpt-4o-mini
OPENAI_FAST_MODEL=gpt-4o-mini
OPENAI_API_KEY=sk-...

# Anthropic modeli
ANTHROPIC_PRIMARY_MODEL=claude-3-5-sonnet-20241022
ANTHROPIC_SECONDARY_MODEL=claude-3-opus
ANTHROPIC_FAST_MODEL=claude-3-haiku
ANTHROPIC_API_KEY=sk-ant-...

# ========================================
# WORKER & CONCURRENCY
# ========================================
WORKER_CONCURRENCY=8           # Broj worker goroutina
MAX_INFLIGHT_PER_MODEL=2       # Max paralelnih requestova po modelu

# ========================================
# CIRCUIT BREAKER
# ========================================
BREAKER_BASE_BACKOFF=30s       # Početni cooldown
BREAKER_MAX_BACKOFF=5m         # Maksimalni cooldown

# ========================================
# QUEUE
# ========================================
REDIS_URL=redis://localhost:6379
QUEUE_STREAM=jobs:ai:pages
QUEUE_GROUP=workers:images
QUEUE_POLL_INTERVAL=100ms

# ========================================
# IMAGE RENDERING
# ========================================
IMAGE_SEND_ALL_PAGES=false     # false = samo HAS_GRAPHICS
IMAGE_DPI=100
IMAGE_COLOR=rgb                # rgb ili gray
IMAGE_FORMAT=jpeg
IMAGE_JPEG_QUALITY=70          # 1-100
IMAGE_CONTEXT_RADIUS=1         # ±N stranica konteksta
IMAGE_MAX_CONTEXT_BYTES=3000   # Max 3KB teksta konteksta
IMAGE_INCLUDE_BASE64=true

# ========================================
# SYSTEM PROMPT
# ========================================
AI_SYSTEM_PROMPT="..."         # Override default system prompt

# ========================================
# STORAGE
# ========================================
AWS_S3_BUCKET=my-bucket
AWS_REGION=us-east-1
AWS_ACCESS_KEY_ID=...
AWS_SECRET_ACCESS_KEY=...

UPLOAD_DIR=./uploads
RESULT_DIR=./uploads/results

# ========================================
# LOGGING
# ========================================
LOG_LEVEL=info                 # debug, info, warn, error
LOG_PRETTY=false               # true za dev
LOG_FILE=logs/aidispatcher.log
LOG_MAX_SIZE_MB=100
LOG_MAX_BACKUPS=10

# ========================================
# AXIOM (opcionalno)
# ========================================
SEND_LOGS_TO_AXIOM=1
AXIOM_API_KEY=...
AXIOM_ORG_ID=...
AXIOM_DATASET=prod_aidispatcher

# ========================================
# WEB DASHBOARD
# ========================================
WEB_USERNAME=admin
WEB_PASSWORD=secret
PORT=8080

# ========================================
# DISPATCHER ENABLE
# ========================================
RUN_DISPATCHER=1               # Omogući worker pool u istom procesu
```

---

## 13. REDIS KEYS REFERENCE

```
# Job status
job:{jobID}:status → Hash (status, progress, metadata, start, end, message)

# Job-to-file mapping
file:{fileID}:job_id → String (jobID)

# Page text storage
job:{jobID}:page:{pageNum} → Hash (text, source, provider, model)

# MuPDF pre-storage (fallback)
job:{jobID}:mupdf:{pageNum} → String (mupdf_text) [TTL: 24h]

# Circuit breaker state
cb:{provider}:{model} → Hash (state, retry_at, failures, opened_at) [TTL: 10m]

# Cancelled jobs set
cancelled_jobs → Set (jobID1, jobID2, ...) [TTL: 24h]

# Queue (Redis Streams)
jobs:ai:pages → Stream (payloads)
jobs:ai:pages:delayed → ZSET (delayed retry - opcionalno)
jobs:ai:pages:dlq → Stream (dead letter queue)

# Idempotency tracking
processed:{idempotency_key} → String (timestamp) [TTL: 24h]
```

---

## 14. PAYLOAD SCHEMA – Final

### AIJobPayload (Queue Message)

```json
{
  "job_id": "uuid-v4",
  "document_id": "file_id_cleaned",
  "page_id": 5,

  "image_b64": "base64_encoded_jpeg_data...",
  "image_mime": "image/jpeg",
  "width_px": 850,
  "height_px": 1100,

  "mupdf_text": "Extracted text from page 5 using MuPDF...",
  "context_text": "=== Page 4 (before) ===\n...\n=== Page 5 (current) ===\n...\n=== Page 6 (after) ===\n...",

  "system_prompt": "You are an expert document analysis AI...",
  "ai_provider": "openai",
  "model_tier": "primary",

  "classification": "HAS_GRAPHICS",
  "idempotency_key": "doc:uuid:page:5:v1",
  "attempt": 1,

  "user": "user@example.com",
  "source": "api"
}
```

**Veličina:**
- Bez slike (TEXT_ONLY): ~5-10 KB
- Sa slikom (HAS_GRAPHICS, JPEG 70%): ~70-200 KB

---

## KRAJ PLANA

Ovaj plan pokriva kompletnu integraciju AI Dispatcher-a sa:
- ✅ Konfigurabilan request timeout (120s) i job timeout (5m)
- ✅ Dvoslojni timeout sistem (request + job level)
- ✅ Determinističko završavanje unutar SLA
- ✅ 4-step failover sa circuit breakerom
- ✅ MuPDF pre-storage i fallback
- ✅ Job cancellation mehanizam
- ✅ Prometheus metrics
- ✅ Kompletna ENV konfiguracija

**Sljedeći korak:** Implementacija po fazama iz checklistea.
