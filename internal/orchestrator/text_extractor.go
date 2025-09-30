package orchestrator

import (
    "context"
    "fmt"
    "io"
    "net/http"
    "os"
    "strings"

    fitz "github.com/gen2brain/go-fitz"
)

// ExtractPageText uses go-fitz (MuPDF) to extract text for a given page (1-based page index).
func ExtractPageText(ctx context.Context, fileRef string, page int) (string, error) {
    // ensure local PDF path
    localPath, tmp, err := ensureLocalPDF(ctx, fileRef)
    if err != nil { return "", err }
    if tmp != "" { defer os.Remove(tmp) }

    doc, err := fitz.New(localPath)
    if err != nil { return "", fmt.Errorf("open pdf: %w", err) }
    defer doc.Close()

    idx := page - 1
    if idx < 0 { idx = 0 }
    if idx >= doc.NumPage() { idx = doc.NumPage() - 1 }
    text, err := doc.Text(idx)
    if err != nil { return "", fmt.Errorf("text page %d: %w", page, err) }
    return text, nil
}

// ensureLocalPDF returns a local file path for a PDF referenced by fileRef and an optional temp path to remove.
func ensureLocalPDF(ctx context.Context, ref string) (string, string, error) {
    if i := strings.Index(ref, "#"); i >= 0 { ref = ref[:i] }
    switch {
    case strings.HasPrefix(ref, "file://"):
        return strings.TrimPrefix(ref, "file://"), "", nil
    case strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://"):
        p, err := downloadHTTPToTemp(ctx, ref)
        return p, p, err
    case strings.HasPrefix(ref, "s3://"):
        p, err := downloadS3ToTemp(ctx, ref)
        return p, p, err
    default:
        return ref, "", nil
    }
}

// duplicate lightweight HTTP downloader to avoid cross-file export
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

