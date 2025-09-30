# Plan: Orkestrator + AI Dispatcher s Redis queue, rate‑limitom i failoverom

Ovaj dokument definira arhitekturu, pravila i korake implementacije servisa za obradu dokumenata uz AI engine‑e (npr. OpenAI i Anthropic), s globalnim ograničenjima RPM/TPM, timeoutima i failover logikom. Plan je pisan tako da drugi Codex session može preuzeti rad bez dodatnih objašnjenja.

## Ciljevi
- Poštovati zadane limite: globalni per‑provider/per‑model RPM i TPM.
- Izbjeći burstove: obrada ide kroz job queue, ne paralelnim ad‑hoc slanjem.
- Failover: na 429/5xx/timeout s primarnog providera pokušati sekundarni.
- Timeout: maksimalno trajanje pojedinog requesta (npr. 60 s) tretirati kao grešku.
- Održivost: backpressure, retry s backoffom, DLQ, idempotency, metrike.
- Fleksibilnost: moći raditi kao jedan servis (monolit) ili dva odvojena servisa.

## Visokorangirani pregled
- Orchestrator servis odlučuje za svaku stranicu ide li na MuPDF (lokalni ekstraktor) ili na AI. Za stranice koje idu na AI, enqueuea job u Redis Stream.
- AI Dispatcher (worker procesi) čitaju jobove, koordiniraju se kroz globalni rate limiter u Redis‑u, pozivaju AI provider i zapisuju rezultat. Na 429/5xx/timeout failoveraju na sekundarnog providera.
- Binarni sadržaj se NE šalje kroz Redis. U job payload ide samo referenca (npr. `file://`, `s3://`), plus metapodaci.

## Komponente
- Orchestrator (postojeći servis + nadogradnje)
  - Radi MuPDF ekstrakciju i donosi odluku per stranica.
  - Za AI stranice enqueuea job u `Redis Streams` + prati status i agregira rezultate.
- Queue sloj: Redis Streams (XADD/XREADGROUP) + ZSET za odgođene retryeve.
- AI Dispatcher worker(i)
  - Concurrency kontroliran lokalno; globalno ograničen rate limiterom (Redis token bucket per provider/model).
  - Timeouts, retry/backoff, DLQ, idempotency.
  - Failover između primarnog i sekundarnog providera.
- Rate limiter: centraliziran u Redis‑u (token bucket/leaky bucket), ključevi po `provider:model` za RPM i TPM.
- Pohrana: rezultati i statusi u DB/objektnoj pohrani; Redis može držati kratkotrajni status/cache.

## Tok podataka (happy path)
1. Orchestrator procesuira dokument, odluči koje stranice idu na AI.
2. Za svaku AI stranicu enqueuea job u `jobs:ai:pages` (Redis Stream) s referencom na sadržaj i preferencijom providerâ.
3. Worker uzima job (XREADGROUP), rezervira RPM/TPM na primarnom provideru, poziva AI s `context.WithTimeout`.
4. Rezultat sprema u storage, ažurira status (`job:{id}:status`), ACK‑a poruku (XACK).

## Tok podataka (greške i failover)
- Ako poziv na primarnom završi s 429/5xx/network/timeout → pokuša sekundarni provider (uz novu rezervaciju RPM/TPM i vlastiti timeout).
- Ako oba pokušaja padnu, job se vraća u red s eksponencijalnim backoffom (ZSET odgoda). Nakon `N` pokušaja ide u DLQ.
- Ako nema budžeta na primarnom: politika je konfigurabilna (instant failover na sekundarni ako ima budžet; ili sticky na primarni s kratkim requeueom).

## Payload i protokol (Redis)
- Stream: `jobs:ai:pages`
- Consumer grupa: `workers:images`
- Polja poruke (JSON u vrijednosti ili hash polja):
  ```json
  {
    "job_id": "uuid",
    "doc_id": "uuid",
    "page_id": 12,
    "content_ref": "file:///mnt/docs/123/page_12.png", 
    "engine_pref": {"primary": "openai", "secondary": "anthropic"},
    "model_hint": "gpt-4o-mini", 
    "opts": {"vision_mode": "ocr"},
    "idempotency_key": "doc:123:page:12:v1",
    "attempt": 1
  }
  ```
