package orchestrator

import (
    "context"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "os"
    "path/filepath"
    "strconv"
    "strings"
    "time"

    "github.com/google/uuid"
    "github.com/local/aidispatcher/internal/converter"
    "github.com/local/aidispatcher/internal/filetype"
    "github.com/local/aidispatcher/internal/mupdf"
    "github.com/rs/zerolog/log"
)

type Queue interface {
    EnqueueAI(ctx context.Context, payload []byte) error
    CancelJob(ctx context.Context, jobID string) error
}

type Status struct {
    Status   string
    Progress int
    Message  string
    Start    *time.Time
    End      *time.Time
    Metadata map[string]any
}

type StatusStore interface {
    Set(ctx context.Context, jobID string, st Status) error
    Get(ctx context.Context, jobID string) (Status, bool, error)
    SetFileJobMapping(ctx context.Context, fileID, jobID string) error
    GetJobByFileID(ctx context.Context, fileID string) (string, error)
}

type Dependencies struct {
    Queue     Queue
    Status    StatusStore
    Pages     PageStore
    Converter *converter.LibreOffice
    FileType  *filetype.Detector
}

type Orchestrator struct {
    deps Dependencies
}

func New(deps Dependencies) *Orchestrator {
    return &Orchestrator{deps: deps}
}

type PageStore interface {
    SavePageText(ctx context.Context, jobID string, page int, text, source, provider, model string) error
    GetPageText(ctx context.Context, jobID string, page int) (string, error)
    AggregateText(ctx context.Context, jobID string, total int) (string, error)
}

func (o *Orchestrator) RegisterRoutes(mux *http.ServeMux) {
    mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request){ w.WriteHeader(http.StatusOK); _,_ = w.Write([]byte("ok")) })
    mux.HandleFunc("/process_file_junior_call", o.handleProcess)
    mux.HandleFunc("/process_file_upload", o.handleProcessUpload)
    mux.HandleFunc("/progress_spec/", o.handleProgress)
    mux.HandleFunc("/download_result/", o.handleDownloadResult)
    mux.HandleFunc("/internal/job_done", o.handleJobDone)
    mux.HandleFunc("/webhook/cancel_job", o.handleCancelJob)
    mux.HandleFunc("/internal/page_done", o.handlePageDone)
    mux.HandleFunc("/internal/page_failed", o.handlePageFailed)
}

type processReq struct {
    FilePath   string                 `json:"file_path"`
    FileURL    string                 `json:"file_url"`
    UserName   string                 `json:"user_name"`
    UserID     string                 `json:"user_id"`
    Password   string                 `json:"password"`
    AIPrompt   string                 `json:"ai_prompt"`
    AIEngine   string                 `json:"ai_engine"`
    TextOnly   bool                   `json:"text_only"`
    FastUpload bool                   `json:"fast_upload"`
    Options    map[string]interface{} `json:"options"`
    Source     string                 `json:"source"`
}

type processResp struct {
    Status        string                 `json:"status"`
    JobID         string                 `json:"job_id"`
    Message       string                 `json:"message"`
    EstimatedTime int                    `json:"estimated_time_seconds,omitempty"`
    QueuePosition int                    `json:"queue_position,omitempty"`
    Metadata      map[string]interface{} `json:"metadata,omitempty"`
}

// extractFileIDFromS3Path extracts the file_id (last part of path, without _original suffix)
// from an S3 URL like s3://bucket/user/folder/file_id_original
func extractFileIDFromS3Path(s3Path string) string {
    // Remove s3:// prefix and bucket name
    path := strings.TrimPrefix(s3Path, "s3://")
    // Find first slash (after bucket name)
    if idx := strings.Index(path, "/"); idx > 0 {
        path = path[idx+1:] // Remove bucket name
    }

    // Get last part of path
    parts := strings.Split(path, "/")
    if len(parts) == 0 {
        return ""
    }
    fileID := parts[len(parts)-1]

    // Remove _original suffix if present
    fileID = strings.TrimSuffix(fileID, "_original")

    return fileID
}

