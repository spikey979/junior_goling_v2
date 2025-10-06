package metrics

import (
    "net/http"
    "time"

    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
    providerReqs = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Namespace: "aidispatcher",
            Name:      "provider_requests_total",
            Help:      "Total provider requests by provider, model and result",
        },
        []string{"provider", "model", "result"},
    )

    providerLatency = prometheus.NewHistogramVec(
        prometheus.HistogramOpts{
            Namespace: "aidispatcher",
            Name:      "provider_request_duration_seconds",
            Help:      "Duration of provider requests by provider and model",
            Buckets:   prometheus.DefBuckets,
        },
        []string{"provider", "model"},
    )

    pagesProcessed = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Namespace: "aidispatcher",
            Name:      "pages_processed_total",
            Help:      "Total pages processed by result (success, dlq)",
        },
        []string{"result"},
    )

    retriesTotal = prometheus.NewCounter(
        prometheus.CounterOpts{
            Namespace: "aidispatcher",
            Name:      "retries_total",
            Help:      "Total number of page retries",
        },
    )

    pagesProcessedAttr = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Namespace: "aidispatcher",
            Name:      "pages_processed_attributes_total",
            Help:      "Pages processed, labeled by result, source and fast flag",
        },
        []string{"result", "source", "fast"},
    )

    retriesAttr = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Namespace: "aidispatcher",
            Name:      "retries_attributes_total",
            Help:      "Retries labeled by source and fast flag",
        },
        []string{"source", "fast"},
    )

    breakerEvents = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Namespace: "aidispatcher",
            Name:      "breaker_events_total",
            Help:      "Circuit breaker events by provider, model and action",
        },
        []string{"provider", "model", "action"},
    )

    queueDepth = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{
            Namespace: "aidispatcher",
            Name:      "queue_depth",
            Help:      "Queue depth gauges for stream, delayed and dlq",
        },
        []string{"type"},
    )
)

// Init registers collectors.
func Init() {
    prometheus.MustRegister(providerReqs, providerLatency, pagesProcessed, retriesTotal, breakerEvents, queueDepth, pagesProcessedAttr, retriesAttr)
}

// Handler returns the http.Handler for /metrics
func Handler() http.Handler { return promhttp.Handler() }

func ObserveProvider(provider, model, result string, dur time.Duration) {
    providerReqs.WithLabelValues(provider, model, result).Inc()
    providerLatency.WithLabelValues(provider, model).Observe(dur.Seconds())
}

func IncProcessed(result string) { pagesProcessed.WithLabelValues(result).Inc() }
func IncRetry()                  { retriesTotal.Inc() }
func BreakerOpened(provider, model string) { breakerEvents.WithLabelValues(provider, model, "opened").Inc() }
func BreakerClosed(provider, model string) { breakerEvents.WithLabelValues(provider, model, "closed").Inc() }

func SetQueueDepth(kind string, v int64) { queueDepth.WithLabelValues(kind).Set(float64(v)) }

func IncProcessedAttr(result, source string, fast bool) {
    pagesProcessedAttr.WithLabelValues(result, source, boolToStr(fast)).Inc()
}

func IncRetryAttr(source string, fast bool) { retriesAttr.WithLabelValues(source, boolToStr(fast)).Inc() }

// IncRefusal tracks content refusal events by provider and model
func IncRefusal(provider, model string) {
    providerReqs.WithLabelValues(provider, model, "content_refused").Inc()
}

func boolToStr(b bool) string { if b { return "true" }; return "false" }