- Odgođeni retry: ZSET `jobs:ai:pages:delayed` (score = unix_ts_execute_at). Periodični „mover” prebacuje spremne u `jobs:ai:pages`.
- Status: `job:{job_id}:status` (hash) ili zapisi u DB; rezultati u DB/objektnoj pohrani.

## Rate limiting (RPM/TPM)
- Ključevi po `provider:model`:
  - `rl:{provider}:{model}:rpm` (token bucket za zahtjeve)
  - `rl:{provider}:{model}:tpm` (token bucket za tokene)
- Refill: kontinuiran (`rpm/60` i `tpm/60` tokena u sekundi). Implementirati atomikom (Lua skripte) ili EVALSHA cache.
- Rezervacija:
  1) Procijeni potrebne tokene za job (konzervativno, s marginom npr. +15%).
  2) Pokušaj atomsku rezervaciju requests+tokens.
  3) Ako nema dovoljno, odluči: čekati (kratko) ili requeue; ili probati sekundarni provider.
- Evidencija stvarne potrošnje: logirati `tokens_used` iz provider response‑a, kalibrirati procjenu po tipu posla.

## Adaptivno ograničavanje bez TPM brojanja
- Motivacija: AI račun dijele drugi servisi; ne znamo real‑time potrošnju tokena ni točan RPM budžet u svakom trenutku.
- Pristup: umjesto TPM procjene, koristimo kombinaciju lokalnog concurrency limita + 429‑aware adaptivnog backoffa per `provider:model`.
- Mehanizmi:
  - Lokalni concurrency cap per worker i per model (npr. `MAX_INFLIGHT_{PROVIDER}_{MODEL}`) da spriječimo burstove.
  - 429 feedback petlja: na `429` (ili specifične rate‑limit greške) postavi „cooldown” za taj model (npr. 30–120s, eksponencijalno raste po uzastopnim greškama).
  - Circuit breaker per model: ključevi `cb:{provider}:{model}` u Redis‑u s state‑om `closed/half_open/open` i `retry_at` timestampom.
  - EWMA/Sliding-window: opcionalno mjeriti uspjehe/neuspjehe i dinamički prilagođavati dopuštenu paralelnost po modelu.
- Posljedica: ne računamo TPM; oslanjamo se na „samoregulaciju” kroz 429 signale i hladne periode (cooldown), uz minimalno bazni RPM cap ako je poznat.

## Provider routing i failover
- `AIClient` sučelje: `Process(ctx, Job) (Result, Usage, error)`; implementacije `OpenAIClient`, `AnthropicClient`.
- Politika:
  - Pokušaj `primary` s timeoutom.
  - Na 429/5xx/network/timeout → pokušaj `secondary` (novi timeout, nova rezervacija budžeta).
  - 4xx validacijske greške su fatalne (nema failovera, job u grešku/DLQ).
- Idempotency: jedinstveni `idempotency_key` sprječava duplu obradu; koristiti isti kroz failover.

## Multi‑model fallback unutar providera
- Konfiguracija per provider: tri modela
  - `primary_model` (npr. za OpenAI: `gpt-4.1`)
  - `secondary_model` (npr. `gpt-4o`)
  - `fast_model` (npr. `gpt-4.1-mini`) – koristi se samo na izričit zahtjev Orchestratora.
- Algoritam odabira modela:
  - Ako job ima `force_fast=true` → koristi `fast_model` tog providera (bez model fallbacka, osim na transient greške kada se može probati i `secondary_model` fast klase, ako je definiran).
  - Inače: kreni s `primary_model`; na `429` ili cooldown `open` pređi na `secondary_model` istog providera.
  - Ako su i `primary_model` i `secondary_model` u cooldownu ili vraćaju 429 → pređi na sekundarnog providera (vidi sekciju „Provider routing i failover”).
- Cooldown logika:
  - Na svaki `429` za `provider:model` u Redis upisati `cb:{provider}:{model} = open` s `retry_at = now + backoff(attempts)`.
  - U `half_open` režimu dopustiti 1 probni zahtjev; uspjeh zatvara breaker (reset), neuspjeh vraća u `open` s većim backoffom.

## Timeout i SLA
- `REQUEST_TIMEOUT` (globalni default, npr. 60s) + per‑provider override (`OPENAI_TIMEOUT`, `ANTHROPIC_TIMEOUT`).
- `JOB_MAX_ATTEMPTS` (npr. 3) i opcionalni `JOB_SLA_MAX_AGE` (npr. 5 min) za rezanje dugotrajnih ciklusa.
- Implementacija u workeru: `context.WithTimeout` oko svakog provider poziva; `deadline exceeded` se tretira kao transient i aktivira failover ili retry/backoff.