func (o *Orchestrator) handleProcess(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        w.WriteHeader(http.StatusMethodNotAllowed); return
    }
    defer r.Body.Close()
    var req processReq
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        http.Error(w, "invalid json", http.StatusBadRequest); return
    }

    // sanitize + normalize
    filePath := req.FilePath
    if filePath == "" { filePath = req.FileURL }
    user := req.UserName
    if user == "" { user = req.UserID }
    if filePath == "" || user == "" {
        http.Error(w, "missing file_path/file_url or user_name/user_id", http.StatusBadRequest); return
    }
    if !strings.HasPrefix(filePath, "s3://") && !strings.HasPrefix(filePath, "http://") && !strings.HasPrefix(filePath, "https://") {
        bucket := os.Getenv("AWS_S3_BUCKET")
        if bucket == "" { bucket = "junior-files-dev" }
        filePath = fmt.Sprintf("s3://%s/%s", bucket, filePath)
    }

    jobID := uuid.NewString()
    log.Info().Str("job_id", jobID).Str("file", filePath).Str("user", user).Msg("job created")
    start := time.Now()
    _ = o.deps.Status.Set(r.Context(), jobID, Status{Status: "queued", Progress: 0, Message: "queued", Start: &start,
        Metadata: map[string]any{"file_path": filePath, "user": user}})

    // Extract file_id from S3 path and create file-to-job mapping
    // Ghost Server uses file_id (with or without _original suffix) to check progress
    if strings.HasPrefix(filePath, "s3://") {
        fileID := extractFileIDFromS3Path(filePath)
        if fileID != "" {
            _ = o.deps.Status.SetFileJobMapping(r.Context(), fileID, jobID)
            log.Debug().Str("file_id", fileID).Str("job_id", jobID).Msg("created file-to-job mapping")
        }
    }

    // For S3/HTTP files, we need to download and potentially convert them
    // This will be handled asynchronously in background to avoid blocking the request
    // For now, we'll process synchronously for simplicity (TODO: make async with goroutine)
    processedPath := filePath

    // Check if we need to download and convert (S3/HTTP paths)
    if strings.HasPrefix(filePath, "s3://") || strings.HasPrefix(filePath, "http://") || strings.HasPrefix(filePath, "https://") {
        // For now, pass through to DetermineTotalPages which handles S3 downloads
        // TODO: Add proper download + file type detection + conversion here
        log.Debug().Str("job_id", jobID).Str("file", filePath).Msg("S3/HTTP file - will be downloaded on-demand")
    }

    // Ako je text_only ili fast_upload, forsiraj MuPDF i preskoči AI
    if req.TextOnly || req.FastUpload {
        log.Info().Str("job_id", jobID).Str("file", processedPath).Bool("text_only", req.TextOnly).Bool("fast_upload", req.FastUpload).Msg("Processing with MuPDF text-only mode (S3)")

        // Download file from S3, convert if needed, then process with MuPDF
        go o.processMuPDFOnlyFromS3(context.Background(), jobID, processedPath, user, req.Password)

        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(http.StatusCreated)
        _ = json.NewEncoder(w).Encode(processResp{Status: "ok", JobID: jobID, Message: "Text extraction started (MuPDF only)"})
        return
    }

    // Odredi broj stranica (pdfcpu) i napravi selekciju
    pages, err := DetermineTotalPages(r.Context(), processedPath)
    if err != nil {
        log.Warn().Err(err).Str("file", filePath).Msg("page count failed; defaulting to 4")
        pages = 4
    }
    log.Info().Str("job_id", jobID).Str("file", filePath).Int("total_pages", pages).Msg("orchestrator detected page count")
    sel := SelectPages(SelectionOptions{TextOnly: req.TextOnly, TotalPages: pages})
    log.Info().Str("job_id", jobID).Int("ai_pages", len(sel.AIPages)).Int("mupdf_pages", len(sel.MuPDFPages)).Msg("orchestrator allocated pages")
    // enqueue AI stranice
    for _, p := range sel.AIPages {
        payload := map[string]any{
            "job_id": jobID,
            "file_path": filePath,
            "page_id": p,
            "content_ref": fmt.Sprintf("%s#page=%d", filePath, p),
            "user": user,
            "ai_engine": req.AIEngine,
            "text_only": req.TextOnly,
            "idempotency_key": fmt.Sprintf("doc:%s:page:%d", jobID, p),
            "attempt": 1,
        }
        if req.Source != "" { payload["source"] = req.Source } else { payload["source"] = "api" }
        data, _ := json.Marshal(payload)
        if err := o.deps.Queue.EnqueueAI(r.Context(), data); err != nil {
            log.Error().Err(err).Msg("enqueue failed")
            http.Error(w, "queue unavailable", http.StatusServiceUnavailable)
            return
        }
        log.Info().Str("job_id", jobID).Int("page_id", p).Str("ai_engine", req.AIEngine).Msg("enqueued page for AI")
    }
    // update status
    _ = o.deps.Status.Set(r.Context(), jobID, Status{Status: "processing", Progress: 10, Message: "enqueued AI pages",
        Metadata: map[string]any{"total_pages": pages, "ai_pages": len(sel.AIPages), "mupdf_pages": len(sel.MuPDFPages), "pages_done": 0, "pages_failed": 0}})

    resp := processResp{
        Status:  "ok",
        JobID:   jobID,
        Message: "File processing job created successfully",
        Metadata: map[string]any{"ai_engine": req.AIEngine, "timestamp": time.Now().Format(time.RFC3339)},
    }
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(http.StatusCreated)
    _ = json.NewEncoder(w).Encode(resp)
}

