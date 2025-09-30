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

    awscfg "github.com/aws/aws-sdk-go-v2/config"
    "github.com/aws/aws-sdk-go-v2/service/s3"
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
        localPath, err = downloadS3ToTemp(ctx, ref)
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

func downloadS3ToTemp(ctx context.Context, s3url string) (string, error) {
    // s3://bucket/key
    path := strings.TrimPrefix(s3url, "s3://")
    slash := strings.Index(path, "/")
    if slash <= 0 { return "", fmt.Errorf("invalid s3 url: %s", s3url) }
    bucket := path[:slash]
    key := path[slash+1:]

    // Load AWS config (region from env or default chain)
    cfg, err := awscfg.LoadDefaultConfig(ctx)
    if err != nil { return "", err }
    cli := s3.NewFromConfig(cfg)

    out, err := cli.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: &key})
    if err != nil { return "", err }
    defer out.Body.Close()

    // Ensure .pdf extension for pdfcpu expectations
    f, err := os.CreateTemp("", "s3pdf-*.pdf")
    if err != nil { return "", err }
    defer f.Close()
    if _, err := io.Copy(f, out.Body); err != nil { return "", err }
    log.Info().Str("bucket", bucket).Str("key", key).Str("file", filepath.Base(f.Name())).Msg("downloaded s3 pdf to temp")
    return f.Name(), nil
}
