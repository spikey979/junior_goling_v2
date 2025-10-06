package orchestrator

import (
	"context"
	"time"

	"github.com/rs/zerolog/log"
)

// monitorJobCompletion monitors job completion and handles timeout
func (o *Orchestrator) monitorJobCompletion(ctx context.Context, jobID string, totalPages, aiPages int, aiStartTime time.Time) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	log.Info().
		Str("job_id", jobID).
		Int("total_pages", totalPages).
		Int("ai_pages", aiPages).
		Dur("timeout", o.cfg.Timeouts.JobTimeout).
		Msg("started job completion monitor")

	for {
		select {
		case <-ctx.Done():
			// Job timeout reached - finalize with partial results
			log.Warn().
				Str("job_id", jobID).
				Dur("timeout", o.cfg.Timeouts.JobTimeout).
				Msg("job timeout reached - finalizing with partial results")

			// Cancel job in Redis so workers stop processing
			_ = o.deps.Queue.CancelJob(context.Background(), jobID)

			// Finalize with partial results
			o.finalizeJobWithPartialResults(context.Background(), jobID, totalPages)
			return

		case <-ticker.C:
			// Check if job was cancelled in Redis (primary check)
			if cancelled, _ := o.deps.Queue.IsCancelled(context.Background(), jobID); cancelled {
				log.Info().Str("job_id", jobID).Msg("job cancelled (detected via Redis) - stopping monitor")
				return
			}

			// Check if all pages are processed
			st, ok, err := o.deps.Status.Get(context.Background(), jobID)
			if !ok || err != nil {
				log.Warn().
					Err(err).
					Str("job_id", jobID).
					Msg("failed to get status in monitor")
				continue
			}

			// Check if job was cancelled externally (secondary check via status)
			if st.Status == "cancelled" {
				log.Info().Str("job_id", jobID).Msg("job cancelled (detected via status) - stopping monitor")
				return
			}

			// Check completion
			pagesDone := intFromMeta(st.Metadata, "pages_done")
			pagesFailed := intFromMeta(st.Metadata, "pages_failed")
			pagesProcessed := pagesDone + pagesFailed

			log.Debug().
				Str("job_id", jobID).
				Int("pages_done", pagesDone).
				Int("pages_failed", pagesFailed).
				Int("ai_pages", aiPages).
				Msg("monitor tick")

			if pagesProcessed >= aiPages {
				// All AI pages processed - normal completion
				aiDuration := time.Since(aiStartTime)
				log.Info().
					Str("job_id", jobID).
					Int("ai_pages", aiPages).
					Int("pages_done", pagesDone).
					Int("pages_failed", pagesFailed).
					Float64("ai_total_duration_sec", aiDuration.Seconds()).
					Msg("all AI pages processed - finalizing job")

				o.finalizeJobComplete(context.Background(), jobID, totalPages)
				return
			}
		}
	}
}