// handleProcessUpload accepts multipart/form-data uploads from dashboard.
// For text_only mode, it directly processes with MuPDF without queueing.
// Otherwise, it enqueues work for AI processing.
func (o *Orchestrator) handleProcessUpload(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost { w.WriteHeader(http.StatusMethodNotAllowed); return }
    // Expect multipart with fields: file (required), user_name, ai_engine, text_only
    if err := r.ParseMultipartForm(64 << 20); err != nil { // 64MB max memory before temp files
        http.Error(w, "invalid multipart form", http.StatusBadRequest); return
    }
    file, hdr, err := r.FormFile("file")
    if err != nil { http.Error(w, "missing file", http.StatusBadRequest); return }
    defer file.Close()
    user := r.FormValue("user_name")
    if user == "" { http.Error(w, "missing user_name", http.StatusBadRequest); return }
    aiEngine := r.FormValue("ai_engine")
    textOnly := r.FormValue("text_only") == "on" || r.FormValue("text_only") == "true"

    // Persist upload to local storage
    uploadDir := os.Getenv("UPLOAD_DIR")
    if uploadDir == "" { uploadDir = "uploads" }
    if err := os.MkdirAll(uploadDir, 0o755); err != nil { http.Error(w, "cannot create upload dir", 500); return }
    jobID := uuid.NewString()
    // derive filename with job prefix to avoid collisions
    name := hdr.Filename
    if name == "" { name = "upload.pdf" }
    localPath := fmt.Sprintf("%s/%s_%s", strings.TrimRight(uploadDir, "/"), jobID, name)
    out, err := os.Create(localPath)
    if err != nil { http.Error(w, "cannot save upload", 500); return }
    if _, err := io.Copy(out, file); err != nil { out.Close(); http.Error(w, "write failed", 500); return }
    _ = out.Close()

    // Initialize status
    start := time.Now()
    _ = o.deps.Status.Set(r.Context(), jobID, Status{Status: "queued", Progress: 0, Message: "queued",
        Start: &start, Metadata: map[string]any{"file_local": localPath, "user": user, "source": "upload"}})

    // Detect file type
    fileInfo, err := o.deps.FileType.Detect(localPath)
    if err != nil {
        log.Error().Err(err).Str("file", localPath).Msg("failed to detect file type")
        http.Error(w, fmt.Sprintf("file type detection failed: %v", err), http.StatusBadRequest)
        return
    }

    if !fileInfo.Supported {
        log.Warn().Str("mime", fileInfo.MIMEType).Str("file", localPath).Msg("unsupported file type")
        http.Error(w, fmt.Sprintf("unsupported file type: %s", fileInfo.Description), http.StatusBadRequest)
        return
    }

    log.Info().Str("job_id", jobID).Str("mime", fileInfo.MIMEType).Str("desc", fileInfo.Description).Msg("detected file type")

    // Convert to PDF if needed (Office formats)
    pdfPath := localPath
    if fileInfo.MIMEType != "application/pdf" && !fileInfo.IsText {
        log.Info().Str("job_id", jobID).Str("file", localPath).Msg("converting to PDF with LibreOffice")
        _ = o.deps.Status.Set(r.Context(), jobID, Status{Status: "processing", Progress: 5, Message: "converting to PDF",
            Start: &start, Metadata: map[string]any{"file_local": localPath, "user": user, "source": "upload", "original_mime": fileInfo.MIMEType}})

        convertedPath := filepath.Join(filepath.Dir(localPath), fmt.Sprintf("%s_converted.pdf", jobID))
        convJob := converter.Job{
            InputPath:  localPath,
            OutputPath: convertedPath,
            Extension:  fileInfo.Extension,
            Timeout:    180 * time.Second,
        }

        result := o.deps.Converter.ConvertToPDF(convJob)
        if !result.Success {
            log.Error().Str("job_id", jobID).Str("error", result.Error).Msg("conversion failed")
            _ = o.deps.Status.Set(r.Context(), jobID, Status{Status: "failed", Progress: 0, Message: fmt.Sprintf("conversion failed: %s", result.Error),
                Start: &start, End: &start, Metadata: map[string]any{"file_local": localPath, "user": user, "source": "upload", "error": result.Error}})
            http.Error(w, fmt.Sprintf("conversion failed: %s", result.Error), http.StatusInternalServerError)
            return
        }

        pdfPath = result.OutputPath
        log.Info().Str("job_id", jobID).Str("pdf", pdfPath).Dur("duration", result.Duration).Msg("conversion successful")
    }

    // Check if text_only mode is enabled
    if textOnly {
        log.Info().Str("job_id", jobID).Str("file", pdfPath).Msg("Processing with MuPDF text-only mode")

        // Process asynchronously with MuPDF (use background context to avoid cancellation when request ends)
        go o.processMuPDFOnly(context.Background(), jobID, pdfPath)

        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(http.StatusCreated)
        _ = json.NewEncoder(w).Encode(processResp{Status: "ok", JobID: jobID, Message: "Text extraction started"})
        return
    }

    // Page count and selection for AI mode
    fileRef := "file://" + pdfPath
    pages, err := DetermineTotalPages(r.Context(), fileRef)
    if err != nil {
        log.Warn().Err(err).Str("file", fileRef).Msg("upload page count failed; defaulting to 1")
        pages = 1
    }
    log.Info().Str("job_id", jobID).Str("file", fileRef).Int("total_pages", pages).Msg("orchestrator detected upload page count")
    sel := SelectPages(SelectionOptions{TextOnly: textOnly, TotalPages: pages})
    log.Info().Str("job_id", jobID).Int("ai_pages", len(sel.AIPages)).Int("mupdf_pages", len(sel.MuPDFPages)).Msg("orchestrator allocated upload pages")

    // Enqueue AI pages
    for _, p := range sel.AIPages {
        payload := map[string]any{
            "job_id": jobID,
            "file_path": fileRef,
            "page_id": p,
            "content_ref": fmt.Sprintf("%s#page=%d", fileRef, p),
            "user": user,
            "ai_engine": aiEngine,
            "text_only": textOnly,
            "source": "upload",
            "idempotency_key": fmt.Sprintf("doc:%s:page:%d", jobID, p),
            "attempt": 1,
        }
        data, _ := json.Marshal(payload)
        if err := o.deps.Queue.EnqueueAI(r.Context(), data); err != nil {
            http.Error(w, "queue unavailable", http.StatusServiceUnavailable); return
        }
        log.Info().Str("job_id", jobID).Int("page_id", p).Str("ai_engine", aiEngine).Msg("enqueued upload page for AI")
    }

    _ = o.deps.Status.Set(r.Context(), jobID, Status{Status: "processing", Progress: 10,
        Message: "enqueued AI pages", Metadata: map[string]any{"total_pages": pages, "ai_pages": len(sel.AIPages), "mupdf_pages": len(sel.MuPDFPages), "pages_done": 0, "pages_failed": 0, "file_local": localPath, "source": "upload"}})

    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(http.StatusCreated)
    _ = json.NewEncoder(w).Encode(processResp{Status: "ok", JobID: jobID, Message: "Upload job created"})
}

