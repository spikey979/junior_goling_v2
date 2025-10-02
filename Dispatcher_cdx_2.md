# AI Dispatcher – Integracija v2 (120s request, 5m job SLA)

Ovaj dokument definira konkretne korake za integraciju Dispatchera s novim vremenskim ograničenjima: maksimalno trajanje jednog AI requesta 120 sekundi i maksimalno trajanje cijelog posla (sve stranice jednog dokumenta) 5 minuta. Vrijednosti moraju biti konfigurabilne kroz `internal/config/env.go`.

## Ciljevi i promjene
- Request-level timeout: 120s po AI pozivu (konfigurabilno `REQUEST_TIMEOUT`).
- Document-level timeout: 5m za cijeli job (konfigurabilno `DOCUMENT_PROCESSING_TIMEOUT`).
- Orchestrator deterministički finalizira posao na timeout (AI + MuPDF fallback za nedovršene stranice).
- Dispatcher tretira timeout kao transient (kao 429) i radi failover bez blokiranja.
- Sve postavke centralizirane u `env.go` s razumnim defaultovima i mogućnošću override‑a ENV varijablama.

## ENV varijable (ključne)
- `DOCUMENT_PROCESSING_TIMEOUT=5m` – maksimalno vrijeme obrade cijelog dokumenta.
- `REQUEST_TIMEOUT=120s` – maksimalno vrijeme pojedinog AI poziva.
- `OPENAI_TIMEOUT` / `ANTHROPIC_TIMEOUT` – opcionalni override po provideru (ako nije zadano, nasljeđuje `REQUEST_TIMEOUT`).
- (postojeće) `PAGE_TOTAL_TIMEOUT=120s` – zaštitni limit po stranici u workeru (može ostati kao guardrail, ali SLA upravlja Orchestrator).

Napomena: Ako se `DOCUMENT_PROCESSING_TIMEOUT` eksplicitno ne postavi, default je 5m. Ako se `REQUEST_TIMEOUT` ne postavi, default je 120s.

## Promjene u kodu (precizno po modulima)

### 1) `internal/config/env.go`
- Dodati novu sekciju konfiguracije za Orchestrator ili proširiti postojeću strukturu:

```go
// OrchestratorConfig drži postavke vezane uz procesiranje cijelog dokumenta
type OrchestratorConfig struct {
    DocumentProcessingTimeout time.Duration
}

// U glavnoj Config strukturi
type Config struct {
    Logging     LoggingConfig
    Axiom       AxiomConfig
    Providers   ProvidersConfig
    Worker      WorkerConfig
    Queue       QueueConfig
    Image       ImageOptions
    Orchestrator OrchestratorConfig // NEW
}

// U FromEnv()
cfg.Orchestrator = OrchestratorConfig{
    DocumentProcessingTimeout: parseDuration(getEnv("DOCUMENT_PROCESSING_TIMEOUT", "5m"), 5*time.Minute),
}

// U Worker defaultima podesiti novi default request timeout na 120s
cfg.Worker = WorkerConfig{
    // ...
    RequestTimeout:   parseDuration(getEnv("REQUEST_TIMEOUT", "120s"), 120*time.Second),
    OpenAITimeout:    parseDuration(getEnv("OPENAI_TIMEOUT", ""), 0),
    AnthropicTimeout: parseDuration(getEnv("ANTHROPIC_TIMEOUT", ""), 0),
    // ostalo ne mijenjati
}

// Nakon učitavanja worker timeouts, zadržati fallback logiku:
if cfg.Worker.OpenAITimeout <= 0 { cfg.Worker.OpenAITimeout = cfg.Worker.RequestTimeout }
if cfg.Worker.AnthropicTimeout <= 0 { cfg.Worker.AnthropicTimeout = cfg.Worker.RequestTimeout }
```

Bez promjene naziva postojećih polja – samo dodati `Orchestrator` i ažurirati default `REQUEST_TIMEOUT` na 120s.

### 2) Orchestrator – dokument-level nadzor i dovršetak
Lokacije: `internal/orchestrator/orchestrator.go` (i/ili gdje je implementiran pipeline za AI)

- U funkciji koja pokreće obradu (npr. `ProcessJobForAI`):
  - Stvoriti `context.WithTimeout(ctx, cfg.Orchestrator.DocumentProcessingTimeout)` za dokument.
  - Pokrenuti goroutinu `monitorJobCompletion` koja:
    - periodički čita status (Redis),
    - na timeout: označi sve neobrađene stranice MuPDF fallbackom, agregira rezultate i finalizira job (`status=success`, `timeout_occurred=true`),
    - pozove `CancelJob(jobID)` (flag u Redis‑u) kako bi worker prestao s obradom istog posla.

Primjer (sažetak):
```go
docCtx, cancel := context.WithTimeout(ctx, o.cfg.Orchestrator.DocumentProcessingTimeout)
defer cancel()

go o.monitorJobCompletion(docCtx, jobID, totalPages)
```

