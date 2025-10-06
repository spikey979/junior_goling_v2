package orchestrator

import (
    "context"
    "fmt"
    "os"
    "strings"
    "time"

    "github.com/local/aidispatcher/internal/storage"
    "github.com/rs/zerolog/log"
)

// QueueChecker interface for checking if a job is cancelled
type QueueChecker interface {
    IsCancelled(ctx context.Context, jobID string) (bool, error)
}

// uploadMuPDFTextAsV1 uploads aggregated MuPDF text as version 1 to S3
// Returns: s3URL (with _v1 suffix), error
func uploadMuPDFTextAsV1(ctx context.Context, queue QueueChecker, originalRef, jobID, text, password string) (string, error) {
	// Check if job was cancelled before uploading
	if queue != nil {
		if cancelled, err := queue.IsCancelled(ctx, jobID); err == nil && cancelled {
			log.Warn().Str("job_id", jobID).Msg("job is cancelled; aborting MuPDF v1 upload")
			return "", fmt.Errorf("job %s was cancelled; MuPDF v1 upload aborted", jobID)
		}
	}

	bucket := os.Getenv("AWS_S3_BUCKET")
	if bucket == "" {
		bucket = "junior-files-dev" // default bucket
	}

	// Extract bucket and original key from originalRef
	var originalKey string
	if strings.HasPrefix(originalRef, "s3://") {
		path := strings.TrimPrefix(originalRef, "s3://")
		parts := strings.SplitN(path, "/", 2)
		if len(parts) == 2 && parts[0] != "" {
			bucket = parts[0]
			originalKey = parts[1]
		}
	}

	// Generate v1 key by removing _original suffix and adding _v1
	baseKey := strings.TrimSuffix(originalKey, "_original")
	if baseKey == "" || baseKey == originalKey {
		// Fallback if no _original suffix found
		baseKey = fmt.Sprintf("results/%s/extracted_text", jobID)
		log.Warn().Str("original_key", originalKey).Str("fallback_key", baseKey).Msg("original key missing _original suffix, using fallback")
	}
	v1Key := baseKey + "_v1"

	// Create S3 client
	s3Client, err := storage.NewS3Client(ctx, bucket)
	if err != nil {
		return "", fmt.Errorf("failed to create S3 client: %w", err)
	}

	// Download original file metadata to preserve Ghost Server fields
	var originalMetadata *storage.FileMetadata
	if originalKey != "" {
		_, origMeta, err := s3Client.DownloadFile(ctx, originalKey, password)
		if err != nil {
			log.Warn().Err(err).Str("original_key", originalKey).Msg("failed to get original metadata for v1, using defaults")
		} else {
			originalMetadata = origMeta
		}
	}

	// Prepare metadata for v1 upload (preserve original name)
	originalName := "extracted_text_v1.txt"
	if originalMetadata != nil && originalMetadata.OriginalName != "" {
		originalName = originalMetadata.OriginalName
	}

	metadata := &storage.FileMetadata{
		OriginalName: originalName,
		ContentType:  "application/json", // Ghost Server expects application/json
		Size:         int64(len(text)),
		Metadata:     make(map[string]string),
	}

	// Copy ALL metadata from original file (Ghost Server compatibility)
	if originalMetadata != nil && originalMetadata.Metadata != nil {
		for k, v := range originalMetadata.Metadata {
			metadata.Metadata[k] = v
		}
	}

	// Override with v1-specific fields
	metadata.Metadata["job_id"] = jobID
	metadata.Metadata["version"] = "1"
	metadata.Metadata["source"] = "mupdf_extraction"
	metadata.Metadata["format"] = "plain_text"
	metadata.Metadata["created"] = time.Now().UTC().Format(time.RFC3339)

	// Upload to S3 as _v1
	data := []byte(text)
	if err := s3Client.UploadFile(ctx, v1Key, data, password, metadata); err != nil {
		return "", fmt.Errorf("failed to upload MuPDF v1 to S3: %w", err)
	}

	log.Info().
		Str("job_id", jobID).
		Str("v1_key", v1Key).
		Int("size", len(text)).
		Msg("uploaded MuPDF text as version 1 to S3")

	// Promote v1 to base key (same as v2 does) for Ghost Server compatibility
	baseMeta := map[string]string{
		"name":              metadata.OriginalName,
		"content-type":      metadata.ContentType,
		"encrypted":         "true",
		"encryption-format": "3NCR0PTD",
		"job_id":            jobID,
		"version":           "1",
		"source":            "mupdf_extraction",
		"format":            "plain_text",
		"created":           metadata.Metadata["created"],
		"promoted_from":     v1Key,
	}

	// Copy all other metadata from v1
	for k, v := range metadata.Metadata {
		if _, exists := baseMeta[k]; !exists {
			baseMeta[k] = v
		}
	}

	if err := s3Client.CopyObjectWithMetadata(ctx, v1Key, baseKey, baseMeta); err != nil {
		log.Warn().Err(err).Str("src", v1Key).Str("dst", baseKey).Msg("failed to promote v1 to base key")
		// Return v1 URL even if promotion fails
		return fmt.Sprintf("s3://%s/%s", bucket, v1Key), nil
	}

	log.Info().
		Str("job_id", jobID).
		Str("versioned_key", v1Key).
		Str("base_key", baseKey).
		Msg("promoted MuPDF v1 to base key")

	// Return base key URL (promoted as latest)
	s3URL := fmt.Sprintf("s3://%s/%s", bucket, baseKey)
	return s3URL, nil
}