## Retry, backoff i DLQ
- Retry na transient greške: eksponencijalni backoff s jitterom.
- Nakon `JOB_MAX_ATTEMPTS` → DLQ: `jobs:ai:pages:dlq` (Redis Stream) s razlogom i zadnjom greškom.
- Kod requeued jobova inkrementirati `attempt` i voditi `last_provider`.

## Metrike i logiranje
- Metrike (Prometheus preporuka): queue depth, in‑flight, success/fail, retry rate, DLQ count, latencije, 4xx/5xx providera, iskorištenost RPM/TPM po `provider:model`.
- Logovi (strukturirani): `job_id`, `provider`, `model`, `attempt`, `tokens_used`, `latency_ms`, `error_class`, `failover=true/false`.

## Logiranje (lokalni file + rotacija + Axiom)
- Logger: `zerolog` s JSON outputom; opcionalno console pretty za dev.
- Lokacija: direktorij `logs/` i datoteka `logs/aidispathcher.log`.
- Rotacija/kompresija: `lumberjack`
  - Prag veličine: konfigurabilan (npr. 50–100 MB).
  - Kompresija: uključena (`.gz`).
  - Zadržavanje: posljednjih 10 kompresiranih fajlova (`MaxBackups=10`).
- Leveli: `debug`, `info`, `warn`, `error` (standardni zerolog). Konfigurabilno:
  - Dev: `debug`.
  - Prod: `warn` i `error` (global level `warn`).
- Slanje na Axiom:
  - Integracija putem Axiom Go SDK; batchiranje i periodični flush.
  - Filtriranje: ne slati `debug` na Axiom; slati `info`/`warn`/`error`.
  - Dataseti: jedan glavni dataset s prefiksom okruženja (npr. `dev_aidispatcher`).
- Implementacijske napomene:
  - Kreirati writer chain: file (lumberjack) + Axiom writer (+ console u dev‑u).
  - Zatvaranje: na shutdown flushati Axiom i zatvoriti rotator (ako je potrebno).

## Konfiguracija preko `env.go` (bez .env file)
- Centralni `env.go` drži sve postavke s razumnim defaultovima i čitanjem `os.Getenv` kada postavljeno:
  - Logiranje:
    - `LOG_LEVEL` (default `info`), `LOG_PRETTY` (dev),
    - `LOG_FILE=logs/aidispathcher.log`,
    - `LOG_MAX_SIZE_MB` (npr. 100), `LOG_MAX_BACKUPS=10`, `LOG_MAX_AGE_DAYS=30`, `LOG_COMPRESS=true`.
  - Axiom:
    - `AXIOM_API_KEY`, `AXIOM_ORG_ID`, `AXIOM_DATASET` (npr. `dev` → generiramo `dev_aidispatcher`).
    - `SEND_LOGS_TO_AXIOM=1|0` za enable/disable.
  - Provideri i modeli: kako je definirano u sekciji multi‑model fallback.
  - Worker postavke: concurrency, timeouts, backoff, itd.
- `env.go` je dio koda (bez .env datoteka); prod vrijednosti dolaze iz pravih env varijabli okruženja ili se predefiniraju unutar `env.go` defaultova.

## Konfiguracija (ENV)
- Provideri i prioritet:
  - `PRIMARY_ENGINE=openai|anthropic`
  - `SECONDARY_ENGINE=anthropic|openai`
  - Per‑provider modeli:
    - OpenAI: `OPENAI_PRIMARY_MODEL=gpt-4.1`, `OPENAI_SECONDARY_MODEL=gpt-4o`, `OPENAI_FAST_MODEL=gpt-4.1-mini`
    - Anthropic: `ANTHROPIC_PRIMARY_MODEL=claude-3-5-sonnet`, `ANTHROPIC_SECONDARY_MODEL=claude-3-opus` (primjer), `ANTHROPIC_FAST_MODEL=claude-3-haiku`
- Timeout/SLA:
  - `REQUEST_TIMEOUT=60s`, `OPENAI_TIMEOUT=60s`, `ANTHROPIC_TIMEOUT=60s`
  - `JOB_MAX_ATTEMPTS=3`, `RETRY_BASE_DELAY=2s`, `RETRY_JITTER=200ms`, `RETRY_BACKOFF_FACTOR=2.0`
