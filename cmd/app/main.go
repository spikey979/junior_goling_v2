package main

import (
    "context"
    "fmt"
    "net/http"
    "os"
    "os/signal"
    "syscall"
    "time"

    "github.com/joho/godotenv"
    "github.com/rs/zerolog/log"

    cfgpkg "github.com/local/aidispatcher/internal/config"
    logpkg "github.com/local/aidispatcher/internal/logger"
    "github.com/local/aidispatcher/internal/converter"
    "github.com/local/aidispatcher/internal/dispatcher"
    "github.com/local/aidispatcher/internal/filetype"
    "github.com/local/aidispatcher/internal/orchestrator"
    "github.com/local/aidispatcher/internal/queue"
    "github.com/local/aidispatcher/internal/statuscheck"
    web "github.com/local/aidispatcher/internal/web"
    "github.com/local/aidispatcher/internal/store"
    mpkg "github.com/local/aidispatcher/internal/metrics"
)

func main() {
    // Load .env file if it exists
    _ = godotenv.Load()

    cfg := cfgpkg.FromEnv()

    // Init logging
    _ = logpkg.Init(logpkg.Options{
        Level: cfg.Logging.Level,
        Pretty: cfg.Logging.Pretty,
        File: cfg.Logging.File,
        MaxSizeMB: cfg.Logging.MaxSizeMB,
        MaxBackups: cfg.Logging.MaxBackups,
        MaxAgeDays: cfg.Logging.MaxAgeDays,
        Compress: cfg.Logging.Compress,
        SendToAxiom: cfg.Axiom.Send && cfg.Axiom.APIKey != "",
        AxiomAPIKey: cfg.Axiom.APIKey,
        AxiomOrgID: cfg.Axiom.OrgID,
        AxiomDataset: cfg.Axiom.Dataset,
        AxiomFlush: cfg.Axiom.FlushInterval,
    })
    defer logpkg.Close()

    // Queue
    rq, err := queue.NewRedisQueue(cfg.Queue.RedisURL, cfg.Queue.Stream, cfg.Queue.Group, cfg.Queue.PollInterval)
    if err != nil {
        log.Fatal().Err(err).Msg("failed to connect to redis")
    }
    defer rq.Close()

    // Status store
    rs, err := store.NewRedisStatus(cfg.Queue.RedisURL)
    if err != nil {
        log.Fatal().Err(err).Msg("failed to init redis status store")
    }
    defer rs.Close()

    // Orchestrator HTTP server
    // Page store
    ps, err := store.NewPageStore(cfg.Queue.RedisURL)
    if err != nil { log.Fatal().Err(err).Msg("failed to init page store") }
    defer ps.Close()

    // LibreOffice converter
    librePort := 8100
    if p := os.Getenv("LIBREOFFICE_PORT"); p != "" {
        fmt.Sscanf(p, "%d", &librePort)
    }
    maxWorkers := 4
    if w := os.Getenv("LIBREOFFICE_MAX_WORKERS"); w != "" {
        fmt.Sscanf(w, "%d", &maxWorkers)
    }
    conv := converter.NewLibreOffice(librePort, maxWorkers)
    if err := conv.Initialize(); err != nil {
        log.Warn().Err(err).Msg("LibreOffice converter initialization failed - Office document conversion will not be available")
    } else {
        log.Info().Msg("LibreOffice converter initialized")
    }
    defer conv.Shutdown()

    // File type detector
    fileTypeDetector := filetype.New()

    orch := orchestrator.New(
        orchestrator.Dependencies{
            Queue:     rq,
            Status:    orchestrator.NewStatusAdapter(rs),
            Pages:     ps,
            Converter: conv,
            FileType:  fileTypeDetector,
            Redis:     orchestrator.NewRedisAdapter(rs.Client()), // Add Redis client for MuPDF pre-storage
        },
        orchestrator.Config{
            Timeouts: orchestrator.TimeoutConfig{
                JobTimeout: cfg.Timeouts.JobTimeout,
            },
            SystemPrompt: orchestrator.SystemPromptConfig{
                DefaultPrompt: cfg.SystemPrompt.DefaultPrompt,
            },
        },
    )
    mux := http.NewServeMux()
    orch.RegisterRoutes(mux)
    // Deep health route
    mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request){
        ctx, cancel := context.WithTimeout(r.Context(), 1*time.Second)
        defer cancel()
        type resp struct { OK bool `json:"ok"`; Redis string `json:"redis"`; Stream int64 `json:"stream_len"`; Delayed int64 `json:"delayed_len"`; DLQ int64 `json:"dlq_len"` }
        if err := rq.Ping(ctx); err != nil {
            w.WriteHeader(http.StatusServiceUnavailable)
            _, _ = w.Write([]byte(`{"ok":false,"redis":"down"}`))
            return
        }
        s, d, dlq, err := rq.Depths(ctx)
        if err != nil {
            w.WriteHeader(http.StatusServiceUnavailable)
            _, _ = w.Write([]byte(`{"ok":false,"redis":"error_depths"}`))
            return
        }
        w.Header().Set("Content-Type", "application/json")
        _, _ = w.Write([]byte(fmt.Sprintf(`{"ok":true,"redis":"ok","stream_len":%d,"delayed_len":%d,"dlq_len":%d}`, s, d, dlq)))
    })

    // Metrics
    mpkg.Init()
    mux.Handle("/metrics", mpkg.Handler())

    // Dashboard
    statusChecker := statuscheck.New(statuscheck.Options{
        Redis:       rq,
        S3Bucket:    os.Getenv("AWS_S3_BUCKET"),
        OpenAIKey:   os.Getenv("OPENAI_API_KEY"),
        AnthropicKey: os.Getenv("ANTHROPIC_API_KEY"),
    })
    web := web.New(statusChecker)
    web.RegisterRoutes(mux)

    // Dispatcher worker (optional)
    runDispatcher := os.Getenv("RUN_DISPATCHER")
    if runDispatcher == "" || runDispatcher == "1" || runDispatcher == "true" {
        disp := dispatcher.New(dispatcher.Config{Concurrency: cfg.Worker.Concurrency}, rq)
        disp.Start()
        defer disp.Stop(context.Background())
    }

    port := os.Getenv("PORT")
    if port == "" { port = "8080" }
    srv := &http.Server{Addr: ":"+port, Handler: mux}

    go func(){
        log.Info().Msgf("HTTP server listening on :%s", port)
        if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
            log.Fatal().Err(err).Msg("http server error")
        }
    }()

    // Background: publish queue depths to Prometheus
    go func(){
        ticker := time.NewTicker(5 * time.Second)
        defer ticker.Stop()
        for range ticker.C {
            ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
            s, d, dlq, err := rq.Depths(ctx)
            cancel()
            if err == nil {
                mpkg.SetQueueDepth("stream", s)
                mpkg.SetQueueDepth("delayed", d)
                mpkg.SetQueueDepth("dlq", dlq)
            }
        }
    }()

    // Graceful shutdown
    stop := make(chan os.Signal, 1)
    signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
    <-stop
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    _ = srv.Shutdown(ctx)
    fmt.Println("shutdown complete")
}
