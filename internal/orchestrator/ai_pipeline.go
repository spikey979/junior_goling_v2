package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/local/aidispatcher/internal/config"
	"github.com/local/aidispatcher/internal/converter"
	"github.com/local/aidispatcher/internal/imagerender"
	"github.com/local/aidispatcher/internal/mupdf"
	"github.com/rs/zerolog/log"
)

// PageClassification represents classification result for a single page
type PageClassification struct {
	PageNum        int
	CharCount      int
	Classification string // "TEXT_ONLY" or "HAS_GRAPHICS"
	MuPDFText      string // Extracted text if TEXT_ONLY
}

// AIPagePayload represents data prepared for AI processing
type AIPagePayload struct {
	PageNum        int
	ImageBytes     []byte // In-memory JPEG bytes
	ImageBase64    string // Base64 encoded image for JSON
	ImageMIME      string // "image/jpeg"
	WidthPx        int    // Image width in pixels
	HeightPx       int    // Image height in pixels
	ContextText    string // Text from surrounding pages (limited by MaxContextBytes)
	MuPDFText      string // Extracted MuPDF text
	Classification string // "TEXT_ONLY" or "HAS_GRAPHICS"
}

// ProcessJobForAI handles complete AI pipeline: download, convert, classify, extract, prepare
// This function does NOT send to AI workers - it only prepares everything and returns success
func (o *Orchestrator) ProcessJobForAI(ctx context.Context, jobID, filePath, user, password string) error {
	startTime := time.Now()
	log.Info().Str("job_id", jobID).Str("file", filePath).Msg("starting AI pipeline processing")

	// Update status: starting
	_ = o.deps.Status.Set(ctx, jobID, Status{
		Status:   "processing",
		Progress: 5,
		Message:  "Starting processing",
		Start:    &startTime,
		Metadata: map[string]any{
			"file_path": filePath,
			"user":      user,
			"password":  password,
			"source":    "api",
		},
	})

	// Step 1: Download file from S3 (if needed) - 10%
	pdfPath, cleanup, err := o.downloadAndPrepareFile(ctx, filePath, password, jobID)
	if cleanup != nil {
		defer cleanup() // Ensure cleanup happens
	}
	if err != nil {
		return fmt.Errorf("failed to download/prepare file: %w", err)
	}

	_ = o.deps.Status.Set(ctx, jobID, Status{
		Status:   "processing",
		Progress: 10,
		Message:  "File downloaded and prepared",
		Start:    &startTime,
		Metadata: map[string]any{
			"file_path":  filePath,
			"user":       user,
			"password":   password,
			"source":     "api",
			"local_path": pdfPath,
		},
	})

	// Step 2: MuPDF page-by-page text extraction - 20-25%
	pageTexts, err := o.extractPageTexts(ctx, pdfPath, jobID)
	if err != nil {
		return fmt.Errorf("failed to extract page texts: %w", err)
	}

	// PRE-STORAGE: Store MuPDF text in Redis for all pages (for fallback on AI failure/timeout)
	for pageNum, text := range pageTexts {
		key := fmt.Sprintf("job:%s:mupdf:%d", jobID, pageNum)
		err := o.deps.Redis.Set(ctx, key, text, 24*time.Hour).Err()
		if err != nil {
			log.Warn().
				Err(err).
				Str("job_id", jobID).
				Int("page", pageNum).
				Msg("failed to pre-store MuPDF text")
		}
	}

	log.Info().
		Str("job_id", jobID).
		Int("pages", len(pageTexts)).
		Msg("pre-stored MuPDF text for all pages in Redis")

	_ = o.deps.Status.Set(ctx, jobID, Status{
		Status:   "processing",
		Progress: 25,
		Message:  fmt.Sprintf("Extracted text from %d pages", len(pageTexts)),
		Start:    &startTime,
		Metadata: map[string]any{
			"file_path":  filePath,
			"user":       user,
			"password":   password,
			"source":     "api",
			"local_path": pdfPath,
			"page_count": len(pageTexts),
		},
	})

	// Step 3: Page classification - 30-40%
	classifications, err := o.classifyPages(ctx, pdfPath, pageTexts, jobID)
	if err != nil {
		return fmt.Errorf("failed to classify pages: %w", err)
	}

	_ = o.deps.Status.Set(ctx, jobID, Status{
		Status:   "processing",
		Progress: 40,
		Message:  "Page classification completed",
		Start:    &startTime,
		Metadata: map[string]any{
			"file_path":         filePath,
			"user":              user,
			"password":          password,
			"source":            "api",
			"local_path":        pdfPath,
			"page_count":        len(classifications),
			"text_only_pages":   countByClassification(classifications, "TEXT_ONLY"),
			"graphics_pages":    countByClassification(classifications, "HAS_GRAPHICS"),
		},
	})

	// Step 4: Image rendering and payload preparation - 40-50%
	payloads, err := o.prepareAIPayloads(ctx, pdfPath, classifications, pageTexts, jobID)
	if err != nil {
		return fmt.Errorf("failed to prepare AI payloads: %w", err)
	}

	_ = o.deps.Status.Set(ctx, jobID, Status{
		Status:   "processing",
		Progress: 50,
		Message:  "AI payloads prepared",
		Start:    &startTime,
		Metadata: map[string]any{
			"file_path":         filePath,
			"user":              user,
			"password":          password,
			"source":            "api",
			"local_path":        pdfPath,
			"page_count":        len(classifications),
			"text_only_pages":   countByClassification(classifications, "TEXT_ONLY"),
			"graphics_pages":    countByClassification(classifications, "HAS_GRAPHICS"),
			"payloads_prepared": len(payloads),
		},
	})

	// Step 5: Enqueue AI payloads to Redis queue
	totalPages := len(classifications)
	aiPages := len(payloads)
	textOnlyPages := countByClassification(classifications, "TEXT_ONLY")

	// Enqueue all AI pages with proper worker payload format
	for _, aiPayload := range payloads {
		// Create worker-compatible payload
		workerPayload := map[string]interface{}{
			"job_id":           jobID,
			"page_id":          aiPayload.PageNum,
			"content_ref":      "", // Empty for in-memory processing
			"ai_engine":        "openai", // Default, can be overridden
			"force_fast":       false,
			"attempt":          1,
			"image_b64":        aiPayload.ImageBase64,
			"image_mime":       aiPayload.ImageMIME,
			"width_px":         aiPayload.WidthPx,
			"height_px":        aiPayload.HeightPx,
			"mupdf_text":       aiPayload.MuPDFText,
			"context_text":     aiPayload.ContextText,
			"system_prompt":    o.cfg.SystemPrompt.DefaultPrompt,
			"classification":   aiPayload.Classification,
			"idempotency_key":  fmt.Sprintf("%s:page:%d", jobID, aiPayload.PageNum),
			"user":             user,
			"source":           "api",
		}

		payloadJSON, err := json.Marshal(workerPayload)
		if err != nil {
			log.Error().Err(err).Str("job_id", jobID).Int("page", aiPayload.PageNum).Msg("failed to marshal worker payload")
			continue
		}

		if err := o.deps.Queue.EnqueueAI(ctx, payloadJSON); err != nil {
			log.Error().Err(err).Str("job_id", jobID).Int("page", aiPayload.PageNum).Msg("failed to enqueue AI payload")
			continue
		}
	}

	log.Info().
		Str("job_id", jobID).
		Int("ai_pages", aiPages).
		Int("text_only_pages", textOnlyPages).
		Msg("AI pages enqueued to Redis queue")

	// Update status with AI page tracking
	_ = o.deps.Status.Set(ctx, jobID, Status{
		Status:   "processing",
		Progress: 60,
		Message:  fmt.Sprintf("Enqueued %d AI pages for processing", aiPages),
		Start:    &startTime,
		Metadata: map[string]any{
			"file_path":       filePath,
			"user":            user,
			"password":        password,
			"source":          "api",
			"local_path":      pdfPath,
			"total_pages":     totalPages,
			"ai_pages":        aiPages,
			"text_only_pages": textOnlyPages,
			"pages_done":      0,
			"pages_failed":    0,
		},
	})

	// Step 6: Start job monitor with timeout (JOB_TIMEOUT default: 5m)
	go func() {
		monitorCtx, monitorCancel := context.WithTimeout(context.Background(), o.cfg.Timeouts.JobTimeout)
		defer monitorCancel()
		o.monitorJobCompletion(monitorCtx, jobID, totalPages, aiPages)
	}()

	log.Info().
		Str("job_id", jobID).
		Dur("duration", time.Since(startTime)).
		Int("ai_pages", aiPages).
		Int("total_pages", totalPages).
		Msg("AI pipeline setup completed - monitor started")

	return nil
}