- Limiti (primjeri):
  - (Opcionalno) statički RPM cap per `provider:model`, ako poznat: `OPENAI_GPT41_RPM=...`, `OPENAI_GPT4O_RPM=...`, `ANTHROPIC_SONNET_RPM=...`
- Worker:
  - `WORKER_CONCURRENCY=8`, `QUEUE_POLL_INTERVAL=100ms`
  - (Opcionalno) lokalni paralelizam po modelu: `MAX_INFLIGHT_OPENAI_GPT41=4`, `MAX_INFLIGHT_OPENAI_GPT4O=4`, ...
- Storage/queue:
  - `REDIS_URL=redis://...`, `RESULT_STORE=postgres|s3`, `FILES_BASE=file:///mnt/docs`

## Docker Compose (skica)
```yaml
version: "3.9"
services:
  redis:
    image: redis:7
    ports: ["6379:6379"]
  app: # monolit (orchestrator + dispatcher)
    build: .
    env_file: [.env]
    depends_on: [redis]
    volumes:
      - ./data:/mnt/docs
  # Alternativa micro‑servisi:
  orchestrator:
    build: .
    command: ["/bin/orchestrator"]
    env_file: [.env]
    depends_on: [redis]
  dispatcher:
    build: .
    command: ["/bin/dispatcher"]
    env_file: [.env]
    depends_on: [redis]
    deploy:
      replicas: 2 # skaliranje dispatchera
```

## Struktura koda (Go, predložak)
- `cmd/orchestrator/main.go` – starta orchestrator HTTP/CLI, MuPDF odluka, enqueue u Redis.
- `cmd/dispatcher/main.go` – worker loop, limiter, retry, failover, result writeback.
- `internal/queue/redis.go` – Streams + delayed ZSET utili.
- `internal/limiter/redis.go` – token bucket (RPM/TPM) s Lua atomikom.
- `internal/ai/client.go` – `AIClient` sučelje.
- `internal/ai/openai.go`, `internal/ai/anthropic.go` – provider implementacije.
- `internal/worker/worker.go` – orchestracija pokušaja, timeouti, idempotency, metrika hookovi.
- `internal/model/types.go` – `Job`, `Result`, `Usage`, error klasifikacija.

## Pseudokôd: worker loop i failover
```go
for msg := range stream.ConsumerGroup("jobs:ai:pages", "workers:images") {
  job := decode(msg)
  if job.Attempt >= cfg.JobMaxAttempts { moveToDLQ(job, "max_attempts"); ack(msg); continue }

  primaryProv := providerFrom(cfg.Primary)
  secondaryProv := providerFrom(cfg.Secondary)

  // Kandidat modeli unutar primarnog providera
  models := selectModels(primaryProv, job.ForceFast) // npr. [primary, secondary] ili [fast]

  // pokuša primarni
  tried := false
  for _, m := range models {
    if isOpenBreaker(primaryProv, m) { continue }
    if !allowInflight(primaryProv, m) { continue }
    usage, err := callWithTimeout(ctx, primaryProv, m, job)
    tried = true
    if err == nil { recordSuccess(primaryProv, m); saveResult(job, usage); ack(msg); break }
    if isFatal(err) { saveError(job, err); ack(msg); break }
    if isRateLimited(err) { openBreaker(primaryProv, m); continue }
    // ostale transient greške → probaj drugi model ili failover
  }
  if acked(msg) { continue }

  // failover na sekundarni
  models2 := selectModels(secondaryProv, job.ForceFast)
  for _, m := range models2 {
    if isOpenBreaker(secondaryProv, m) { continue }
    if !allowInflight(secondaryProv, m) { continue }
    usage, err := callWithTimeout(ctx, secondaryProv, m, job)
    if err == nil { recordSuccess(secondaryProv, m); saveResult(job, usage); ack(msg); break }
    if isFatal(err) { saveError(job, err); ack(msg); break }
    if isRateLimited(err) { openBreaker(secondaryProv, m); continue }
  }
  
  // requeue s backoffom
  job.Attempt++
  delay := backoff(job.Attempt)
  enqueueDelayed(job, now+delay)
  ack(msg)
}
```

