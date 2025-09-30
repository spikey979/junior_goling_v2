package web

import (
    "bytes"
    "encoding/json"
    "fmt"
    "html/template"
    "mime/multipart"
    "io"
    "net/http"
    "os"
    "path/filepath"
    "strings"
)

type Web struct {
    tpl       *template.Template
    username  string
    password  string
    port      string
}

func New() *Web {
    // load templates
    tpl := template.Must(template.ParseGlob(filepath.Join("web", "templates", "*.html")))
    return &Web{
        tpl:      tpl,
        username: os.Getenv("WEB_USERNAME"),
        password: os.Getenv("WEB_PASSWORD"),
        port:     getenv("PORT", "8080"),
    }
}

func (w *Web) RegisterRoutes(mux *http.ServeMux) {
    mux.HandleFunc("/web/login", w.handleLogin)
    mux.HandleFunc("/web/logout", w.handleLogout)
    mux.HandleFunc("/web/", w.requireAuth(w.handleDashboard))
    mux.HandleFunc("/web/dashboard", w.requireAuth(w.handleDashboard))
    mux.HandleFunc("/web/process", w.requireAuth(w.handleProcess))
    mux.HandleFunc("/web/upload", w.requireAuth(w.handleUpload))
    mux.HandleFunc("/web/progress/", w.requireAuth(w.handleProgress))
}

func (w *Web) render(wr http.ResponseWriter, name string, data any) {
    _ = w.tpl.ExecuteTemplate(wr, name, data)
}

func (w *Web) requireAuth(next http.HandlerFunc) http.HandlerFunc {
    return func(wr http.ResponseWriter, r *http.Request) {
        if w.username == "" || w.password == "" {
            http.Error(wr, "WEB_USERNAME/WEB_PASSWORD not set", http.StatusForbidden)
            return
        }
        c, err := r.Cookie("auth")
        if err != nil || c.Value != "1" {
            http.Redirect(wr, r, "/web/login", http.StatusSeeOther)
            return
        }
        next(wr, r)
    }
}

func (w *Web) handleLogin(wr http.ResponseWriter, r *http.Request) {
    switch r.Method {
    case http.MethodGet:
        w.render(wr, "login.html", map[string]any{"Error": r.URL.Query().Get("error")})
    case http.MethodPost:
        if err := r.ParseForm(); err != nil { http.Redirect(wr, r, "/web/login?error=invalid+form", http.StatusSeeOther); return }
        if r.Form.Get("username") == w.username && r.Form.Get("password") == w.password {
            http.SetCookie(wr, &http.Cookie{Name: "auth", Value: "1", Path: "/", HttpOnly: true})
            http.Redirect(wr, r, "/web/dashboard", http.StatusSeeOther)
            return
        }
        http.Redirect(wr, r, "/web/login?error=invalid+credentials", http.StatusSeeOther)
    default:
        wr.WriteHeader(http.StatusMethodNotAllowed)
    }
}

func (w *Web) handleLogout(wr http.ResponseWriter, r *http.Request) {
    http.SetCookie(wr, &http.Cookie{Name: "auth", Value: "", Path: "/", MaxAge: -1})
    http.Redirect(wr, r, "/web/login", http.StatusSeeOther)
}

func (w *Web) handleDashboard(wr http.ResponseWriter, r *http.Request) {
    w.render(wr, "dashboard.html", map[string]any{
        "Username": w.username,
    })
}

func (w *Web) handleProcess(wr http.ResponseWriter, r *http.Request) {
    if err := r.ParseForm(); err != nil { http.Error(wr, "invalid form", 400); return }
    filePath := r.Form.Get("file_path")
    userName := r.Form.Get("user_name")
    aiEngine := r.Form.Get("ai_engine")
    textOnly := r.Form.Get("text_only") == "on"
    body := map[string]any{"file_path": filePath, "user_name": userName, "ai_engine": aiEngine, "text_only": textOnly, "source": "dashboard"}
    b, _ := json.Marshal(body)
    url := fmt.Sprintf("http://127.0.0.1:%s/process_file_junior_call", w.port)
    resp, err := http.Post(url, "application/json", bytes.NewReader(b))
    if err != nil { http.Error(wr, "request failed", 500); return }
    defer resp.Body.Close()
    out, _ := io.ReadAll(resp.Body)
    wr.Header().Set("Content-Type", "application/json")
    wr.Write(out)
}

// handleUpload proxies multipart upload from the dashboard to the API endpoint /process_file_upload
func (w *Web) handleUpload(wr http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost { wr.WriteHeader(http.StatusMethodNotAllowed); return }
    // Ensure we can read multipart
    if err := r.ParseMultipartForm(64 << 20); err != nil { http.Error(wr, "invalid multipart form", 400); return }

    var b bytes.Buffer
    mw := multipart.NewWriter(&b)

    // Copy file part
    file, hdr, err := r.FormFile("file")
    if err != nil { http.Error(wr, "missing file", 400); return }
    defer file.Close()
    fw, err := mw.CreateFormFile("file", hdr.Filename)
    if err != nil { http.Error(wr, "upload error", 500); return }
    if _, err := io.Copy(fw, file); err != nil { http.Error(wr, "upload error", 500); return }

    // Copy other fields
    for _, k := range []string{"user_name", "ai_engine", "text_only"} {
        if v := r.FormValue(k); v != "" {
            _ = mw.WriteField(k, v)
        }
    }
    _ = mw.Close()

    url := fmt.Sprintf("http://127.0.0.1:%s/process_file_upload", w.port)
    req, _ := http.NewRequest(http.MethodPost, url, &b)
    req.Header.Set("Content-Type", mw.FormDataContentType())
    resp, err := http.DefaultClient.Do(req)
    if err != nil { http.Error(wr, "request failed", 500); return }
    defer resp.Body.Close()
    wr.Header().Set("Content-Type", "application/json")
    wr.WriteHeader(resp.StatusCode)
    io.Copy(wr, resp.Body)
}

func (w *Web) handleProgress(wr http.ResponseWriter, r *http.Request) {
    jobID := strings.TrimPrefix(r.URL.Path, "/web/progress/")
    url := fmt.Sprintf("http://127.0.0.1:%s/progress_spec/%s", w.port, jobID)
    resp, err := http.Get(url)
    if err != nil { http.Error(wr, "progress failed", 500); return }
    defer resp.Body.Close()
    wr.Header().Set("Content-Type", "application/json")
    io.Copy(wr, resp.Body)
}

func getenv(k, d string) string { if v := os.Getenv(k); v != "" { return v }; return d }
