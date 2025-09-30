package orchestrator

import (
    "context"
    "fmt"
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

// downloadHTTPToTemp provided in pagecount.go within same package