## Integracija s postojećim servisom
- Iz postojeće logike (npr. `ai_images_processor.go`) izdvojiti AI pozive u `internal/ai/*` i korištenje zamijeniti enqueuom + (u monolitu) lokalnim worker poolom.
- Odluka MuPDF vs AI ostaje u orchestratoru. Za AI stranice koristimo queue payload s referencama na datoteke.
- Agregacija rezultata (MuPDF + AI) i dalje živi u orchestratoru.

## Testiranje
- Jedinično: limiter (RPM/TPM), backoff, idempotency, klasifikacija grešaka, timeouti.
- Integracijsko: fake AI provider s injekcijama 429/5xx/timeout; testirati 5×20 stranica bez prekoračenja limita.
- Load: mjeriti throughput, latenciju, queue depth, iskorištenost RPM/TPM, stopu failovera.

## Migracija
1. Faza 1: Orchestrator umjesto direktnih AI poziva enqueuea poslove.
2. Faza 2: Uključiti workere (u istom procesu – monolit) i validirati metrike.
3. Faza 3: Po potrebi izdvojiti dispatcher u zaseban binar/servis, skalirati replike.

## Otvorena pitanja / odluke
- Politika kad primarni nema budžeta: instant failover vs sticky na primarni? (konfig)
- Procjena tokena po tipu dokumenta/stranice i sigurnosna margina.
- Persistencija statusa: Redis vs DB (ovisno o postojećoj arhitekturi).

## TODO (iterativno ćemo označavati)
- [ ] Definirati finalni payload schema i mapu modela (logički → provider‑specifično)
- [ ] Implementirati adaptivni limiter (concurrency + 429 cooldown)
- [ ] Implementirati Redis Streams queue + delayed ZSET mover
- [ ] Izvući AI klijente (OpenAI/Anthropic) i normalizirati usage
- [ ] Worker loop: timeouti, multi‑model fallback, provider failover, retry/DLQ, idempotency
- [ ] Integrirati s orchestratorom (enqueue + rezultat agregacija)
- [ ] Metrike (Prometheus) i strukturirano logiranje
- [ ] Docker Compose targeti (monolit i micro varijante)
- [ ] E2E test batcha 5×20 bez prekoračenja RPM/TPM

## Promjene i napredak (Changelog)
- [x] Dodan bazni Go modul i entrypoint:
  - `go.mod`
  - `cmd/app/main.go` – starta HTTP Orchestrator i Dispatcher (opcionalno) + graceful shutdown
- [x] Logger i konfiguracija:
  - `internal/logger/logger.go` – zerolog + lumberjack rotacija + Axiom forwarding (batched)
  - `internal/config/env.go` – centralna konfiguracija (logiranje, Axiom, provider modeli, worker, queue)
- [x] Queue (PoC):
  - `internal/queue/redis.go` – minimalna implementacija preko Redis LIST (LPUSH/BRPOP)
  - Napomena: prelazak na Redis Streams ostaje TODO
- [x] Orchestrator (PoC):
  - `internal/orchestrator/orchestrator.go` – rute `/health`, `POST /process_file_junior_call`, `GET /progress_spec/{id}`
  - Enqueue AI posla s minimalnim payloadom (stranice/heuristike TBD)
  - Interni in‑memory status store (TODO: prebaciti na Redis status)
- [x] Dispatcher (PoC):
  - `internal/dispatcher/worker.go` – worker pool čita s queuea i logira „obradu” (stub)
  - Callback: nakon obrade poziva Orchestrator `/internal/job_done?job_id=...` za markiranje uspjeha
  - Novo: per‑stranica obrada s globalnim timeoutom od 2 minute i per‑request timeoutom 60s; failover redoslijed:
    - Primarni provider + primarni model
    - Ako 429 → sekundarni model istog providera, inače odmah sekundarni provider
    - Ako sekundarni provider ne uspije → Orchestratoru javiti fail stranice (MuPDF fallback)
  - Callback rute: `/internal/page_done` i `/internal/page_failed` s `job_id` i `page_id`
  - Orchestrator prima AI tekst po stranici (JSON body) i sprema u Redis; za failed stranice radi MuPDF extraction i agregira rezultat
- [x] Docker: 
  - Ažuriran `Dockerfile` (multi‑stage build) i `docker-compose.yml` (dodana `redis` usluga i `app`)