// downloadAndPrepareFile downloads from S3, detects file type, converts to PDF if needed
// Returns: pdfPath, cleanup function, error
func (o *Orchestrator) downloadAndPrepareFile(ctx context.Context, filePath, password, jobID string) (string, func(), error) {
	var cleanupFuncs []func()
	cleanup := func() {
		for _, fn := range cleanupFuncs {
			fn()
		}
	}

	// Download from S3 with decryption
	if !strings.HasPrefix(filePath, "s3://") {
		return "", cleanup, fmt.Errorf("only S3 paths supported for now: %s", filePath)
	}

	tempFile, err := downloadS3ToTemp(ctx, filePath, password)
	if err != nil {
		return "", cleanup, fmt.Errorf("failed to download from S3: %w", err)
	}
	cleanupFuncs = append(cleanupFuncs, func() { os.Remove(tempFile) })

	log.Info().Str("job_id", jobID).Str("temp_file", tempFile).Msg("file downloaded from S3")

	// Detect file type
	fileInfo, err := o.deps.FileType.Detect(tempFile)
	if err != nil {
		return "", cleanup, fmt.Errorf("file type detection failed: %w", err)
	}

	if !fileInfo.Supported {
		return "", cleanup, fmt.Errorf("unsupported file type: %s", fileInfo.Description)
	}

	log.Info().Str("job_id", jobID).Str("mime", fileInfo.MIMEType).Str("desc", fileInfo.Description).Msg("detected file type")

	// Convert to PDF if needed
	pdfPath := tempFile
	if fileInfo.MIMEType != "application/pdf" && !fileInfo.IsText {
		log.Info().Str("job_id", jobID).Str("file", tempFile).Msg("converting to PDF with LibreOffice")

		convertedPath := filepath.Join(filepath.Dir(tempFile), fmt.Sprintf("%s_converted.pdf", jobID))
		convJob := converter.Job{
			InputPath:  tempFile,
			OutputPath: convertedPath,
			Extension:  fileInfo.Extension,
			Timeout:    180 * time.Second,
		}

		result := o.deps.Converter.ConvertToPDF(convJob)
		if !result.Success {
			return "", cleanup, fmt.Errorf("conversion failed: %s", result.Error)
		}

		pdfPath = result.OutputPath
		cleanupFuncs = append(cleanupFuncs, func() { os.Remove(pdfPath) })

		log.Info().Str("job_id", jobID).Str("pdf", pdfPath).Dur("duration", result.Duration).Msg("conversion successful")
	}

	return pdfPath, cleanup, nil
}