// uploadAITextAsV2 uploads aggregated AI text as version 2 to S3 and promotes to base key
// Returns: s3URL (base key, promoted as latest), error
func uploadAITextAsV2(ctx context.Context, queue QueueChecker, originalRef, jobID, text, password string) (string, error) {
	// Check if job was cancelled before uploading
	if queue != nil {
		if cancelled, err := queue.IsCancelled(ctx, jobID); err == nil && cancelled {
			log.Warn().Str("job_id", jobID).Msg("job is cancelled; aborting AI v2 upload")
			return "", fmt.Errorf("job %s was cancelled; AI v2 upload aborted", jobID)
		}
	}

	bucket := os.Getenv("AWS_S3_BUCKET")
	if bucket == "" {
		bucket = "junior-files-dev" // default bucket
	}

	// Extract bucket and original key from originalRef
	var originalKey string
	if strings.HasPrefix(originalRef, "s3://") {
		path := strings.TrimPrefix(originalRef, "s3://")
		parts := strings.SplitN(path, "/", 2)
		if len(parts) == 2 && parts[0] != "" {
			bucket = parts[0]
			originalKey = parts[1]
		}
	}

	// Generate v2 key and base key
	baseKey := strings.TrimSuffix(originalKey, "_original")
	if baseKey == "" || baseKey == originalKey {
		// Fallback if no _original suffix found
		baseKey = fmt.Sprintf("results/%s/extracted_text", jobID)
		log.Warn().Str("original_key", originalKey).Str("fallback_key", baseKey).Msg("original key missing _original suffix, using fallback")
	}
	v2Key := baseKey + "_v2"

	// Create S3 client
	s3Client, err := storage.NewS3Client(ctx, bucket)
	if err != nil {
		return "", fmt.Errorf("failed to create S3 client: %w", err)
	}

	// Prepare metadata for v2 upload
	metadata := &storage.FileMetadata{
		OriginalName: "extracted_text_v2.txt",
		ContentType:  "application/json", // Ghost Server expects application/json
		Size:         int64(len(text)),
		Metadata: map[string]string{
			"job_id":  jobID,
			"version": "2",
			"source":  "ai_extraction",
			"format":  "plain_text",
			"created": time.Now().UTC().Format(time.RFC3339),
		},
	}

	// Upload to S3 as _v2
	data := []byte(text)
	if err := s3Client.UploadFile(ctx, v2Key, data, password, metadata); err != nil {
		return "", fmt.Errorf("failed to upload AI v2 to S3: %w", err)
	}

	log.Info().
		Str("job_id", jobID).
		Str("v2_key", v2Key).
		Int("size", len(text)).
		Msg("uploaded AI text as version 2 to S3")

	// Promote v2 to base key (latest version)
	baseMeta := map[string]string{
		"name":              "extracted_text.txt",
		"content-type":      metadata.ContentType,
		"encrypted":         "true",
		"encryption-format": "3NCR0PTD",
		"job_id":            jobID,
		"version":           "2",
		"source":            "ai_extraction",
		"format":            "plain_text",
		"created":           metadata.Metadata["created"],
		"promoted_from":     v2Key,
	}

	if err := s3Client.CopyObjectWithMetadata(ctx, v2Key, baseKey, baseMeta); err != nil {
		log.Warn().Err(err).Str("src", v2Key).Str("dst", baseKey).Msg("failed to promote v2 to base key")
		// Return v2 URL even if promotion fails
		return fmt.Sprintf("s3://%s/%s", bucket, v2Key), nil
	}

	log.Info().
		Str("job_id", jobID).
		Str("base_key", baseKey).
		Msg("promoted AI v2 to base key as latest version")

	// Return base key URL (promoted latest)
	s3URL := fmt.Sprintf("s3://%s/%s", bucket, baseKey)
	return s3URL, nil
}

