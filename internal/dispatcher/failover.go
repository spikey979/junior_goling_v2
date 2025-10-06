package dispatcher

import (
	"context"
	"fmt"
	"time"

	"github.com/local/aidispatcher/internal/ai"
	mpkg "github.com/local/aidispatcher/internal/metrics"
	"github.com/rs/zerolog/log"
)

// processPageWithFailover implements 4-step failover logic with circuit breaker
func (w *Worker) processPageWithFailover(ctx context.Context, jobID string, pageID int, contentRef, preferEngine string, forceFast bool, imageB64, imageMIME, systemPrompt, contextText, mupdfText string) (bool, string, string, string, error) {
	// Determine providers
	primaryProv := w.conf.Providers.PrimaryEngine
	secondaryProv := w.conf.Providers.SecondaryEngine
	if preferEngine != "" {
		primaryProv = preferEngine
		// Determine secondary based on primary
		if primaryProv == "openai" {
			secondaryProv = "anthropic"
		} else {
			secondaryProv = "openai"
		}
	}

	// Helper to get model for tier
	getModel := func(provider, tier string) string {
		switch provider {
		case "openai":
			switch tier {
			case "primary":
				return w.conf.Providers.OpenAI.Primary
			case "secondary":
				return w.conf.Providers.OpenAI.Secondary
			case "fast":
				return w.conf.Providers.OpenAI.Fast
			default:
				return w.conf.Providers.OpenAI.Primary
			}
		case "anthropic":
			switch tier {
			case "primary":
				return w.conf.Providers.Anthropic.Primary
			case "secondary":
				return w.conf.Providers.Anthropic.Secondary
			case "fast":
				return w.conf.Providers.Anthropic.Fast
			default:
				return w.conf.Providers.Anthropic.Primary
			}
		}
		return ""
	}

	// Helper to call AI with timeout and error handling
	callAI := func(provider, model string) (ai.Response, error) {
		timeout := w.conf.Timeouts.RequestTimeout

		req := ai.Request{
			JobID:        jobID,
			PageID:       pageID,
			ContentRef:   contentRef,
			Model:        model,
			Timeout:      timeout,
			ImageBase64:  imageB64,
			ImageMIME:    imageMIME,
			SystemPrompt: systemPrompt,
			ContextText:  contextText,
			MuPDFText:    mupdfText,
		}

		// IMPORTANT: Create fresh context for each attempt, not inheriting from parent
		// This prevents cancelled context from previous attempt affecting this one
		cctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		var client ai.Client
		switch provider {
		case "openai":
			client = w.openai
		case "anthropic":
			client = w.anthropic
		default:
			return ai.Response{}, fmt.Errorf("unknown provider: %s", provider)
		}

		start := time.Now()
		resp, err := client.Do(cctx, req)
		dur := time.Since(start)

		// Handle timeout
		if err != nil && (cctx.Err() == context.DeadlineExceeded) {
			mpkg.ObserveProvider(provider, model, "timeout", dur)
			log.Warn().
				Str("job_id", jobID).
				Int("page_id", pageID).
				Str("provider", provider).
				Str("model", model).
				Dur("duration", dur).
				Dur("timeout", timeout).
				Msg("AI request timeout - will trigger failover")
			return ai.Response{}, &RateLimitError{Provider: provider, Model: model, Reason: "timeout"}
		}

		// Classify error
		result := "success"
		if err != nil {
			if ai.IsRateLimited(err) {
				result = "rate_limited"
			} else if isTransientError(err) {
				result = "transient"
			} else if isFatalError(err) {
				result = "fatal"
			} else {
				result = "unknown"
			}
		}

		mpkg.ObserveProvider(provider, model, result, dur)

		if err != nil {
			log.Warn().
				Str("job_id", jobID).
				Int("page_id", pageID).
				Str("provider", provider).
				Str("model", model).
				Dur("duration", dur).
				Str("result", result).
				Err(err).
				Msg("AI provider call failed")
		} else {
			log.Debug().
				Str("job_id", jobID).
				Int("page_id", pageID).
				Str("provider", provider).
				Str("model", model).
				Dur("duration", dur).
				Int("tokens_in", resp.TokensIn).
				Int("tokens_out", resp.TokensOut).
				Msg("AI provider call success")
		}

		return resp, err
	}

	var lastErr error

	// Determine model tier based on forceFast
	modelTier := "primary"
	if forceFast {
		modelTier = "fast"
	}

	// === ATTEMPT 1: Primary Provider, Primary/Fast Model ===
	primaryModel := getModel(primaryProv, modelTier)
	if primaryModel != "" && !w.breaker.IsCircuitOpen(ctx, primaryProv, primaryModel) {
		log.Info().
			Str("job_id", jobID).
			Int("page_id", pageID).
			Str("provider", primaryProv).
			Str("model", primaryModel).
			Msg("attempting AI processing [1/4]")

		resp, err := callAI(primaryProv, primaryModel)

		if err == nil {
			// SUCCESS
			w.breaker.CloseCircuitBreaker(ctx, primaryProv, primaryModel)
			mpkg.BreakerClosed(primaryProv, primaryModel)
			return true, primaryProv, primaryModel, resp.Text, nil
		}

		// Handle error
		if isTransientError(err) {
			w.breaker.OpenCircuitBreaker(ctx, primaryProv, primaryModel)
			mpkg.BreakerOpened(primaryProv, primaryModel)
			log.Warn().
				Err(err).
				Str("job_id", jobID).
				Int("page_id", pageID).
				Str("provider", primaryProv).
				Str("model", primaryModel).
				Msg("transient error - trying fallback")
			lastErr = err
		} else if isFatalError(err) {
			log.Error().
				Err(err).
				Str("job_id", jobID).
				Int("page_id", pageID).
				Str("provider", primaryProv).
				Str("model", primaryModel).
				Msg("fatal error - no retry")
			return false, "", "", "", err
		}
		lastErr = err
	} else {
		log.Debug().
			Str("provider", primaryProv).
			Str("model", primaryModel).
			Msg("circuit breaker OPEN - skipping primary attempt")
	}

	// === ATTEMPT 2: Primary Provider, Secondary Model ===
	secondaryModel := getModel(primaryProv, "secondary")
	if secondaryModel != "" && secondaryModel != primaryModel && !w.breaker.IsCircuitOpen(ctx, primaryProv, secondaryModel) {
		log.Info().
			Str("job_id", jobID).
			Int("page_id", pageID).
			Str("provider", primaryProv).
			Str("model", secondaryModel).
			Msg("attempting secondary model same provider [2/4]")

		resp, err := callAI(primaryProv, secondaryModel)

		if err == nil {
			w.breaker.CloseCircuitBreaker(ctx, primaryProv, secondaryModel)
			mpkg.BreakerClosed(primaryProv, secondaryModel)
			return true, primaryProv, secondaryModel, resp.Text, nil
		}

		if isTransientError(err) {
			w.breaker.OpenCircuitBreaker(ctx, primaryProv, secondaryModel)
			mpkg.BreakerOpened(primaryProv, secondaryModel)
			lastErr = err
		} else if isFatalError(err) {
			return false, "", "", "", err
		}
		lastErr = err
	}

	// === ATTEMPT 3: Secondary Provider, Primary/Fast Model ===
	secondaryProviderModel := getModel(secondaryProv, modelTier)
	if secondaryProviderModel != "" && !w.breaker.IsCircuitOpen(ctx, secondaryProv, secondaryProviderModel) {
		log.Info().
			Str("job_id", jobID).
			Int("page_id", pageID).
			Str("provider", secondaryProv).
			Str("model", secondaryProviderModel).
			Msg("attempting secondary provider [3/4]")

		resp, err := callAI(secondaryProv, secondaryProviderModel)

		if err == nil {
			w.breaker.CloseCircuitBreaker(ctx, secondaryProv, secondaryProviderModel)
			mpkg.BreakerClosed(secondaryProv, secondaryProviderModel)
			return true, secondaryProv, secondaryProviderModel, resp.Text, nil
		}

		if isTransientError(err) {
			w.breaker.OpenCircuitBreaker(ctx, secondaryProv, secondaryProviderModel)
			mpkg.BreakerOpened(secondaryProv, secondaryProviderModel)
			lastErr = err
		} else if isFatalError(err) {
			return false, "", "", "", err
		}
		lastErr = err
	}

	// === ATTEMPT 4: Secondary Provider, Secondary Model ===
	secondaryProviderSecondaryModel := getModel(secondaryProv, "secondary")
	if secondaryProviderSecondaryModel != "" && secondaryProviderSecondaryModel != secondaryProviderModel && !w.breaker.IsCircuitOpen(ctx, secondaryProv, secondaryProviderSecondaryModel) {
		log.Info().
			Str("job_id", jobID).
			Int("page_id", pageID).
			Str("provider", secondaryProv).
			Str("model", secondaryProviderSecondaryModel).
			Msg("attempting secondary provider secondary model [4/4]")

		resp, err := callAI(secondaryProv, secondaryProviderSecondaryModel)

		if err == nil {
			w.breaker.CloseCircuitBreaker(ctx, secondaryProv, secondaryProviderSecondaryModel)
			mpkg.BreakerClosed(secondaryProv, secondaryProviderSecondaryModel)
			return true, secondaryProv, secondaryProviderSecondaryModel, resp.Text, nil
		}

		if isTransientError(err) {
			w.breaker.OpenCircuitBreaker(ctx, secondaryProv, secondaryProviderSecondaryModel)
			mpkg.BreakerOpened(secondaryProv, secondaryProviderSecondaryModel)
			lastErr = err
		}
		lastErr = err
	}

	// === ALL ATTEMPTS EXHAUSTED ===
	log.Error().
		Str("job_id", jobID).
		Int("page_id", pageID).
		Err(lastErr).
		Msg("all AI providers/models exhausted - marking page as failed")

	mpkg.ObserveProvider("all", "all", "exhausted", 0)

	if lastErr == nil {
		lastErr = fmt.Errorf("all providers exhausted for job %s page %d", jobID, pageID)
	}

	return false, "", "", "", lastErr
}
