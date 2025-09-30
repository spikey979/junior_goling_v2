package logger

import (
    "context"
    "encoding/json"
    "fmt"
    "io"
    "os"
    "path/filepath"
    "sync"
    "time"

    "github.com/axiomhq/axiom-go/axiom"
    "github.com/axiomhq/axiom-go/axiom/ingest"
    "github.com/rs/zerolog"
    "github.com/rs/zerolog/log"
    lumberjack "gopkg.in/natefinch/lumberjack.v2"
)

// Options defines logger initialization parameters.
type Options struct {
    Level        string
    Pretty       bool
    File         string
    MaxSizeMB    int
    MaxBackups   int
    MaxAgeDays   int
    Compress     bool

    // Axiom
    SendToAxiom  bool
    AxiomAPIKey  string
    AxiomOrgID   string
    AxiomDataset string
    AxiomFlush   time.Duration
}

var (
    global zerolog.Logger
    ax     *axiomClient
)

// Init sets up global logger: file rotation, optional console, optional Axiom forwarding.
func Init(opts Options) error {
    // Ensure log directory exists
    if opts.File != "" {
        if err := os.MkdirAll(filepath.Dir(opts.File), 0o755); err != nil {
            return fmt.Errorf("create logs dir: %w", err)
        }
    }

    // Build writers
    var writers []io.Writer

    if opts.File != "" {
        writers = append(writers, &lumberjack.Logger{
            Filename:   opts.File,
            MaxSize:    opts.MaxSizeMB,
            MaxBackups: opts.MaxBackups,
            MaxAge:     opts.MaxAgeDays,
            Compress:   opts.Compress,
        })
    }

    if opts.Pretty {
        cw := zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339}
        writers = append(writers, cw)
    } else {
        writers = append(writers, os.Stdout)
    }

    // Optional Axiom writer (info+)
    if opts.SendToAxiom && opts.AxiomAPIKey != "" {
        client, err := newAxiomClient(opts.AxiomAPIKey, opts.AxiomOrgID, opts.AxiomDataset, opts.AxiomFlush)
        if err != nil {
            // log to stderr and continue without Axiom
            fmt.Fprintf(os.Stderr, "Axiom disabled: %v\n", err)
        } else {
            ax = client
            writers = append(writers, &axiomWriter{client: client})
        }
    }

    out := io.MultiWriter(writers...)

    // Global zerolog config
    zerolog.TimeFieldFormat = time.RFC3339
    lvl, err := zerolog.ParseLevel(opts.Level)
    if err != nil {
        lvl = zerolog.InfoLevel
    }

    global = zerolog.New(out).Level(lvl).With().Timestamp().Logger()
    log.Logger = global
    return nil
}

// Close flushes any buffered external loggers.
func Close() {
    if ax != nil {
        _ = ax.Close()
    }
}

// Get returns the global logger.
func Get() *zerolog.Logger { return &global }

// Convenience methods
func Debug(msg string) { global.Debug().Msg(msg) }
func Info(msg string)  { global.Info().Msg(msg) }
func Warn(msg string)  { global.Warn().Msg(msg) }
func Error(msg string) { global.Error().Msg(msg) }

// axiomWriter forwards zerolog JSON lines to Axiom (dropping debug level).
type axiomWriter struct { client *axiomClient }

func (w *axiomWriter) Write(p []byte) (int, error) {
    var ev map[string]interface{}
    if err := json.Unmarshal(p, &ev); err != nil {
        ev = map[string]interface{}{"message": string(p), "level": "info"}
    }
    if lvl, ok := ev["level"].(string); ok && lvl == "debug" {
        return len(p), nil
    }
    ev["service"] = "aidispatcher"
    if _, ok := ev[ingest.TimestampField]; !ok {
        ev[ingest.TimestampField] = time.Now()
    }
    axEvent := axiom.Event(ev)
    w.client.Send(axEvent)
    return len(p), nil
}

// Minimal Axiom batching client
type axiomClient struct {
    client  *axiom.Client
    dataset string
    ch      chan axiom.Event
    wg      sync.WaitGroup
    ctx     context.Context
    cancel  context.CancelFunc
}

func newAxiomClient(token, orgID, dataset string, flushEvery time.Duration) (*axiomClient, error) {
    if dataset == "" { dataset = "dev_aidispatcher" }
    opts := []axiom.Option{axiom.SetToken(token)}
    if orgID != "" { opts = append(opts, axiom.SetOrganizationID(orgID)) }
    c, err := axiom.NewClient(opts...)
    if err != nil { return nil, err }
    ctx, cancel := context.WithCancel(context.Background())
    ac := &axiomClient{
        client:  c,
        dataset: dataset,
        ch:      make(chan axiom.Event, 1000),
        ctx:     ctx,
        cancel:  cancel,
    }
    if flushEvery <= 0 { flushEvery = 10 * time.Second }
    ac.wg.Add(1)
    go ac.loop(flushEvery)
    return ac, nil
}

func (a *axiomClient) Send(ev axiom.Event) {
    select {
    case a.ch <- ev:
    default:
        // drop if buffer full
    }
}

func (a *axiomClient) loop(flushEvery time.Duration) {
    defer a.wg.Done()
    ticker := time.NewTicker(flushEvery)
    defer ticker.Stop()
    batch := make([]axiom.Event, 0, 200)
    flush := func() {
        if len(batch) == 0 { return }
        ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
        _, _ = a.client.IngestEvents(ctx, a.dataset, batch)
        cancel()
        batch = batch[:0]
    }
    for {
        select {
        case <-a.ctx.Done():
            flush()
            return
        case <-ticker.C:
            flush()
        case ev := <-a.ch:
            batch = append(batch, ev)
            if len(batch) >= 200 { flush() }
        }
    }
}

func (a *axiomClient) Close() error {
    a.cancel()
    a.wg.Wait()
    return nil
}