// extractPageTexts extracts text from each page using MuPDF/go-fitz
func (o *Orchestrator) extractPageTexts(ctx context.Context, pdfPath, jobID string) (map[int]string, error) {
	extractor := mupdf.NewGoFitzExtractor()

	pageCount, err := extractor.GetPageCount(pdfPath)
	if err != nil {
		return nil, fmt.Errorf("failed to get page count: %w", err)
	}

	pageTexts := make(map[int]string)

	for i := 1; i <= pageCount; i++ {
		pageText, err := extractor.ExtractTextByPage(pdfPath, i)
		if err != nil {
			log.Warn().Err(err).Int("page", i).Msg("failed to extract text from page")
			pageText = "" // Empty text for failed pages
		}

		pageTexts[i] = pageText

		log.Debug().
			Str("job_id", jobID).
			Int("page", i).
			Int("chars", len(pageText)).
			Msg("extracted page text")
	}

	log.Info().Str("job_id", jobID).Int("pages", pageCount).Msg("completed page-by-page text extraction")
	return pageTexts, nil
}

// classifyPages classifies each page as TEXT_ONLY or HAS_GRAPHICS
func (o *Orchestrator) classifyPages(ctx context.Context, pdfPath string, pageTexts map[int]string, jobID string) ([]PageClassification, error) {
	// Create graphics detector
	detector := NewGraphicsDetector()

	var classifications []PageClassification

	for pageNum, text := range pageTexts {
		charCount := countVisibleChars(text)

		// Check for large graphics (>= 2x2 cm)
		hasLargeGraphics, err := detector.HasLargeGraphics(pdfPath, pageNum, MinGraphicsSizeCM)
		if err != nil {
			log.Warn().Err(err).Int("page", pageNum).Msg("graphics detection failed, falling back to text-based classification")
			hasLargeGraphics = false
		}

		// Classification logic:
		// 1. If page has graphics >= 2x2 cm → HAS_GRAPHICS
		// 2. Otherwise → TEXT_ONLY
		classification := "TEXT_ONLY"
		reason := "no large graphics detected"

		if hasLargeGraphics {
			classification = "HAS_GRAPHICS"
			reason = "large graphics detected (>= 2x2 cm)"
		}

		classifications = append(classifications, PageClassification{
			PageNum:        pageNum,
			CharCount:      charCount,
			Classification: classification,
			MuPDFText:      text,
		})

		// Get detailed analysis for logging (debug only)
		if log.Debug().Enabled() {
			analysis, _ := detector.AnalyzePageGraphics(pdfPath, pageNum)
			log.Debug().
				Str("job_id", jobID).
				Int("page", pageNum).
				Interface("graphics_analysis", analysis).
				Msg("detailed graphics analysis")
		}

		log.Info().
			Str("job_id", jobID).
			Int("page", pageNum).
			Str("classification", classification).
			Str("reason", reason).
			Msg("page classified")
	}

	log.Info().
		Str("job_id", jobID).
		Int("total_pages", len(classifications)).
		Int("text_only", countByClassification(classifications, "TEXT_ONLY")).
		Int("graphics", countByClassification(classifications, "HAS_GRAPHICS")).
		Msg("page classification completed")

	return classifications, nil
}