- [x] Logiranje: planirano i opisano; output ide u `logs/aidispathcher.log` uz rotaciju/kompresiju.
- [x] Dashboard (PoC):
  - `internal/web/web.go` – minimalni login (env `WEB_USERNAME`/`WEB_PASSWORD`), dashboard i forme
  - `web/templates/{login.html,dashboard.html}` – osnovni UI; proces pokreće POST na `/process_file_junior_call`

## Sljedeći koraci
- [x] Orchestrator: dodan skeleton MuPDF selekcije (heuristika, PoC) i enqueuing per-stranica.
- [x] Orchestrator: status/progres prebačen na Redis store (umjesto in-memory) i povezano s cancel/job_done.
- [x] Orchestrator: dodan brojač stranica (pdfcpu) s HTTP/S3 podrškom; fallback na default ako preuzimanje nije moguće.
- [x] Orchestrator: dodan `fast_upload` flag – preskače AI potpuno i forsira MuPDF putanju.
- [x] Orchestrator: dodana MuPDF ekstrakcija teksta po stranici (go-fitz) za fallback; agregirani tekst se sprema u S3 i URL dodaje u status.
- [ ] Orchestrator: integrirati napredne heuristike (tekst vs slika) i `content_ref` za page artefakte na S3.
- [ ] Dispatcher: dodati adaptivni limiter i multi‑model fallback (primary→secondary; fast na zahtjev).
  - [x] Zamijenjeni stubovi za OpenAI/Anthropic realnim HTTP pozivima; mapiranje 429 → rate_limited
- [ ] Queue: migrirati na Redis Streams + consumer groups, dodati delayed ZSET mover.
- [ ] Status: zamijeniti in‑memory status Redis‑om i izraditi rezultat agregaciju (S3/DB).
- [ ] Axiom: dodati strukturirana polja (job_id, provider, model, attempt, duration).
- [ ] Health i metrics: dodati `/metrics` (Prometheus) i health detalje.

---

Napomena: Binarne datoteke ne stavljati u Redis. Koristiti samo reference (lokalni `file://` ili objektna pohrana poput S3) i hash za integritet.

## Orchestrator (GhostServer API, S3, MuPDF selekcija)

Ovaj odjeljak definira hibridni Orchestrator koji prima zahtjeve od GhostServer‑a (POST `/process_file_junior_call`), odlučuje koje stranice obraditi lokalno (MuPDF) a koje preko AI Dispatchera, ažurira progres i pohranjuje rezultate. Orchestrator i Dispatcher rade u istom servisu, ali su jasno odvojeni paketi/binariji za kasniji split.

### API Endpoints
- POST `/process_file_junior_call`: prihvaća zahtjev za obradu jednog dokumenta.
- GET `/progress_spec/{job_id_or_file_id}`: vraća status i progres obrade.
- GET `/health`, `/health_check`, `/status`: healthz.
 - POST `/webhook/cancel_job`: otkazivanje posla; zapis u Redis cancel set i ažuriranje statusa.
 - POST `/internal/page_done?job_id=&page_id=`: dispatcher šalje završenu AI stranicu (body: `{text,provider,model}`).
 - POST `/internal/page_failed?job_id=&page_id=`: dispatcher označi stranicu kao fail (orchestrator radi MuPDF fallback).

