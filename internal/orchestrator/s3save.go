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

// SaveAggregatedTextToS3 uploads the aggregated text to S3 (encrypted) and returns the s3:// URL.
// It derives the output location as: results/{jobID}/extracted_text.txt in the same bucket.
// NOTE: File is encrypted using the same password used to download the original file.
// NOTE: Preserves original metadata from source file for Ghost Server compatibility.
func SaveAggregatedTextToS3(ctx context.Context, originalRef, jobID, text, password string) (string, error) {
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
