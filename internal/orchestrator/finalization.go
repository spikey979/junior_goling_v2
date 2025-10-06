package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// getMuPDFTextForPage retrieves MuPDF text for a specific page from Redis
func (o *Orchestrator) getMuPDFTextForPage(ctx context.Context, jobID string, pageNum int) string {
	// First try from page store (may already be saved)
	existingText, err := o.deps.Pages.GetPageText(ctx, jobID, pageNum)
	if err == nil && existingText != "" {
		log.Debug().
			Str("job_id", jobID).
			Int("page", pageNum).
			Msg("retrieved existing page text")
		return existingText
	}

	// Try from MuPDF cache (pre-stored during initial extraction)
	key := fmt.Sprintf("job:%s:mupdf:%d", jobID, pageNum)
	mupdfText, err := o.deps.Redis.Get(ctx, key).Result()
	if err == nil && mupdfText != "" {
		log.Debug().
			Str("job_id", jobID).
			Int("page", pageNum).
			Int("text_len", len(mupdfText)).
			Msg("retrieved MuPDF text from pre-storage")
		return mupdfText
	}

	// Fallback - return placeholder
	log.Warn().
		Str("job_id", jobID).
		Int("page", pageNum).
		Msg("MuPDF text not available for fallback - using placeholder")

	return fmt.Sprintf("[Page %d - text not available]", pageNum)
}