// handleDownloadResult serves the aggregated text for upload-origin jobs as a file download.
func (o *Orchestrator) handleDownloadResult(w http.ResponseWriter, r *http.Request) {
    id := strings.TrimPrefix(r.URL.Path, "/download_result/")
    st, ok, err := o.deps.Status.Get(r.Context(), id)
    if err != nil || !ok { http.Error(w, "not found", http.StatusNotFound); return }
    if st.Status != "success" { http.Error(w, "not ready", http.StatusAccepted); return }
    // Only for upload-origin jobs
    if st.Metadata == nil || st.Metadata["source"] != "upload" {
        http.Error(w, "not an upload job", http.StatusBadRequest); return
    }
    p, _ := st.Metadata["result_local_path"].(string)
    if p == "" { http.Error(w, "result not available", http.StatusNotFound); return }
    b, err := os.ReadFile(p)
    if err != nil { http.Error(w, "failed to read", 500); return }
    w.Header().Set("Content-Type", "text/plain; charset=utf-8")
    w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=extracted_text_%s.txt", id))
    w.Write(b)
}

func (o *Orchestrator) handleProgress(w http.ResponseWriter, r *http.Request) {
    identifier := strings.TrimPrefix(r.URL.Path, "/progress_spec/")

    // Try to get status directly first (if identifier is a job_id)
    st, ok, err := o.deps.Status.Get(r.Context(), identifier)

    // If not found, try to resolve via file-to-job mapping
    if !ok || err != nil {
        // Remove _original suffix if present (Ghost Server may send with or without it)
        normalizedID := strings.TrimSuffix(identifier, "_original")

        // Try to get job_id from file_id mapping
        if jobID, err := o.deps.Status.GetJobByFileID(r.Context(), normalizedID); err == nil {
            log.Debug().Str("file_id", identifier).Str("job_id", jobID).Msg("resolved file_id to job_id")
            // Get status using the mapped job_id
            st, ok, err = o.deps.Status.Get(r.Context(), jobID)
            if ok && err == nil {
                // Use the actual job_id in response
                identifier = jobID
            }
        }
    }

    if err != nil {
        http.Error(w, "error retrieving status", 500)
        return
    }
    if !ok {
        http.Error(w, "job or file not found", http.StatusNotFound)
        return
    }

    w.Header().Set("Content-Type", "application/json")
    _ = json.NewEncoder(w).Encode(map[string]any{
        "success":    st.Status == "success",
        "job_id":     identifier,
        "status":     st.Status,
        "progress":   st.Progress,
        "message":    st.Message,
        "start_time": st.Start,
        "end_time":   st.End,
        "metadata":   st.Metadata,
    })
}

func (o *Orchestrator) handleJobDone(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost { w.WriteHeader(http.StatusMethodNotAllowed); return }
    jobID := r.URL.Query().Get("job_id")
    if jobID == "" { http.Error(w, "missing job_id", http.StatusBadRequest); return }
    st, ok, err := o.deps.Status.Get(r.Context(), jobID)
    if err != nil { http.Error(w, "error", 500); return }
    if !ok { http.Error(w, "not found", http.StatusNotFound); return }
    now := time.Now()
    st.Status = "success"
    st.Progress = 100
    st.Message = "completed"
    st.End = &now
    _ = o.deps.Status.Set(r.Context(), jobID, st)
    log.Info().Str("job_id", jobID).Msg("job marked done via webhook")
    w.WriteHeader(http.StatusNoContent)
}

func (o *Orchestrator) handlePageDone(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost { w.WriteHeader(http.StatusMethodNotAllowed); return }
    jobID := r.URL.Query().Get("job_id")
    pageIDStr := r.URL.Query().Get("page_id")
    if jobID == "" || pageIDStr == "" { http.Error(w, "missing job_id/page_id", 400); return }
    // Parse optional body with text/provider/model
    var body struct{ Text string `json:"text"`; Provider string `json:"provider"`; Model string `json:"model"` }
    _ = json.NewDecoder(r.Body).Decode(&body)
    // Save page text if provided
    pageNum, _ := strconv.Atoi(pageIDStr)
    if body.Text != "" {
        _ = o.deps.Pages.SavePageText(r.Context(), jobID, pageNum, body.Text, "ai", body.Provider, body.Model)
    }
    st, ok, err := o.deps.Status.Get(r.Context(), jobID)
    if err != nil || !ok { w.WriteHeader(http.StatusNoContent); return }
    if st.Metadata == nil { st.Metadata = map[string]any{} }
    // update metadata counts
    done := intFromMeta(st.Metadata, "pages_done") + 1
    failed := intFromMeta(st.Metadata, "pages_failed")
    total := intFromMeta(st.Metadata, "total_pages")
    st.Metadata["pages_done"] = done
    // progress
    if total > 0 { st.Progress = int(float64(done+failed) / float64(total) * 100) }
    st.Message = fmt.Sprintf("page %s done", pageIDStr)
    log.Info().Str("job_id", jobID).Int("page_id", pageNum).Int("pages_done", done).Int("pages_failed", failed).Int("total_pages", total).Str("provider", body.Provider).Str("model", body.Model).Msg("page completed")
    // If all pages accounted, aggregate and mark success
    if total > 0 && done+failed >= total {
        agg, _ := o.deps.Pages.AggregateText(r.Context(), jobID, total)
        if st.Metadata == nil { st.Metadata = map[string]any{} }
        st.Metadata["result_text_len"] = len(agg)
        // Save result depending on source
        if src, _ := st.Metadata["source"].(string); src == "upload" {
            if localPath, err := SaveAggregatedTextToLocal(r.Context(), jobID, agg); err == nil {
                st.Metadata["result_local_path"] = localPath
                log.Info().Str("job_id", jobID).Str("result_path", localPath).Msg("aggregated result stored locally")
            }
        } else {
            // Save to S3 (encrypted)
            filePath, _ := st.Metadata["file_path"].(string)
            password, _ := st.Metadata["password"].(string)
            if s3url, err := SaveAggregatedTextToS3(r.Context(), filePath, jobID, agg, password); err == nil {
                st.Metadata["result_s3_url"] = s3url
                log.Info().Str("job_id", jobID).Str("result_s3_url", s3url).Msg("aggregated result stored to S3")
            }
        }
        st.Status = "success"
        st.Progress = 100
        // Cleanup stale temp files older than 1h as part of job completion hygiene
        CleanupTemps(1 * time.Hour)
        log.Info().Str("job_id", jobID).Int("pages_done", done).Int("pages_failed", failed).Msg("job completed")
    }
    _ = o.deps.Status.Set(r.Context(), jobID, st)
    w.WriteHeader(http.StatusNoContent)
}

