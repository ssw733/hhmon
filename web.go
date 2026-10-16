package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

//go:embed webui/index.html
var indexHTML []byte

type webServer struct {
	cfg   Config
	store *Store
	mon   *Monitor
	buf   *lineBuffer
}

func (w *webServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", w.handleIndex)
	mux.HandleFunc("/api/vacancies", w.handleVacancies)
	mux.HandleFunc("/api/vacancy", w.handleVacancy)
	mux.HandleFunc("/api/status", w.handleStatus)
	mux.HandleFunc("/api/log", w.handleLog)
	mux.HandleFunc("/api/scan", w.handleScan)
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func (w *webServer) handleIndex(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = rw.Write(indexHTML)
}

type vacancyListItem struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Company     string `json:"company"`
	Region      string `json:"region"`
	Salary      string `json:"salary"`
	URL         string `json:"url"`
	FoundAt     string `json:"found_at"`
	PublishedAt string `json:"published_at"`
	HasLetter   bool   `json:"has_letter"`
}

func (w *webServer) handleVacancies(rw http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	vs, err := w.store.Recent(limit)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]vacancyListItem, 0, len(vs))
	for _, v := range vs {
		out = append(out, vacancyListItem{
			ID: v.ID, Title: v.Title, Company: v.Company, Region: v.Region, Salary: v.Salary,
			URL: v.URL, FoundAt: v.FoundAt.Format(time.RFC3339),
			PublishedAt: v.PublishedAt.Format(time.RFC3339), HasLetter: v.CoverLetter != "",
		})
	}
	writeJSON(rw, out)
}

func (w *webServer) handleVacancy(rw http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(rw, "id required", http.StatusBadRequest)
		return
	}
	v, err := w.store.Get(id)
	if err != nil {
		http.Error(rw, "not found", http.StatusNotFound)
		return
	}
	writeJSON(rw, v)
}

type searchStatus struct {
	Name  string `json:"name"`
	Query string `json:"query"`
	LLM   string `json:"llm"`
}

type statusResponse struct {
	Searches     []searchStatus `json:"searches"`
	PollInterval int            `json:"poll_interval_seconds"`
	Total        int            `json:"total"`
	WithLetter   int            `json:"with_letter"`
	LastRun      string         `json:"last_run"`
	LastNew      int            `json:"last_new"`
	SeedFirstRun bool           `json:"seed_on_first_run"`
}

func (w *webServer) handleStatus(rw http.ResponseWriter, r *http.Request) {
	total, _ := w.store.Count()
	withLetter, _ := w.store.CountWithLetters()
	searches := make([]searchStatus, 0, len(w.mon.searches))
	for _, s := range w.mon.searches {
		searches = append(searches, searchStatus{Name: s.name, Query: s.search.Query, LLM: describeLLM(s.search, w.cfg)})
	}
	lastRun := ""
	if t := w.mon.LastRun(); !t.IsZero() {
		lastRun = t.Format(time.RFC3339)
	}
	writeJSON(rw, statusResponse{
		Searches: searches, PollInterval: w.cfg.PollIntervalSec,
		Total: total, WithLetter: withLetter,
		LastRun: lastRun, LastNew: w.mon.LastNew(),
		SeedFirstRun: w.cfg.SeedOnFirstRun,
	})
}

func (w *webServer) handleLog(rw http.ResponseWriter, r *http.Request) {
	lines := 200
	if v := r.URL.Query().Get("lines"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			lines = n
		}
	}
	all := w.buf.Snapshot()
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	writeJSON(rw, all)
}

func (w *webServer) handleScan(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "POST only", http.StatusMethodNotAllowed)
		return
	}
	n, err := w.mon.ScanNow(r.Context())
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(rw, map[string]int{"new": n})
}

// exportStatic writes a self-contained snapshot of the UI with data baked in,
// so it can be opened locally without running the server.
func (w *webServer) exportStatic(path string) error {
	vs, err := w.store.Recent(1000)
	if err != nil {
		return err
	}
	list := make([]vacancyListItem, 0, len(vs))
	details := make(map[string]Vacancy, len(vs))
	for _, v := range vs {
		list = append(list, vacancyListItem{
			ID: v.ID, Title: v.Title, Company: v.Company, Region: v.Region, Salary: v.Salary,
			URL: v.URL, FoundAt: v.FoundAt.Format(time.RFC3339),
			PublishedAt: v.PublishedAt.Format(time.RFC3339), HasLetter: v.CoverLetter != "",
		})
		details[v.ID] = v
	}
	total, _ := w.store.Count()
	withLetter, _ := w.store.CountWithLetters()
	searches := make([]searchStatus, 0, len(w.mon.searches))
	for _, s := range w.mon.searches {
		searches = append(searches, searchStatus{Name: s.name, Query: s.search.Query, LLM: describeLLM(s.search, w.cfg)})
	}
	lastRun := ""
	if t := w.mon.LastRun(); !t.IsZero() {
		lastRun = t.Format(time.RFC3339)
	}
	status := statusResponse{
		Searches: searches, PollInterval: w.cfg.PollIntervalSec,
		Total: total, WithLetter: withLetter, LastRun: lastRun, LastNew: w.mon.LastNew(),
		SeedFirstRun: w.cfg.SeedOnFirstRun,
	}
	logLines := w.buf.Snapshot()
	if len(logLines) == 0 {
		if b, err := os.ReadFile(w.cfg.LogPath); err == nil {
			logLines = strings.Split(strings.TrimRight(string(b), "\n"), "\n")
		}
	}
	data := map[string]any{
		"status":    status,
		"vacancies": list,
		"details":   details,
		"log":       logLines,
	}
	blob, err := json.Marshal(data)
	if err != nil {
		return err
	}
	inject := []byte("<script>window.__STATIC_DATA__=" + string(blob) + ";</script>\n<script>")
	out := bytes.Replace(indexHTML, []byte("<script>"), inject, 1)
	return os.WriteFile(path, out, 0o644)
}