// aggregateAllPages combines text from all pages into a single document
func (o *Orchestrator) aggregateAllPages(ctx context.Context, jobID string, totalPages int) (string, []string) {
	var builder strings.Builder
	sources := make([]string, totalPages)

	for i := 1; i <= totalPages; i++ {
		pageText, source, err := o.deps.Pages.GetPageTextWithSource(ctx, jobID, i)
		if err != nil {
			pageText = fmt.Sprintf("[Page %d - error retrieving text]", i)
			source = "error"
		}
		if pageText == "" {
			pageText = fmt.Sprintf("[Page %d - no text]", i)
			if source == "" {
				source = "missing"
			}
		}

		if i > 1 {
			builder.WriteString("\n\n")
		}

		// Check if pageText already starts with page marker (AI adds it)
		expectedMarker := fmt.Sprintf("=== Page %d ===", i)
		if !strings.HasPrefix(strings.TrimSpace(pageText), expectedMarker) {
			// Add marker only if not already present
			builder.WriteString(fmt.Sprintf("=== Page %d ===\n", i))
		}
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

// finalizeJobComplete finalizes a job when all pages are successfully processed
func (o *Orchestrator) finalizeJobComplete(ctx context.Context, jobID string, totalPages int) {
	log.Info().
		Str("job_id", jobID).
		Int("total_pages", totalPages).
		Msg("finalizing job - all pages processed")

	// Get current status
	st, ok, err := o.deps.Status.Get(ctx, jobID)
	if !ok || err != nil {
		log.Error().
			Err(err).
			Str("job_id", jobID).
			Msg("failed to get status for finalization")
		return
	}

	// Check if already finalized (avoid double-save)
	if st.Status == "success" {
		log.Debug().
			Str("job_id", jobID).
			Msg("job already finalized - skipping")
		return
	}

	// Aggregate all pages
	aggregatedText, sources := o.aggregateAllPages(ctx, jobID, totalPages)

	// DEBUG: Log aggregated text details
	preview := aggregatedText
	if len(aggregatedText) > 300 {
		preview = aggregatedText[:300] + "..."
	}
	log.Debug().
		Str("job_id", jobID).
		Int("agg_length", len(aggregatedText)).
		Str("agg_preview", preview).
		Msg("finalizeJobComplete aggregated text")

	// Save result (S3 or local based on source)
	if src, _ := st.Metadata["source"].(string); src == "upload" {
		localPath, err := SaveAggregatedTextToLocal(ctx, jobID, aggregatedText)
		if err != nil {
			log.Error().Err(err).Str("job_id", jobID).Msg("failed to save result locally")
			st.Status = "failed"
			st.Message = fmt.Sprintf("Failed to save result: %v", err)
			_ = o.deps.Status.Set(ctx, jobID, st)
			return
		}
		st.Metadata["result_local_path"] = localPath
	} else {
		filePath, _ := st.Metadata["file_path"].(string)
		password, _ := st.Metadata["password"].(string)
		// Upload as AI v2 (replaces old SaveAggregatedTextToS3)
		s3URL, err := uploadAITextAsV2(ctx, o.deps.Queue, filePath, jobID, aggregatedText, password)
		if err != nil {
			log.Error().Err(err).Str("job_id", jobID).Msg("failed to save AI v2 result to S3")
			st.Status = "failed"
			st.Message = fmt.Sprintf("Failed to save result: %v", err)
			_ = o.deps.Status.Set(ctx, jobID, st)
			return
		}
		st.Metadata["result_s3_url"] = s3URL
		st.Metadata["text_v2_url"] = s3URL // Mark as v2
		// Ghost Server compatibility fields for V2
		st.Metadata["v2_ready"] = true
		st.Metadata["v2_s3_key"] = s3URL
		st.Metadata["current_version"] = "2"
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

	_ = o.deps.Status.Set(ctx, jobID, st)

	log.Info().
		Str("job_id", jobID).
		Int("total_chars", len(aggregatedText)).
		Int("ai_pages", aiCount).
		Int("mupdf_pages", mupdfCount).
		Msg("job finalized successfully")
}

// finalizeJobWithPartialResults finalizes a job on timeout with partial AI results
func (o *Orchestrator) finalizeJobWithPartialResults(ctx context.Context, jobID string, totalPages int) {
	log.Warn().
		Str("job_id", jobID).
		Msg("job timeout - finalizing with partial results")

	st, ok, err := o.deps.Status.Get(ctx, jobID)
	if !ok || err != nil {
		log.Error().
			Err(err).
			Str("job_id", jobID).
			Msg("status not found for timeout finalization")
		return
	}

	// Identify missing pages
	missingPages := []int{}

	// Get list of AI pages
	aiPagesInterface, _ := st.Metadata["ai_page_numbers"].([]interface{})
	aiPageNumbers := []int{}
	for _, p := range aiPagesInterface {
		if pNum, ok := p.(int); ok {
			aiPageNumbers = append(aiPageNumbers, pNum)
		} else if pNum, ok := p.(float64); ok {
			aiPageNumbers = append(aiPageNumbers, int(pNum))
		}
	}

	// Check which AI page is missing
	for _, pageNum := range aiPageNumbers {
		existingText, err := o.deps.Pages.GetPageText(ctx, jobID, pageNum)
		if err != nil || existingText == "" {
			missingPages = append(missingPages, pageNum)
		}
	}

	log.Warn().
		Str("job_id", jobID).
		Ints("missing_pages", missingPages).
		Int("count", len(missingPages)).
		Msg("applying MuPDF fallback for timeout pages")

	// Fill missing pages with MuPDF text
	for _, pageNum := range missingPages {
		mupdfText := o.getMuPDFTextForPage(ctx, jobID, pageNum)

		_ = o.deps.Pages.SavePageText(
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

	// Aggregate all pages (AI + MuPDF + timeout fallback)
	aggregatedText, sources := o.aggregateAllPages(ctx, jobID, totalPages)

	// Save result
	if src, _ := st.Metadata["source"].(string); src == "upload" {
		localPath, _ := SaveAggregatedTextToLocal(ctx, jobID, aggregatedText)
		st.Metadata["result_local_path"] = localPath
	} else {
		filePath, _ := st.Metadata["file_path"].(string)
		password, _ := st.Metadata["password"].(string)
		// Upload as AI v2 (timeout/partial results)
		s3URL, _ := uploadAITextAsV2(ctx, o.deps.Queue, filePath, jobID, aggregatedText, password)
		st.Metadata["result_s3_url"] = s3URL
		st.Metadata["text_v2_url"] = s3URL // Mark as v2
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

	_ = o.deps.Status.Set(ctx, jobID, st)

	log.Warn().
		Str("job_id", jobID).
		Int("total_chars", len(aggregatedText)).
		Int("ai_pages", aiCount).
		Int("mupdf_pages", mupdfCount).
		Int("timeout_fallback", timeoutFallbackCount).
		Msg("job finalized with timeout fallback")
}

// checkAndFinalizeJob checks if all pages are processed and finalizes if ready
func (o *Orchestrator) checkAndFinalizeJob(ctx context.Context, jobID string) {
	st, ok, err := o.deps.Status.Get(ctx, jobID)
	if !ok || err != nil {
		return
	}

	totalPages := intFromMeta(st.Metadata, "total_pages")
	aiPages := intFromMeta(st.Metadata, "ai_pages")
	pagesProcessed := intFromMeta(st.Metadata, "pages_done") + intFromMeta(st.Metadata, "pages_failed")

	if pagesProcessed >= aiPages {
		// All AI pages accounted for - finalize
		o.finalizeJobComplete(ctx, jobID, totalPages)
	}
}

// Helper to safely extract int from metadata map
func intFromMeta(meta map[string]any, key string) int {
	if meta == nil {
		return 0
	}
	val, ok := meta[key]
	if !ok {
		return 0
	}
	switch v := val.(type) {
	case int:
		return v
	case float64:
		return int(v)
	case int64:
		return int(v)
	default:
		return 0
	}
}