func (o *Orchestrator) handlePageFailed(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost { w.WriteHeader(http.StatusMethodNotAllowed); return }
    jobID := r.URL.Query().Get("job_id")
    pageIDStr := r.URL.Query().Get("page_id")
    if jobID == "" || pageIDStr == "" { http.Error(w, "missing job_id/page_id", 400); return }
    st, ok, err := o.deps.Status.Get(r.Context(), jobID)
    if err != nil || !ok { w.WriteHeader(http.StatusNoContent); return }
    // increment failed
    if st.Metadata == nil { st.Metadata = map[string]any{} }
    done := intFromMeta(st.Metadata, "pages_done")
    failed := intFromMeta(st.Metadata, "pages_failed") + 1
    total := intFromMeta(st.Metadata, "total_pages")
    st.Metadata["pages_failed"] = failed
    // Extract MuPDF text for this page and save
    pageNum, _ := strconv.Atoi(pageIDStr)
    filePath, _ := st.Metadata["file_path"].(string)
    if filePath == "" { filePath = jobID }
    if txt, err := ExtractPageText(r.Context(), filePath, pageNum); err == nil {
        _ = o.deps.Pages.SavePageText(r.Context(), jobID, pageNum, txt, "mupdf", "", "")
    }
    if total > 0 { st.Progress = int(float64(done+failed) / float64(total) * 100) }
    st.Message = fmt.Sprintf("page %s failed (fallback to MuPDF)", pageIDStr)
    log.Warn().Str("job_id", jobID).Int("page_id", pageNum).Int("pages_done", done).Int("pages_failed", failed).Int("total_pages", total).Msg("page failed; MuPDF fallback")
    // If all pages accounted, aggregate and mark success
    if total > 0 && done+failed >= total {
        agg, _ := o.deps.Pages.AggregateText(r.Context(), jobID, total)
        st.Metadata["result_text_len"] = len(agg)
        // Save result depending on source
        if src, _ := st.Metadata["source"].(string); src == "upload" {
            if localPath, err := SaveAggregatedTextToLocal(r.Context(), jobID, agg); err == nil {
                st.Metadata["result_local_path"] = localPath
                log.Info().Str("job_id", jobID).Str("result_path", localPath).Msg("job completed with fallback (local)")
            }
        } else {
            filePath, _ := st.Metadata["file_path"].(string)
            password, _ := st.Metadata["password"].(string)
            if s3url, err := SaveAggregatedTextToS3(r.Context(), filePath, jobID, agg, password); err == nil {
                st.Metadata["result_s3_url"] = s3url
                log.Info().Str("job_id", jobID).Str("result_s3_url", s3url).Msg("job completed with fallback (S3)")
            }
        }
        st.Status = "success"
        st.Progress = 100
        log.Info().Str("job_id", jobID).Int("pages_done", done).Int("pages_failed", failed).Msg("job completed after fallback")
    }
    _ = o.deps.Status.Set(r.Context(), jobID, st)
    w.WriteHeader(http.StatusNoContent)
}

func intFromMeta(m map[string]any, key string) int {
    if m == nil { return 0 }
    if v, ok := m[key]; ok {
        switch t := v.(type) {
        case float64: return int(t)
        case int: return t
        }
    }
    return 0
}

type cancelReq struct {
    JobID  string `json:"job_id"`
    Reason string `json:"reason,omitempty"`
}

func (o *Orchestrator) handleCancelJob(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost { w.WriteHeader(http.StatusMethodNotAllowed); return }
    var req cancelReq
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil { http.Error(w, "invalid json", 400); return }
    if req.JobID == "" { http.Error(w, "missing job_id", 400); return }
    // mark cancelled in queue store
    if err := o.deps.Queue.CancelJob(r.Context(), req.JobID); err != nil {
        http.Error(w, "cancel failed", 500); return
    }
    st, ok, _ := o.deps.Status.Get(r.Context(), req.JobID)
    if !ok { st = Status{} }
    st.Status = "cancelled"
    st.Progress = 0
    if req.Reason != "" { st.Message = fmt.Sprintf("Cancelled: %s", req.Reason) } else { st.Message = "Cancelled" }
    now := time.Now(); st.End = &now
    _ = o.deps.Status.Set(r.Context(), req.JobID, st)
    _ = json.NewEncoder(w).Encode(map[string]any{"success": true, "job_id": req.JobID, "status": "cancelled"})
}

