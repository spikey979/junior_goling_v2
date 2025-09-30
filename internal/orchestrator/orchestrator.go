package orchestrator

import (
    "context"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "os"
    "strings"
    "time"

    "github.com/google/uuid"
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
}

type Dependencies struct {
    Queue Queue
    Status StatusStore
    Pages PageStore
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

    // Ako je fast_upload, forsiraj MuPDF i preskoči AI
    if req.FastUpload {
        pages, err := DetermineTotalPages(r.Context(), filePath)
        if err != nil { pages = 1 }
        _ = o.deps.Status.Set(r.Context(), jobID, Status{Status: "processing", Progress: 10,
            Message: "fast_upload: MuPDF only", Metadata: map[string]any{"total_pages": pages, "ai_pages": 0, "mupdf_pages": pages, "pages_done": pages}})
        // Simuliraj trenutno dovršavanje za PoC (TODO: dodati stvarni MuPDF extraction i spremanje)
        end := time.Now()
        _ = o.deps.Status.Set(r.Context(), jobID, Status{Status: "success", Progress: 100, Message: "completed (MuPDF only)", End: &end})
        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(http.StatusCreated)
        _ = json.NewEncoder(w).Encode(processResp{Status: "ok", JobID: jobID, Message: "Fast upload: MuPDF only"})
        return
    }

    // Odredi broj stranica (pdfcpu) i napravi selekciju
    pages, err := DetermineTotalPages(r.Context(), filePath)
    if err != nil {
        log.Warn().Err(err).Str("file", filePath).Msg("page count failed; defaulting to 4")
        pages = 4
    }
    sel := SelectPages(SelectionOptions{TextOnly: req.TextOnly, TotalPages: pages})
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

// handleProcessUpload accepts multipart/form-data uploads from dashboard and enqueues work.
// It mirrors the /process_file_junior_call flow but skips S3 usage entirely.
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

    // Page count and selection
    fileRef := "file://" + localPath
    pages, err := DetermineTotalPages(r.Context(), fileRef)
    if err != nil { pages = 1 }
    sel := SelectPages(SelectionOptions{TextOnly: textOnly, TotalPages: pages})

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
    id := strings.TrimPrefix(r.URL.Path, "/progress_spec/")
    st, ok, err := o.deps.Status.Get(r.Context(), id)
    if err != nil { http.Error(w, "error", 500); return }
    if !ok {
        http.Error(w, "not found", http.StatusNotFound); return
    }
    w.Header().Set("Content-Type", "application/json")
    _ = json.NewEncoder(w).Encode(map[string]any{
        "success":  st.Status == "success",
        "job_id":   id,
        "status":   st.Status,
        "progress": st.Progress,
        "message":  st.Message,
        "start_time": st.Start,
        "end_time": st.End,
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
    if body.Text != "" {
        p := 0; fmt.Sscan(pageIDStr, &p)
        _ = o.deps.Pages.SavePageText(r.Context(), jobID, p, body.Text, "ai", body.Provider, body.Model)
    }
    st, ok, err := o.deps.Status.Get(r.Context(), jobID)
    if err != nil || !ok { w.WriteHeader(http.StatusNoContent); return }
    // update metadata counts
    done := intFromMeta(st.Metadata, "pages_done") + 1
    failed := intFromMeta(st.Metadata, "pages_failed")
    total := intFromMeta(st.Metadata, "total_pages")
    st.Metadata["pages_done"] = done
    // progress
    if total > 0 { st.Progress = int(float64(done+failed) / float64(total) * 100) }
    st.Message = fmt.Sprintf("page %s done", pageIDStr)
    // If all pages accounted, aggregate and mark success
    if total > 0 && done+failed >= total {
        agg, _ := o.deps.Pages.AggregateText(r.Context(), jobID, total)
        if st.Metadata == nil { st.Metadata = map[string]any{} }
        st.Metadata["result_text_len"] = len(agg)
        // Save result depending on source
        if src, _ := st.Metadata["source"].(string); src == "upload" {
            if localPath, err := SaveAggregatedTextToLocal(r.Context(), jobID, agg); err == nil {
                st.Metadata["result_local_path"] = localPath
            }
        } else {
            // Save to S3
            filePath, _ := st.Metadata["file_path"].(string)
            if s3url, err := SaveAggregatedTextToS3(r.Context(), filePath, jobID, agg); err == nil {
                st.Metadata["result_s3_url"] = s3url
            }
        }
        st.Status = "success"
        st.Progress = 100
        // Cleanup stale temp files older than 1h as part of job completion hygiene
        CleanupTemps(1 * time.Hour)
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
    done := intFromMeta(st.Metadata, "pages_done")
    failed := intFromMeta(st.Metadata, "pages_failed") + 1
    total := intFromMeta(st.Metadata, "total_pages")
    if st.Metadata == nil { st.Metadata = map[string]any{} }
    st.Metadata["pages_failed"] = failed
    // Extract MuPDF text for this page and save
    p := 0; fmt.Sscan(pageIDStr, &p)
    filePath, _ := st.Metadata["file_path"].(string)
    if filePath == "" { filePath = jobID }
    if txt, err := ExtractPageText(r.Context(), filePath, p); err == nil {
        _ = o.deps.Pages.SavePageText(r.Context(), jobID, p, txt, "mupdf", "", "")
    }
    if total > 0 { st.Progress = int(float64(done+failed) / float64(total) * 100) }
    st.Message = fmt.Sprintf("page %s failed (fallback to MuPDF)", pageIDStr)
    // If all pages accounted, aggregate and mark success
    if total > 0 && done+failed >= total {
        agg, _ := o.deps.Pages.AggregateText(r.Context(), jobID, total)
        st.Metadata["result_text_len"] = len(agg)
        // Save result depending on source
        if src, _ := st.Metadata["source"].(string); src == "upload" {
            if localPath, err := SaveAggregatedTextToLocal(r.Context(), jobID, agg); err == nil {
                st.Metadata["result_local_path"] = localPath
            }
        } else {
            filePath, _ := st.Metadata["file_path"].(string)
            if s3url, err := SaveAggregatedTextToS3(r.Context(), filePath, jobID, agg); err == nil {
                st.Metadata["result_s3_url"] = s3url
            }
        }
        st.Status = "success"
        st.Progress = 100
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

// (status store now backed by Redis via StatusStore interface)
