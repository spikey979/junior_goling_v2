package main

import (
    "context"
    "fmt"
    "net/http"
    "os"
    "os/signal"
    "syscall"
    "time"

    "github.com/rs/zerolog/log"

    cfgpkg "github.com/local/aidispatcher/internal/config"
    logpkg "github.com/local/aidispatcher/internal/logger"
    "github.com/local/aidispatcher/internal/dispatcher"
    "github.com/local/aidispatcher/internal/orchestrator"
    "github.com/local/aidispatcher/internal/queue"
    web "github.com/local/aidispatcher/internal/web"
    "github.com/local/aidispatcher/internal/store"
)

func main() {
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
    rq, err := queue.NewRedisQueue(cfg.Queue.RedisURL)
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

    orch := orchestrator.New(orchestrator.Dependencies{
        Queue:  rq,
        Status: orchestrator.NewStatusAdapter(rs),
        Pages:  ps,
    })
    mux := http.NewServeMux()
    orch.RegisterRoutes(mux)

    // Dashboard
    web := web.New()
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

    // Graceful shutdown
    stop := make(chan os.Signal, 1)
    signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
    <-stop
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    _ = srv.Shutdown(ctx)
    fmt.Println("shutdown complete")
}
