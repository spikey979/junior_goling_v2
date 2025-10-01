package orchestrator

import (
    "context"
    "fmt"
    "os"
    "path/filepath"
)

// SaveAggregatedTextToLocal stores aggregated text to a local results directory and
// returns the local filesystem path. Directory defaults to ./uploads/results unless RESULT_DIR is set.
func SaveAggregatedTextToLocal(ctx context.Context, jobID, text string) (string, error) {
    dir := os.Getenv("RESULT_DIR")
    if dir == "" { dir = filepath.Join("uploads", "results") }
    if err := os.MkdirAll(dir, 0o755); err != nil { return "", err }
    p := filepath.Join(dir, fmt.Sprintf("%s_extracted_text.txt", jobID))
    if err := os.WriteFile(p, []byte(text), 0o644); err != nil { return "", err }
    return p, nil
}