// prepareAIPayloads renders pages to JPEG (in-memory) and prepares payloads with context
func (o *Orchestrator) prepareAIPayloads(ctx context.Context, pdfPath string, classifications []PageClassification, pageTexts map[int]string, jobID string) ([]AIPagePayload, error) {
	// Get image options from config
	opts := config.DefaultImageOptions()

	var payloads []AIPagePayload
	var totalImageBytes int64

	for _, class := range classifications {
		var imageBytes []byte
		var imageBase64 string
		var widthPx, heightPx int
		var imageMIME string

		// Only render images for HAS_GRAPHICS pages (unless SendAllPages=true)
		shouldRenderImage := class.Classification == "HAS_GRAPHICS" || opts.SendAllPages

		if shouldRenderImage {
			// Render page to JPEG (in-memory)
			jpegBytes, width, height, err := imagerender.RenderPageToJPEG(
				pdfPath,
				class.PageNum,
				opts.DPI,
				opts.JPEGQuality,
				opts.Color,
			)
			if err != nil {
				log.Warn().Err(err).Int("page", class.PageNum).Msg("failed to render page to JPEG")
			} else {
				imageBytes = jpegBytes
				widthPx = width
				heightPx = height
				imageMIME = "image/jpeg"
				totalImageBytes += int64(len(jpegBytes))

				// Encode to base64 if requested
				if opts.IncludeBase64 {
					imageBase64 = imagerender.EncodeToBase64(jpegBytes)
				}

				log.Debug().
					Str("job_id", jobID).
					Int("page", class.PageNum).
					Int("jpeg_size", len(jpegBytes)).
					Int("width", width).
					Int("height", height).
					Msg("rendered page to JPEG (in-memory)")
			}
		}

		// Prepare context text with size limit
		contextText := prepareContextTextWithLimit(pageTexts, class.PageNum, opts.ContextRadius, opts.MaxContextBytes)

		payload := AIPagePayload{
			PageNum:        class.PageNum,
			ImageBytes:     imageBytes,
			ImageBase64:    imageBase64,
			ImageMIME:      imageMIME,
			WidthPx:        widthPx,
			HeightPx:       heightPx,
			ContextText:    contextText,
			MuPDFText:      class.MuPDFText,
			Classification: class.Classification,
		}

		payloads = append(payloads, payload)

		log.Debug().
			Str("job_id", jobID).
			Int("page", class.PageNum).
			Bool("has_image", len(imageBytes) > 0).
			Int("context_chars", len(contextText)).
			Int("mupdf_chars", len(class.MuPDFText)).
			Msg("prepared AI payload")
	}

	log.Info().
		Str("job_id", jobID).
		Int("payloads", len(payloads)).
		Int64("total_image_bytes", totalImageBytes).
		Msg("AI payload preparation completed (in-memory)")

	return payloads, nil
}