// processMuPDFOnly handles text-only extraction using MuPDF without AI
func (o *Orchestrator) processMuPDFOnly(ctx context.Context, jobID, pdfPath string) {
    startTime := time.Now()

    // Update status to processing
    _ = o.deps.Status.Set(ctx, jobID, Status{
        Status:   "processing",
        Progress: 5,
        Message:  "Starting text extraction",
        Start:    &startTime,
        Metadata: map[string]any{"file_local": pdfPath, "source": "upload", "mode": "text_only"},
    })

    // Try go-fitz first, fallback to mutool if needed
    extractor := mupdf.NewGoFitzExtractor()
    if !extractor.IsAvailable() {
        log.Warn().Msg("go-fitz not available, falling back to mutool")
        mutoolExtractor := mupdf.NewExtractor()
        if !mutoolExtractor.IsAvailable() {
            log.Error().Str("job_id", jobID).Msg("Neither go-fitz nor mutool available")
            endTime := time.Now()
            _ = o.deps.Status.Set(ctx, jobID, Status{
                Status:   "failed",
                Progress: 0,
                Message:  "MuPDF tools not available",
                Start:    &startTime,
                End:      &endTime,
                Metadata: map[string]any{"error": "MuPDF tools not installed"},
            })
            return
        }
    }

    // Get page count
    pageCount, err := extractor.GetPageCount(pdfPath)
    if err != nil {
        log.Error().Err(err).Str("job_id", jobID).Msg("Failed to get page count")
        endTime := time.Now()
        _ = o.deps.Status.Set(ctx, jobID, Status{
            Status:   "failed",
            Progress: 0,
            Message:  "Failed to read PDF",
            Start:    &startTime,
            End:      &endTime,
            Metadata: map[string]any{"error": err.Error()},
        })
        return
    }

    log.Info().Str("job_id", jobID).Int("pages", pageCount).Msg("Starting MuPDF text extraction")

    // Update progress
    _ = o.deps.Status.Set(ctx, jobID, Status{
        Status:   "processing",
        Progress: 10,
        Message:  fmt.Sprintf("Extracting text from %d pages", pageCount),
        Start:    &startTime,
        Metadata: map[string]any{
            "file_local":  pdfPath,
            "source":      "upload",
            "mode":        "text_only",
            "total_pages": pageCount,
        },
    })

    // Extract text from all pages
    var allText strings.Builder
    extractedChars := 0

    for i := 1; i <= pageCount; i++ {
        pageText, err := extractor.ExtractTextByPage(pdfPath, i)
        if err != nil {
            log.Warn().Err(err).Int("page", i).Msg("Failed to extract text from page")
            pageText = fmt.Sprintf("[Page %d extraction failed]\n", i)
        }

        // Add page separator
        if i > 1 {
            allText.WriteString("\n\n")
        }
        allText.WriteString(fmt.Sprintf("=== Page %d ===\n", i))
        allText.WriteString(pageText)
        extractedChars += len(pageText)

        // Update progress (10% to 90% for extraction)
        progress := 10 + (80 * i / pageCount)
        _ = o.deps.Status.Set(ctx, jobID, Status{
            Status:   "processing",
            Progress: progress,
            Message:  fmt.Sprintf("Processed page %d of %d", i, pageCount),
            Start:    &startTime,
            Metadata: map[string]any{
                "file_local":       pdfPath,
                "source":           "upload",
                "mode":             "text_only",
                "total_pages":      pageCount,
                "pages_processed":  i,
                "chars_extracted": extractedChars,
            },
        })

        // Check for cancellation
        select {
        case <-ctx.Done():
            log.Info().Str("job_id", jobID).Msg("Job cancelled during processing")
            endTime := time.Now()
            _ = o.deps.Status.Set(ctx, jobID, Status{
                Status:   "cancelled",
                Progress: 0,
                Message:  "Job cancelled",
                Start:    &startTime,
                End:      &endTime,
            })
            return
        default:
        }
    }

    // Save result to file
    resultDir := os.Getenv("RESULT_DIR")
    if resultDir == "" {
        resultDir = "uploads/results"
    }
    if err := os.MkdirAll(resultDir, 0o755); err != nil {
        log.Error().Err(err).Str("job_id", jobID).Msg("Failed to create result directory")
    }

    resultPath := fmt.Sprintf("%s/%s.txt", strings.TrimRight(resultDir, "/"), jobID)
    resultText := allText.String()

    if err := os.WriteFile(resultPath, []byte(resultText), 0o644); err != nil {
        log.Error().Err(err).Str("job_id", jobID).Msg("Failed to save result")
        endTime := time.Now()
        _ = o.deps.Status.Set(ctx, jobID, Status{
            Status:   "failed",
            Progress: 95,
            Message:  "Failed to save result",
            Start:    &startTime,
            End:      &endTime,
            Metadata: map[string]any{"error": err.Error()},
        })
        return
    }

    // Mark as successful
    endTime := time.Now()
    duration := endTime.Sub(startTime).Seconds()

    _ = o.deps.Status.Set(ctx, jobID, Status{
        Status:   "success",
        Progress: 100,
        Message:  fmt.Sprintf("Text extraction completed in %.1f seconds", duration),
        Start:    &startTime,
        End:      &endTime,
        Metadata: map[string]any{
            "file_local":        pdfPath,
            "source":            "upload",
            "mode":              "text_only",
            "total_pages":       pageCount,
            "chars_extracted":   len(resultText),
            "result_local_path": resultPath,
            "processing_time":   duration,
        },
    })

    log.Info().
        Str("job_id", jobID).
        Int("pages", pageCount).
        Int("chars", len(resultText)).
        Float64("duration", duration).
        Msg("MuPDF text extraction completed successfully")
}