// SaveAggregatedTextToS3 uploads the aggregated text to S3 (encrypted) and returns the s3:// URL.
// It derives the output location as: results/{jobID}/extracted_text.txt in the same bucket.
// NOTE: File is encrypted using the same password used to download the original file.
// NOTE: Preserves original metadata from source file for Ghost Server compatibility.
// NOTE: Checks if job is cancelled before uploading to prevent uploading cancelled jobs.
func SaveAggregatedTextToS3(ctx context.Context, queue QueueChecker, originalRef, jobID, text, password string) (string, error) {
    // Check if job was cancelled before uploading to S3
    if queue != nil {
        if cancelled, err := queue.IsCancelled(ctx, jobID); err == nil && cancelled {
            log.Warn().Str("job_id", jobID).Msg("job is cancelled; aborting S3 upload")
            return "", fmt.Errorf("job %s was cancelled; S3 upload aborted", jobID)
        }
    }
    bucket := os.Getenv("AWS_S3_BUCKET")
    if bucket == "" {
        bucket = "junior-files-dev" // default bucket
    }

    // Extract bucket and original key from originalRef
    var originalKey string
    if strings.HasPrefix(originalRef, "s3://") {
        path := strings.TrimPrefix(originalRef, "s3://")
        parts := strings.SplitN(path, "/", 2)
        if len(parts) == 2 && parts[0] != "" {
            bucket = parts[0]
            originalKey = parts[1]
        }
    }

    // Generate output key by removing _original suffix (Ghost Server compatibility)
    // Ghost Server expects processed files at same location without _original suffix
    key := strings.TrimSuffix(originalKey, "_original")
    if key == "" || key == originalKey {
        // Fallback if no _original suffix found
        key = fmt.Sprintf("results/%s/extracted_text.txt", jobID)
        log.Warn().Str("original_key", originalKey).Str("fallback_key", key).Msg("original key missing _original suffix, using fallback")
    }

    // Create S3 client with encryption support
    s3Client, err := storage.NewS3Client(ctx, bucket)
    if err != nil {
        return "", fmt.Errorf("failed to create S3 client: %w", err)
    }

    // Download original file to get its metadata (Ghost Server fields)
    var originalMetadata *storage.FileMetadata
    if originalKey != "" {
        _, origMeta, err := s3Client.DownloadFile(ctx, originalKey, password)
        if err != nil {
            log.Warn().Err(err).Str("original_key", originalKey).Msg("failed to get original metadata, using defaults")
        } else {
            originalMetadata = origMeta
        }
    }

    // Prepare metadata with Ghost Server compatibility
    // Start with original filename from source metadata if available
    originalName := "extracted_text.txt"
    if originalMetadata != nil && originalMetadata.OriginalName != "" {
        originalName = originalMetadata.OriginalName
    }

    metadata := &storage.FileMetadata{
        OriginalName: originalName,
        ContentType:  "application/json", // Ghost Server expects application/json
        Size:         int64(len(text)),
        Metadata:     make(map[string]string),
    }

    // Copy ALL metadata from original file first (Ghost Server compatibility)
    if originalMetadata != nil && originalMetadata.Metadata != nil {
        for k, v := range originalMetadata.Metadata {
            metadata.Metadata[k] = v
        }
    }

    // Add/override processing-specific fields
    metadata.Metadata["job_id"] = jobID
    metadata.Metadata["source"] = "mupdf_extraction"
    metadata.Metadata["format"] = "plain_text"
    metadata.Metadata["created"] = time.Now().UTC().Format(time.RFC3339)
    metadata.Metadata["body_tokens"] = fmt.Sprintf("%d", len(text)/4)

    // Upload encrypted file to S3 (with optional versioning)
    data := []byte(text)

    // DEBUG: Log what we're about to save
    log.Debug().
        Str("job_id", jobID).
        Str("base_key", key).
        Int("text_length", len(text)).
        Int("data_length", len(data)).
        Bool("has_password", password != "").
        Int("password_length", len(password)).
        Str("text_preview", truncate(text, 200)).
        Msg("preparing to upload text to S3")

    versioningEnabled := strings.ToLower(os.Getenv("RESULT_VERSIONING_ENABLED")) == "true"
    if versioningEnabled {
        // Compute next version key
        n, err := s3Client.ListNextVersion(ctx, key)
        if err != nil { log.Warn().Err(err).Str("base_key", key).Msg("failed to list next version; defaulting to v1") }
        if n <= 0 { n = 1 }
        versionedKey := fmt.Sprintf("%s_v%d", key, n)

        // Upload to versioned key
        if err := s3Client.UploadFile(ctx, versionedKey, data, password, metadata); err != nil {
            return "", fmt.Errorf("failed to upload versioned object to S3: %w", err)
        }

        // Build base metadata for promotion (REPLACE)
        baseMeta := map[string]string{
            "name":               metadata.OriginalName,
            "content-type":       metadata.ContentType,
            "encrypted":          "true",
            "encryption-format":  firstNonEmpty(metadata.EncryptionFormat, "3NCR0PTD"),
            "pgw_version":        "latest",
            "source_version":     versionedKey,
        }
        for k, v := range metadata.Metadata { baseMeta[k] = v }

        // Promote to base key with replaced metadata
        if err := s3Client.CopyObjectWithMetadata(ctx, versionedKey, key, baseMeta); err != nil {
            log.Warn().Err(err).Str("src", versionedKey).Str("dst", key).Msg("promotion to base failed; keeping versioned object only")
        } else {
            log.Info().Str("versioned_key", versionedKey).Str("base_key", key).Msg("uploaded versioned object and promoted to base")
        }

        s3URL := fmt.Sprintf("s3://%s/%s", bucket, key)
        return s3URL, nil
    }

    // Legacy behavior: upload directly to base key
    if err := s3Client.UploadFile(ctx, key, data, password, metadata); err != nil {
        return "", fmt.Errorf("failed to upload to S3: %w", err)
    }
    s3URL := fmt.Sprintf("s3://%s/%s", bucket, key)
    log.Info().Str("s3_url", s3URL).Int("size", len(text)).Msg("uploaded encrypted text result to S3 with Ghost Server metadata (no versioning)")
    return s3URL, nil
}

// truncate returns first n chars of string with ellipsis if truncated
func truncate(s string, n int) string {
    if len(s) <= n {
        return s
    }
    return s[:n] + "..."
}

// firstNonEmpty returns first non-empty string among args, or empty string if none
func firstNonEmpty(values ...string) string {
    for _, v := range values {
        if strings.TrimSpace(v) != "" { return v }
    }
    return ""
}