- Pre‑pohrana MuPDF teksta prije enqueue‑a AI stranica u Redis (`job:{id}:mupdf:{page}`) radi brzog fallbacka.
- Callback handleri:
  - `/internal/page_done`: spremi AI tekst, ažuriraj `pages_done` i `progress`, provjeri dovršetak.
  - `/internal/page_failed`: učitaj i spremi MuPDF fallback tekst, inkrementiraj `pages_failed`, provjeri dovršetak.

### 3) Dispatcher – request timeout i failover
Lokacija: `internal/dispatcher/worker.go`

- Za svaki AI poziv koristiti `context.WithTimeout(..., cfg.Worker.RequestTimeout)`; default 120s.
- Ako je `context.DeadlineExceeded`: tretirati kao transient (ekvivalent 429) za failover.
- Sekvenca pokušaja po stranici:
  1) Primarni provider + primarni model
  2) Primarni provider + sekundarni model (na 429/5xx/timeout)
  3) Sekundarni provider + primarni model (na 429/5xx/timeout)
  4) Ako sve padne → `/internal/page_failed` (bez odgođenog retrya u strogoj SLA konfiguraciji)
- Lokalni per‑model semafor (`MAX_INFLIGHT_PER_MODEL`) + Redis circuit breaker ostaju kako jesu (non‑blocking: ne čekati na cooldown, odmah probaj drugu opciju).

### 4) Job cancel signal (opcionalno, preporučeno)
- U Redis‑u postaviti ključ `job:{id}:cancelled=1` na doc timeout; worker prije obrade svakog payload‑a provjeri flag i preskoči dalje (ack i nastavi na iduću poruku).
- TTL 1–2 sata radi čišćenja.

### 5) Status, agregacija i storage
- Status po poslu u Redis‑u (`job:{id}:status`, `metadata` polja: `total_pages`, `pages_done`, `pages_failed`, `timeout_occurred`).
- Per‑page pohrana teksta: `page:{job}:{n}:text`, `page:{job}:{n}:source` (`ai|mupdf_fallback`).
- Na dovršetak: agregacija AI + MuPDF; lokalni file (`RESULT_DIR`) za upload izvor, S3 za API izvor (postojeće ponašanje).

## Metrike i logiranje
- Metrike (Prometheus):
  - `aidispatcher_request_timeouts_total{provider,model}` – broj request timeouta.
  - `aidispatcher_document_timeouts_total` – broj doc‑level timeouta.
  - `aidispatcher_pages_processed_total{result}` – success|mupdf_fallback.
  - `aidispatcher_breaker_events_total{provider,model,action}` – opened|closed.
- Logovi (zerolog): dodati polja `job_id`, `page_id`, `provider`, `model`, `attempt`, `latency_ms`, `timeout=true/false`, `failover=true/false`.

## Test plan
- Jedinično:
  - `env.go`: parsiranje `DOCUMENT_PROCESSING_TIMEOUT` i `REQUEST_TIMEOUT` defaulti.
  - Worker: `callAIWithTimeout` – timeout mapiran u transient.
  - Circuit breaker: open/half‑open/close s eksponencijalnim backoffom.
- Integracijsko:
  - Dokument s 10–20 stranica: ubrizgati 429/timeout na primarnom modelu → provjeriti failover i da se dokument završi unutar 5m.
  - Doc‑level timeout: usporiti providere tako da se dosegne 5m → provjera da orchestrator finalizira s MuPDF fallbackom i postavi `timeout_occurred=true`.

## Migracija i rollout
1) Ažurirati `env.go` (dodati `OrchestratorConfig` i default `REQUEST_TIMEOUT=120s`).
2) Orchestrator: uvesti dokument-level timeout i monitor; pre‑pohrana MuPDF teksta.
3) Dispatcher: provući `cfg.Worker.RequestTimeout` kroz sve AI pozive; potvrditi failover flow.
4) Dodati/azurirati ENV u `.env.example` (`DOCUMENT_PROCESSING_TIMEOUT=5m`, `REQUEST_TIMEOUT=120s`).
5) Kratki E2E test batch (5×10 stranica) s ograničenjem vremena – potvrditi da nema prekoračenja SLA.

## Napomene o kompatibilnosti
- Postojeći `PAGE_TOTAL_TIMEOUT` (120s) ostaje kao soft guardrail; SLA je efikasno zadan Orchestrator timeoutom 5m.
- Ako se u budućnosti želi diferencirati SLA po korisniku/poslu, `DOCUMENT_PROCESSING_TIMEOUT` može se prepisati iz requesta (uz zaštitu maksimalne vrijednosti u `env.go`).

***

Ovaj plan precizira minimalne izmjene: dodavanje `DOCUMENT_PROCESSING_TIMEOUT` u konfiguraciju i postavljanje default `REQUEST_TIMEOUT=120s`, te orkestracijski nadzor koji garantira dovršetak svakog dokumenta unutar 5 minuta uz MuPDF fallback za preostale stranice.