// processMuPDFOnlyFromS3 handles text-only extraction for S3 files using MuPDF without AI
// Downloads from S3, converts if needed, extracts text with MuPDF, and saves result back to S3
func (o *Orchestrator) processMuPDFOnlyFromS3(ctx context.Context, jobID, s3Path, user, password string) {
    startTime := time.Now()

    // Update status to processing
    _ = o.deps.Status.Set(ctx, jobID, Status{
        Status:   "processing",
        Progress: 5,
        Message:  "Downloading file from S3",
        Start:    &startTime,
        Metadata: map[string]any{"file_path": s3Path, "user": user, "source": "api", "mode": "text_only"},
    })

    // Download file from S3
    localPath, err := downloadS3ToTemp(ctx, s3Path, password)
    if err != nil {
        log.Error().Err(err).Str("job_id", jobID).Str("s3_path", s3Path).Msg("Failed to download from S3")
        endTime := time.Now()
        _ = o.deps.Status.Set(ctx, jobID, Status{
            Status:   "failed",
            Progress: 0,
            Message:  "Failed to download from S3",
            Start:    &startTime,
            End:      &endTime,
            Metadata: map[string]any{"error": err.Error()},
        })
        return
    }
    defer os.Remove(localPath) // cleanup temp file

    log.Info().Str("job_id", jobID).Str("local_path", localPath).Msg("File downloaded from S3")

    // Update progress
    _ = o.deps.Status.Set(ctx, jobID, Status{
        Status:   "processing",
        Progress: 15,
        Message:  "Detecting file type",
        Start:    &startTime,
        Metadata: map[string]any{"file_path": s3Path, "file_local": localPath, "user": user, "source": "api", "mode": "text_only", "password": password},
    })

    // Detect file type
    fileInfo, err := o.deps.FileType.Detect(localPath)
    if err != nil {
        log.Error().Err(err).Str("job_id", jobID).Msg("Failed to detect file type")
        endTime := time.Now()
        _ = o.deps.Status.Set(ctx, jobID, Status{
            Status:   "failed",
            Progress: 0,
            Message:  "File type detection failed",
            Start:    &startTime,
            End:      &endTime,
            Metadata: map[string]any{"error": err.Error()},
        })
        return
    }

    if !fileInfo.Supported {
        log.Warn().Str("mime", fileInfo.MIMEType).Str("job_id", jobID).Msg("Unsupported file type")
        endTime := time.Now()
        _ = o.deps.Status.Set(ctx, jobID, Status{
            Status:   "failed",
            Progress: 0,
            Message:  fmt.Sprintf("Unsupported file type: %s", fileInfo.Description),
            Start:    &startTime,
            End:      &endTime,
            Metadata: map[string]any{"error": "unsupported file type"},
        })
        return
    }

    log.Info().Str("job_id", jobID).Str("mime", fileInfo.MIMEType).Str("desc", fileInfo.Description).Msg("Detected file type")

    // Convert to PDF if needed (Office formats)
    pdfPath := localPath
    if fileInfo.MIMEType != "application/pdf" && !fileInfo.IsText {
        log.Info().Str("job_id", jobID).Str("file", localPath).Msg("Converting to PDF with LibreOffice")
        _ = o.deps.Status.Set(ctx, jobID, Status{
            Status:   "processing",
            Progress: 20,
            Message:  "Converting to PDF",
            Start:    &startTime,
            Metadata: map[string]any{"file_path": s3Path, "file_local": localPath, "user": user, "source": "api", "mode": "text_only", "original_mime": fileInfo.MIMEType},
        })

        convertedPath := filepath.Join(filepath.Dir(localPath), fmt.Sprintf("%s_converted.pdf", jobID))
        convJob := converter.Job{
            InputPath:  localPath,
            OutputPath: convertedPath,
            Extension:  fileInfo.Extension,
            Timeout:    180 * time.Second,
        }

        result := o.deps.Converter.ConvertToPDF(convJob)
        if !result.Success {
            log.Error().Str("job_id", jobID).Str("error", result.Error).Msg("Conversion failed")
            endTime := time.Now()
            _ = o.deps.Status.Set(ctx, jobID, Status{
                Status:   "failed",
                Progress: 0,
                Message:  fmt.Sprintf("Conversion failed: %s", result.Error),
                Start:    &startTime,
                End:      &endTime,
                Metadata: map[string]any{"error": result.Error},
            })
            return
        }

        pdfPath = result.OutputPath
        defer os.Remove(pdfPath) // cleanup converted file
        log.Info().Str("job_id", jobID).Str("pdf", pdfPath).Dur("duration", result.Duration).Msg("Conversion successful")
    }

    // Update progress
    _ = o.deps.Status.Set(ctx, jobID, Status{
        Status:   "processing",
        Progress: 30,
        Message:  "Starting text extraction",
        Start:    &startTime,
        Metadata: map[string]any{"file_path": s3Path, "file_local": pdfPath, "user": user, "source": "api", "mode": "text_only"},
    })

    // Try go-fitz first, fallback to mutool if needed
    gofitzExtractor := mupdf.NewGoFitzExtractor()
    mutoolExtractor := mupdf.NewExtractor()

    useGoFitz := gofitzExtractor.IsAvailable()
    if !useGoFitz {
        log.Warn().Msg("go-fitz not available, falling back to mutool")
        if !mutoolExtractor.IsAvailable() {
            log.Error().Str("job_id", jobID).Msg("Neither go-fitz nor mutool available")
            endTime := time.Now()
            _ = o.deps.Status.Set(ctx, jobID, Status{
                Status:   "failed",
                Progress: 0,
                Message:  "MuPDF tools not available",
                Start:    &startTime,
                End:      &endTime,
                Metadata: map[string]any{"error": "MuPDF tools not installed"},
            })
            return
        }
    }

    // Get page count
    var pageCount int
    if useGoFitz {
        pageCount, err = gofitzExtractor.GetPageCount(pdfPath)
    } else {
        pageCount, err = mutoolExtractor.GetPageCount(pdfPath)
    }
    if err != nil {
        log.Error().Err(err).Str("job_id", jobID).Msg("Failed to get page count")
        endTime := time.Now()
        _ = o.deps.Status.Set(ctx, jobID, Status{
            Status:   "failed",
            Progress: 0,
            Message:  "Failed to read PDF",
            Start:    &startTime,
            End:      &endTime,
            Metadata: map[string]any{"error": err.Error()},
        })
        return
    }

    log.Info().Str("job_id", jobID).Int("pages", pageCount).Msg("Starting MuPDF text extraction from S3 file")

    // Update progress
    _ = o.deps.Status.Set(ctx, jobID, Status{
        Status:   "processing",
        Progress: 35,
        Message:  fmt.Sprintf("Extracting text from %d pages", pageCount),
        Start:    &startTime,
        Metadata: map[string]any{
            "file_path":   s3Path,
            "file_local":  pdfPath,
            "user":        user,
            "source":      "api",
            "mode":        "text_only",
            "total_pages": pageCount,
        },
    })

    // Extract text from all pages
    var allText strings.Builder
    extractedChars := 0

    for i := 1; i <= pageCount; i++ {
        var pageText string
        var err error
        if useGoFitz {
            pageText, err = gofitzExtractor.ExtractTextByPage(pdfPath, i)
        } else {
            pageText, err = mutoolExtractor.ExtractTextByPage(pdfPath, i)
        }
        if err != nil {
            log.Warn().Err(err).Int("page", i).Msg("Failed to extract text from page")
            pageText = fmt.Sprintf("[Page %d extraction failed]\n", i)
        }

        // Add page separator
        if i > 1 {
            allText.WriteString("\n\n")
        }
        allText.WriteString(fmt.Sprintf("=== Page %d ===\n", i))
        allText.WriteString(pageText)
        extractedChars += len(pageText)

        // Update progress (35% to 85% for extraction)
        progress := 35 + (50 * i / pageCount)
        _ = o.deps.Status.Set(ctx, jobID, Status{
            Status:   "processing",
            Progress: progress,
            Message:  fmt.Sprintf("Processed page %d of %d", i, pageCount),
            Start:    &startTime,
            Metadata: map[string]any{
                "file_path":        s3Path,
                "file_local":       pdfPath,
                "user":             user,
                "source":           "api",
                "mode":             "text_only",
                "total_pages":      pageCount,
                "pages_processed":  i,
                "chars_extracted": extractedChars,
            },
        })

        // Check for cancellation
        select {
        case <-ctx.Done():
            log.Info().Str("job_id", jobID).Msg("Job cancelled during processing")
            endTime := time.Now()
            _ = o.deps.Status.Set(ctx, jobID, Status{
                Status:   "cancelled",
                Progress: 0,
                Message:  "Job cancelled",
                Start:    &startTime,
                End:      &endTime,
            })
            return
        default:
        }
    }

    resultText := allText.String()

    // Update progress before saving to S3
    _ = o.deps.Status.Set(ctx, jobID, Status{
        Status:   "processing",
        Progress: 90,
        Message:  "Saving result to S3",
        Start:    &startTime,
        Metadata: map[string]any{
            "file_path":       s3Path,
            "user":            user,
            "source":          "api",
            "mode":            "text_only",
            "total_pages":     pageCount,
            "chars_extracted": len(resultText),
        },
    })

    // Save result to S3 (encrypted)
    s3url, err := SaveAggregatedTextToS3(ctx, s3Path, jobID, resultText, password)
    if err != nil {
        log.Error().Err(err).Str("job_id", jobID).Msg("Failed to save result to S3")
        endTime := time.Now()
        _ = o.deps.Status.Set(ctx, jobID, Status{
            Status:   "failed",
            Progress: 95,
            Message:  "Failed to save result to S3",
            Start:    &startTime,
            End:      &endTime,
            Metadata: map[string]any{"error": err.Error()},
        })
        return
    }

    // Mark as successful
    endTime := time.Now()
    duration := endTime.Sub(startTime).Seconds()

    _ = o.deps.Status.Set(ctx, jobID, Status{
        Status:   "success",
        Progress: 100,
        Message:  fmt.Sprintf("Text extraction completed in %.1f seconds", duration),
        Start:    &startTime,
        End:      &endTime,
        Metadata: map[string]any{
            "file_path":       s3Path,
            "user":            user,
            "source":          "api",
            "mode":            "text_only",
            "total_pages":     pageCount,
            "chars_extracted": len(resultText),
            "result_s3_url":   s3url,
            "processing_time": duration,
        },
    })

    log.Info().
        Str("job_id", jobID).
        Str("s3_url", s3url).
        Int("pages", pageCount).
        Int("chars", len(resultText)).
        Float64("duration", duration).
        Msg("MuPDF text extraction from S3 completed successfully")
}

// (status store now backed by Redis via StatusStore interface)