// Helper functions

func countVisibleChars(text string) int {
	count := 0
	for _, r := range text {
		// Count only alphanumeric and common punctuation
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == ',' || r == ';' || r == ':' || r == '!' || r == '?' {
			count++
		}
	}
	return count
}

func countByClassification(classifications []PageClassification, targetClass string) int {
	count := 0
	for _, c := range classifications {
		if c.Classification == targetClass {
			count++
		}
	}
	return count
}

func prepareContextText(pageTexts map[int]string, pageNum, radius int) string {
	var contextParts []string

	// Add text from previous pages (within radius)
	for i := pageNum - radius; i < pageNum; i++ {
		if i > 0 {
			if text, ok := pageTexts[i]; ok && text != "" {
				contextParts = append(contextParts, fmt.Sprintf("=== Page %d (context) ===\n%s", i, text))
			}
		}
	}

	// Add current page text
	if text, ok := pageTexts[pageNum]; ok && text != "" {
		contextParts = append(contextParts, fmt.Sprintf("=== Page %d (current) ===\n%s", pageNum, text))
	}

	// Add text from next pages (within radius)
	for i := pageNum + 1; i <= pageNum+radius; i++ {
		if text, ok := pageTexts[i]; ok && text != "" {
			contextParts = append(contextParts, fmt.Sprintf("=== Page %d (context) ===\n%s", i, text))
		}
	}

	return strings.Join(contextParts, "\n\n")
}

// prepareContextTextWithLimit prepares context text with byte size limit
func prepareContextTextWithLimit(pageTexts map[int]string, pageNum, radius, maxBytes int) string {
	fullContext := prepareContextText(pageTexts, pageNum, radius)

	// Truncate if exceeds maxBytes
	if len(fullContext) > maxBytes {
		truncated := fullContext[:maxBytes]
		// Try to truncate at word boundary if possible
		if lastSpace := strings.LastIndex(truncated, " "); lastSpace > maxBytes-100 {
			truncated = truncated[:lastSpace]
		}
		return truncated + "...[truncated]"
	}

	return fullContext
}

func truncateText(text string, maxLen int) string {
	if len(text) <= maxLen {
		return text
	}
	return text[:maxLen] + "..."
}

func getClassificationReason(charCount int) string {
	if charCount >= 120 {
		return fmt.Sprintf("charCount=%d >= 120 (threshold)", charCount)
	}
	return fmt.Sprintf("charCount=%d < 120 (threshold)", charCount)
}
