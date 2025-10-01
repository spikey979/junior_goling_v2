package orchestrator

import (
    "context"
    "fmt"
    "io"
    "net/http"
    "os"
    "path/filepath"
    "strings"

    "github.com/pdfcpu/pdfcpu/pkg/api"
    "github.com/rs/zerolog/log"

    "github.com/local/aidispatcher/internal/storage"
)

// DetermineTotalPages returns the number of pages for a PDF referenced by ref.
// Supports:
// - file://path or absolute/relative filesystem paths
// - http(s):// URLs (downloads to temp)
// - s3://bucket/key (downloads to temp via AWS SDK v2)
func DetermineTotalPages(ctx context.Context, ref string) (int, error) {
    // Strip optional #page fragment if present
    if i := strings.Index(ref, "#"); i >= 0 {
        ref = ref[:i]
    }

    var localPath string
    var tmpToRemove string
    var err error

    switch {
    case strings.HasPrefix(ref, "s3://"):
        localPath, err = downloadS3ToTemp(ctx, ref, "") // TODO: pass password if needed
        tmpToRemove = localPath
    case strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://"):
        localPath, err = downloadHTTPToTemp(ctx, ref)
        tmpToRemove = localPath
    case strings.HasPrefix(ref, "file://"):
        localPath = strings.TrimPrefix(ref, "file://")
    default:
        // treat as filesystem path
        localPath = ref
    }
    if err != nil {
        return 0, err
    }
    if tmpToRemove != "" {
        defer os.Remove(tmpToRemove)
    }

    n, err := api.PageCountFile(localPath)
    if err != nil {
        return 0, fmt.Errorf("pdf page count failed: %w", err)
    }
    return n, nil
}

func downloadHTTPToTemp(ctx context.Context, url string) (string, error) {
    req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
    resp, err := http.DefaultClient.Do(req)
    if err != nil { return "", err }
    defer resp.Body.Close()
    if resp.StatusCode != 200 { return "", fmt.Errorf("http %d", resp.StatusCode) }
    f, err := os.CreateTemp("", "pdfdl-*.pdf")
    if err != nil { return "", err }
    defer f.Close()
    if _, err := io.Copy(f, resp.Body); err != nil { return "", err }
    return f.Name(), nil
}

func downloadS3ToTemp(ctx context.Context, s3url, password string) (string, error) {
    // s3://bucket/key
    path := strings.TrimPrefix(s3url, "s3://")
    slash := strings.Index(path, "/")
    if slash <= 0 { return "", fmt.Errorf("invalid s3 url: %s", s3url) }
    bucket := path[:slash]
    key := path[slash+1:]

    // Create S3 client with decryption support
    s3Client, err := storage.NewS3Client(ctx, bucket)
    if err != nil { return "", fmt.Errorf("failed to create S3 client: %w", err) }

    // Download and decrypt file
    data, metadata, err := s3Client.DownloadFile(ctx, key, password)
    if err != nil { return "", fmt.Errorf("failed to download from S3: %w", err) }

    // Determine filename from metadata
    filename := metadata.OriginalName
    if filename == "" {
        // Fallback: extract from S3 key
        parts := strings.Split(key, "/")
        if len(parts) > 0 {
            filename = parts[len(parts)-1]
            filename = strings.TrimSuffix(filename, "_original")
        }
    }

    // Create temp file with determined filename
    // Use pattern that preserves extension for proper file type detection
    var f *os.File
    if filename != "" && strings.Contains(filename, ".") {
        // Has extension - use it to help with file type detection
        ext := filepath.Ext(filename)
        pattern := fmt.Sprintf("s3download-*%s", ext)
        f, err = os.CreateTemp("", pattern)
    } else {
        // No extension - let file type detector rely on magic bytes only
        f, err = os.CreateTemp("", "s3download-*")
    }
    if err != nil { return "", fmt.Errorf("failed to create temp file: %w", err) }
    defer f.Close()

    if _, err := f.Write(data); err != nil {
        os.Remove(f.Name())
        return "", fmt.Errorf("failed to write temp file: %w", err)
    }

    log.Info().
        Str("bucket", bucket).
        Str("key", key).
        Str("original_name", filename).
        Str("temp_file", filepath.Base(f.Name())).
        Str("encryption_format", metadata.EncryptionFormat).
        Int("size", len(data)).
        Msg("downloaded and decrypted s3 file to temp")

    return f.Name(), nil
}

