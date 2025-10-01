# AI Dispatcher – Usklađeni Plan (CDX v1)

Ovaj plan usklađuje DISPATCHER.md s postojećim kodom i ciljem da svaki dokument bude obrađen unutar maksimalnog vremena (SLA), bez čekanja 5 minuta. Fokus je na determinističnom dovršavanju (AI + MuPDF fallback), dokument-level timeoutu i neblokirajućim workerima.

## Ciljevi
- Obrada svakog dokumenta unutar dokument-level SLA, npr. 3 minute (konfigurabilno).
- Non‑blocking worker petlja: inline failover, bez čekanja na providera/model u cooldownu.
- Deterministično dovršavanje: na timeout finalizirati s parcijalnim rezultatima i MuPDF fallbackom.
- Idempotency i sigurno ponavljanje: bez duplikacije rezultata po stranici.
- Minimalne izmjene koda uz dosljednost sa postojećom arhitekturom.

## SLA i Timeouti
- `DOCUMENT_PROCESSING_TIMEOUT` (default: 3m): maksimalno vrijeme za cijeli dokument.
- `REQUEST_TIMEOUT` (default: 60s): maksimalno vrijeme za pojedini AI poziv.
- `PAGE_TOTAL_TIMEOUT` (default: 120s po stranici u workeru) ostaje kao zaštita, ali dokument-level timeout je autoritativan za dovršetak posla.

Behaviour pri doc timeoutu:
- Orchestrator pokreće nadzor (`monitorJobCompletion`).
- Na istek, označi sve neobrađene stranice MuPDF fallbackom i finalizira job kao `success` uz `timeout_occurred=true` metapodatak.
- Poziva `CancelJob(job_id)` u Redis-u da worker(i) prestanu obrađivati otkazani posao.

## Failover i Retrying
- Sekvenca u workeru (inline, bez čekanja):
  1) Primarni provider + primarni model
  2) Primarni provider + sekundarni model (na 429/5xx/timeout)
  3) Sekundarni provider + primarni model (i po potrebi njegov sekundarni)
  4) Ako sve padne → `/internal/page_failed` (bez odgođenih retry pokušaja)
- Odgođeni retry (ZSET) i dalje postoji u kodu kao opcija, ali doc‑SLA nadzor u orchestratoru osigurava da se posao završi na vrijeme (timeout finalization + cancel job). U praksi, preporuka je smanjiti `JOB_MAX_ATTEMPTS` na 1–2 u okruženjima s tvrdim SLA.

## Circuit Breaker i Concurrency
- Redis‑temeljeni cooldown per `provider:model` (TTL ključ + eksponencijalni backoff), reset na uspjeh.
- Lokalni per‑model semafor (`MAX_INFLIGHT_PER_MODEL`) sprječava burstove.
- Kada je breaker OPEN, worker odmah pokušava drugi model/provider – nikad ne čeka.

## MuPDF Fallback
- Na pojedinačni fail stranice worker zove `/internal/page_failed` – orchestrator upisuje fallback tekst te ažurira status.
- Pri doc timeoutu orchestrator dopunjava sve nedostajuće stranice MuPDF tekstom i finalizira rezultat (AI + MuPDF mix).
- Ako je tekst te stranice već spremljen (npr. ranije), koristi se postojeći; inače se izvlači on‑demand.

## Status, Agregacija i Storage
- Status i per‑page tekst u Redis-u (postojeće strukture: `job:{id}:status`, `job:{id}:page:{n}`).
- Finalna agregacija na završetku (sve stranice), rezultat lokalno (upload izvor) ili S3 (API izvor) – već implementirano i zadržano.

## Konfiguracija (sažetak)
- `DOCUMENT_PROCESSING_TIMEOUT` – npr. `3m` (obavezno za SLA)
- `REQUEST_TIMEOUT` – npr. `60s`
- `JOB_MAX_ATTEMPTS` – preporuka `1` ili `2` za strogi SLA
- `MAX_INFLIGHT_PER_MODEL`, `BREAKER_BASE_BACKOFF`, `BREAKER_MAX_BACKOFF` – kontrola adaptivnog limitera

## Implementacijske promjene (u ovom CDX v1)
- Dodan document-level monitor u Orchestratoru koji:
  - prati napredak,
  - na timeout radi MuPDF fallback za sve neobrađene stranice,
  - finalizira job sa parcijalnim rezultatima,
  - poziva `CancelJob(job_id)` kako bi se zaustavile daljnje obrade.
- `/internal/page_failed`: sada prvo pokušava uzeti postojeći tekst iz `PageStore`; tek ako ga nema, izvlači MuPDF on‑demand.

## Daljnji koraci (preporučeno)
- Vision klijenti: proširiti `internal/ai/*` za slanje base64 slike + system prompt + context.
- Pre‑pohrana MuPDF teksta: tijekom pripreme AI payload‑a spremiti MuPDF tekst unaprijed u Redis radi bržeg fallbacka.
- Izložiti "fast" model tier u API-u (`ModelTier`/`force_fast`) i propagirati do workera.
- Uskladiti retry politiku sa SLA (npr. `JOB_MAX_ATTEMPTS=1`) u produkcijskim env-ovima s čvrstim vremenskim ograničenjima.

***

Ovaj plan prioritizira dovršetak svakog dokumenta unutar zadanog maksimalnog vremena kroz orkestracijski timeout i determinističan fallback, bez blokirajućih čekanja u dispatcheru.