### Request schema (usklađeno s postojećim kodom)
- Polja (GhostServer + legacy):
  - `file_path` ili `file_url` (S3 key ili s3:// url)
  - `user_name` ili `user_id`
  - `password` (opcionalno, za enkriptirane dokumente)
  - `ai_prompt` (opcionalno override)
  - `ai_engine` vrijednosti: `OpenAIEngine` | `ClaudeEngine` | `JuniorEngine` (mapira se na `processing_mode` i `ai_provider`)
  - `text_only` (bool): forsira MuPDF text ekstrakciju bez AI/ocr
  - `processing_mode`, `ai_provider`, `options` (interno)

### Mapping ai_engine → provider/mode
- `JuniorEngine` → `processing_mode=bart_1`, `ai_provider=openai`
- `ClaudeEngine` → `processing_mode=bart_1`, `ai_provider=anthropic`
- `OpenAIEngine` → `processing_mode=bart_1`, `ai_provider=openai`
- default: `bart_1` + `openai`

### Tok za POST /process_file_junior_call
1. Validacija i normalizacija polja (uz sanitizirano logiranje bez `password` i punog `ai_prompt`).
2. Normalizacija S3 putanje: ako je samo key, prefiksati s bucketa (`s3://{bucket}/{key}`).
3. Generirati `job_id` (UUID), kreirati zapis u Redis‑u (status `queued`, meta: `file_path`, `file_url`).
4. Izvući `file_id` iz putanje i napraviti mapiranje `file_id → job_id` za praćenje progresa.
5. Enqueue u JobManager (ako postoji) ili fallback na direktnu obradu u goroutini; izračunati `queue_position` i grubo `ETA`.
6. Vratiti 201 s tijelom kompatibilnim s GhostServer‑om: `{status, job_id, message, estimated_time_seconds, queue_position, metadata}`.

### Obrada posla (u pozadini)
1. Preuzmi dokument sa S3 (ili radi na s3 referenci ako se koristi streaming/remote toolchain).
2. MuPDF analiza: ekstrakcija metapodataka i sadržaja po stranici (tekst, detekcija slika/lošeg teksta).
3. Selektor stranica:
   - Ako `text_only=true` → sve stranice kroz MuPDF tekst ekstrakciju, preskoči AI.
   - Inače po heuristici: stranice s visokim udjelom slike/lošeg teksta/diagrami → šalji na AI; ostalo MuPDF.
4. Za AI stranice: enqueue u Redis Stream `jobs:ai:pages` payload s:
   - `job_id`, `doc_id`/`file_id`, `page_id`/`range`, `content_ref` (npr. `s3://bucket/path/page_12.png` ili `file://...`), `engine_pref` (primary/secondary), `model_hint`, `idempotency_key`, `opts`.
   - Ako Orchestrator zahtijeva „fast” model, postaviti `force_fast=true` u payload.
5. Paralelno (lokalno) procesuirati MuPDF stranice; rezultati idu u privremenu pohranu.
6. Agregacija: čekati AI rezultate (poll/push), objediniti s MuPDF rezultatima u finalni tekst i zapisati u S3 (bez lokalne baze). Status metadata: `result_s3_url`, `result_text_len`.

## Pokretanje servisa (Docker Compose)
- Preduvjeti:
  - Docker i Docker Compose instalirani.
  - S3 pristup (čitanje originala i zapis rezultata): `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION`, `AWS_S3_BUCKET`.
  - AI ključevi (opcionalno ako se koristi AI): `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`.

- Pokretanje:
  - `docker compose up --build`
  - Servisi:
    - `redis`: Redis 7 na portu 6379
    - `app`: Orchestrator+Dispatcher na portu 8080

- Bitne varijable (mogu se postaviti u `docker-compose.yml` ili runtime):
  - HTTP/Worker:
    - `PORT=8080`
    - `RUN_DISPATCHER=1` (omogućuje workere u istom procesu)
    - `WORKER_CONCURRENCY` (default 8)
    - `REQUEST_TIMEOUT=60s`, `PAGE_TOTAL_TIMEOUT=120s`
  - Provideri/Modeli:
    - `PRIMARY_ENGINE=openai|anthropic`, `SECONDARY_ENGINE=anthropic|openai`
    - `OPENAI_PRIMARY_MODEL`, `OPENAI_SECONDARY_MODEL`
    - `ANTHROPIC_PRIMARY_MODEL`, `ANTHROPIC_SECONDARY_MODEL`
    - `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`
  - Redis/Queue:
    - `REDIS_URL=redis://redis:6379`
  - S3:
    - `AWS_S3_BUCKET`, `AWS_REGION`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`
  - Dashboard:
    - `WEB_USERNAME`, `WEB_PASSWORD`
  - Logovi:
    - `LOG_FILE=logs/aidispathcher.log`, `LOG_LEVEL=debug|info|warn|error`, `LOG_PRETTY=true|false`
  - Axiom (opcionalno):
    - `SEND_LOGS_TO_AXIOM=1`, `AXIOM_API_KEY`, `AXIOM_ORG_ID`, `AXIOM_DATASET`

- Brzi test (curl):
  - Kreiraj posao (S3 ulaz):
    - `curl -X POST http://localhost:8080/process_file_junior_call -H 'Content-Type: application/json' -d '{"file_path":"s3://<bucket>/path/file.pdf","user_name":"user@example.com"}'`
  - Fast upload (MuPDF only):
    - `curl -X POST http://localhost:8080/process_file_junior_call -H 'Content-Type: application/json' -d '{"file_path":"s3://<bucket>/path/file.pdf","user_name":"user@example.com","fast_upload":true}'`
  - Provjera progresa:
    - `curl http://localhost:8080/progress_spec/<job_id>`
  - Cancel:
    - `curl -X POST http://localhost:8080/webhook/cancel_job -H 'Content-Type: application/json' -d '{"job_id":"<job_id>","reason":"user cancel"}'`

## Struktura repozitorija (sažetak)
- `cmd/app/main.go`: entrypoint; starta HTTP server, Orchestrator, opcionalno Dispatcher workere.
- `internal/orchestrator/*`: API rute, selekcija, MuPDF text extraction (go-fitz), agregacija i S3 spremanje.
- `internal/dispatcher/worker.go`: per‑stranica obrada s timeoutima i failover logikom; šalje rezultat/fail Orchestratoru.
- `internal/queue/redis.go`: minimalni queue (LIST); TODO: migracija na Streams.
- `internal/store/status_redis.go`: Redis status store; `internal/store/page_redis.go`: per‑stranica tekst i agregacija.
- `internal/ai/*`: klijenti za OpenAI i Anthropic (HTTP pozivi, 429 mapiranje).
- `internal/logger/logger.go`: file rotacija + Axiom; `internal/config/env.go`: centralni config.
7. Ažurirati Redis status: `processing` → `success` ili `failed` + poruka. Emitirati progres (0–100%) po milestoneovima: preuzimanje, analiza, enqueuing AI, završetak MuPDF, pristizanje AI rezultata, spajanje, spremanje.

### Progres i status
- `GET /progress_spec/{id}` vraća strukturirani objekt sa statusom: `queued|processing|success|failed`, `progress` (%), `message`, `start_time`, `end_time`, `result` (ako postoji) i meta.
- Progres se izvlači iz Redis status strukture koju ažuriraju Orchestrator (MuPDF napredak) i Dispatcher (pošiljka AI rezultata).

### Hibridno pokretanje (monolit + opcionalni split)
- Jedan servis pokreće HTTP Orchestrator i Dispatcher workere u zasebnim gorutinama; konfiguracijska zastavica npr. `RUN_DISPATCHER=1|0`.
- Jasna paketna podjela omogućava kasnije izdvajanje u dva bina:
  - `cmd/orchestrator/main.go` (HTTP + MuPDF + enqueuing)
  - `cmd/dispatcher/main.go` (queue worker + rate‑limit + failover)
  - Dijele `internal/queue`, `internal/ai`, `internal/limiter`, `internal/logger`.

### S3 integracija
- Orchestrator koristi AWS SDK v2 za čitanje PDF‑a kada je ulaz `s3://bucket/key` (privremeno preuzimanje u lokalni `/tmp`).
- Potrebna konfiguracija: `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION`, `AWS_S3_BUCKET`.
- Preuzimanje/učitavanje stranica: generirati pre‑signed URL‑ove ili koristiti lokalni cache/buffer.
- Ne slati binarne sadržaje kroz Redis; koristiti `content_ref` prema S3 ili lokalnoj privremenoj datoteci (`s3://...#page=N`).

### Sigurnost i logiranje
- U logove ne zapisivati `password` i cijeli `ai_prompt`; logirati duljinu prompta i indikator prisutnosti lozinke.
- Slati logove (info/warn/error) na Axiom; debug ostaje lokalno.

### Plan implementacije Orchestratora (zadaci)
1. Dodati HTTP rutu `POST /process_file_junior_call` s validacijom, normalizacijom S3 i kreiranjem posla u Redis‑u.
2. Implementirati `ProgressHandler` kompatibilan s `GET /progress_spec/{id}` (job_id ili file_id).
3. Dodati MuPDF modul: analiza + tekst ekstrakcija per stranica.
4. Dodati „selektor” stranica (heuristike, konfigurabilni pragovi) i enqueuing AI stranica u `jobs:ai:pages` s odgovarajućim poljima.
5. Agregacija rezultata i spremanje u S3/DB; ažuriranje statusa i napretka.
6. Uvezati s Dispatcherom (in‑process) i logiranjem/metrikama; omogućiti `RUN_DISPATCHER` prekidač.
7. Testni scenariji: dokument s miksom teksta/slika, `text_only=true`, 429 simulacije, failover model/providera, restart otpornost.
