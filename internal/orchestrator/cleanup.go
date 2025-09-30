package orchestrator

import (
    "os"
    "path/filepath"
    "time"
)

// CleanupTemps removes known temporary files created during processing
// older than the provided age threshold. It targets names created by
// our helpers (pdfdl-*.pdf, s3pdf-*.pdf).
func CleanupTemps(maxAge time.Duration) {
    dir := os.TempDir()
    now := time.Now()
    _ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
        if err != nil || info == nil || info.IsDir() { return nil }
        name := info.Name()
        if !(hasPrefix(name, "pdfdl-") || hasPrefix(name, "s3pdf-")) {
            return nil
        }
        if now.Sub(info.ModTime()) >= maxAge {
            _ = os.Remove(path)
        }
        return nil
    })
}

func hasPrefix(s, p string) bool {
    if len(s) < len(p) { return false }
    return s[:len(p)] == p
}

