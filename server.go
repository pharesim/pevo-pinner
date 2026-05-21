package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"time"
)

//go:embed static
var staticFiles embed.FS

// PaperResponse is the JSON shape returned by /api/papers.
type PaperResponse struct {
	Author     string    `json:"author"`
	Permlink   string    `json:"permlink"`
	Title      string    `json:"title"`
	CID        string    `json:"cid"`
	Filename   string    `json:"filename"`
	CIDType    string    `json:"cid_type"`
	Discipline string    `json:"discipline"`
	Created    time.Time `json:"created"`
	Pinned     bool      `json:"pinned"`
}

// StatusResponse is the JSON shape returned by /api/status. The configuration
// fields surface the operator-tunable knobs so an operator (or agent) who did
// not start the process can read back the enforced limits without parsing
// startup logs.
type StatusResponse struct {
	TotalDiscovered    int    `json:"total_discovered"`
	PinnedCount        int    `json:"pinned_count"`
	NextRefresh        string `json:"next_refresh"`
	Mode               string `json:"mode"`
	RefreshInterval    string `json:"refresh_interval"`
	MaxPinBytes        int64  `json:"max_pin_bytes"`
	AutoPinConcurrency int    `json:"autopin_concurrency"`
	AutoPinAuthorCap   int    `json:"autopin_author_cap"`
}

// Server handles the management UI and API.
type Server struct {
	discovery          *Discovery
	backend            IPFSBackend
	autopin            *AutoPinManager
	autopinRunner      *AutoPinRunner
	startTime          time.Time
	refresh            time.Duration
	maxPinBytes        int64
	autoPinConcurrency int
	autoPinAuthorCap   int
	mode               string
	mux                *http.ServeMux
}

// NewServer creates the HTTP server with all routes.
func NewServer(discovery *Discovery, backend IPFSBackend, autopin *AutoPinManager, runner *AutoPinRunner, refreshInterval time.Duration, maxPinBytes int64, autoPinConcurrency, autoPinAuthorCap int) *Server {
	s := &Server{
		discovery:          discovery,
		backend:            backend,
		autopin:            autopin,
		autopinRunner:      runner,
		startTime:          time.Now(),
		refresh:            refreshInterval,
		maxPinBytes:        maxPinBytes,
		autoPinConcurrency: autoPinConcurrency,
		autoPinAuthorCap:   autoPinAuthorCap,
		mode:               backendMode(backend),
		mux:                http.NewServeMux(),
	}

	// API routes
	s.mux.HandleFunc("GET /api/papers", s.handlePapers)
	s.mux.HandleFunc("POST /api/pin/{cid}", s.handlePin)
	s.mux.HandleFunc("POST /api/unpin/{cid}", s.handleUnpin)
	s.mux.HandleFunc("POST /api/pin-all", s.handlePinAll)
	s.mux.HandleFunc("GET /api/status", s.handleStatus)

	// Auto-pin rules
	s.mux.HandleFunc("GET /api/autopin/rules", s.handleGetRules)
	s.mux.HandleFunc("POST /api/autopin/rules", s.handleAddRule)
	s.mux.HandleFunc("PUT /api/autopin/rules/{id}", s.handleUpdateRule)
	s.mux.HandleFunc("DELETE /api/autopin/rules/{id}", s.handleDeleteRule)
	s.mux.HandleFunc("POST /api/autopin/enable-all", s.handleEnableAllRules)
	s.mux.HandleFunc("POST /api/autopin/disable-all", s.handleDisableAllRules)
	s.mux.HandleFunc("POST /api/autopin/evaluate", s.handleEvaluateRules)

	// IPFS gateway proxy (for embedded mode)
	s.mux.HandleFunc("GET /ipfs/", s.handleIPFSProxy)

	// Static files (management UI)
	staticSub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatalf("failed to create sub filesystem: %v", err)
	}
	s.mux.Handle("GET /", http.FileServer(http.FS(staticSub)))

	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handlePapers(w http.ResponseWriter, r *http.Request) {
	items := s.discovery.Items()
	ctx := r.Context()

	papers := make([]PaperResponse, 0, len(items))
	for _, item := range items {
		pinned, _ := s.backend.IsPinned(ctx, item.CID)
		papers = append(papers, PaperResponse{
			Author:     item.Author,
			Permlink:   item.Permlink,
			Title:      item.Title,
			CID:        item.CID,
			Filename:   item.Filename,
			CIDType:    item.CIDType,
			Discipline: item.Discipline,
			Created:    item.Created,
			Pinned:     pinned,
		})
	}

	writeJSON(w, papers)
}

func (s *Server) handlePin(w http.ResponseWriter, r *http.Request) {
	cid := r.PathValue("cid")
	if cid == "" {
		http.Error(w, "missing CID", http.StatusBadRequest)
		return
	}
	if err := ValidateCID(cid); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := s.backend.Pin(r.Context(), cid); err != nil {
		log.Printf("[api] pin %s failed: %v", cid, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]string{"status": "pinned", "cid": cid})
}

func (s *Server) handleUnpin(w http.ResponseWriter, r *http.Request) {
	cid := r.PathValue("cid")
	if cid == "" {
		http.Error(w, "missing CID", http.StatusBadRequest)
		return
	}
	if err := ValidateCID(cid); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := s.backend.Unpin(r.Context(), cid); err != nil {
		log.Printf("[api] unpin %s failed: %v", cid, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]string{"status": "unpinned", "cid": cid})
}

func (s *Server) handlePinAll(w http.ResponseWriter, r *http.Request) {
	// Route through the autopin runner so the bounded pool, per-author cap,
	// and IsPinned pre-check all apply to operator-triggered pin-all. The
	// runner is shared with the discovery callback and evaluate-rules handler,
	// so the global concurrency bound holds across all three callers.
	items := s.discovery.Items()
	res := s.autopinRunner.Run(r.Context(), items)
	writeJSON(w, map[string]int{
		"matched":          res.Matched,
		"pinned":           res.Pinned,
		"failed":           res.Failed,
		"shed":             res.Shed,
		"is_pinned_errors": res.IsPinnedErrors,
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	items := s.discovery.Items()
	ctx := r.Context()

	pinnedCount := 0
	for _, item := range items {
		if pinned, _ := s.backend.IsPinned(ctx, item.CID); pinned {
			pinnedCount++
		}
	}

	elapsed := time.Since(s.startTime)
	remaining := s.refresh - (elapsed % s.refresh)

	writeJSON(w, StatusResponse{
		TotalDiscovered:    len(items),
		PinnedCount:        pinnedCount,
		NextRefresh:        formatDuration(remaining),
		Mode:               s.mode,
		RefreshInterval:    s.refresh.String(),
		MaxPinBytes:        s.maxPinBytes,
		AutoPinConcurrency: s.autoPinConcurrency,
		AutoPinAuthorCap:   s.autoPinAuthorCap,
	})
}

// backendMode returns the operator-facing tag for the active backend. Drives
// StatusResponse.Mode so an agent or operator who did not start the process
// can tell which backend is wired without parsing startup logs.
func backendMode(b IPFSBackend) string {
	switch b.(type) {
	case *EmbeddedNode:
		return "embedded"
	case *PinataBackend:
		return "pinata"
	default:
		return "unknown"
	}
}

func (s *Server) handleIPFSProxy(w http.ResponseWriter, r *http.Request) {
	// Extract CID from path
	cid := r.URL.Path[len("/ipfs/"):]
	if cid == "" {
		http.Error(w, "missing CID", http.StatusBadRequest)
		return
	}
	if err := ValidateCID(cid); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Check if embedded node
	embedded, ok := s.backend.(*EmbeddedNode)
	if !ok {
		// For pinata mode, redirect to a public gateway
		http.Redirect(w, r, "https://gateway.pinata.cloud/ipfs/"+cid, http.StatusTemporaryRedirect)
		return
	}

	embedded.handleGateway(w, r)
}

func (s *Server) handleGetRules(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.autopin.Rules())
}

func (s *Server) handleAddRule(w http.ResponseWriter, r *http.Request) {
	var rule AutoPinRule
	if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	created, err := s.autopin.AddRule(rule)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, created)
}

func (s *Server) handleUpdateRule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var rule AutoPinRule
	if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	updated, err := s.autopin.UpdateRule(id, rule)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, updated)
}

func (s *Server) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.autopin.DeleteRule(id); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]string{"status": "deleted"})
}

func (s *Server) handleEnableAllRules(w http.ResponseWriter, r *http.Request) {
	if err := s.autopin.SetAllEnabled(true); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, s.autopin.Rules())
}

func (s *Server) handleDisableAllRules(w http.ResponseWriter, r *http.Request) {
	if err := s.autopin.SetAllEnabled(false); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, s.autopin.Rules())
}

func (s *Server) handleEvaluateRules(w http.ResponseWriter, r *http.Request) {
	items := s.discovery.Items()
	matched := s.autopin.MatchingItems(items)
	res := s.autopinRunner.Run(r.Context(), matched)
	writeJSON(w, map[string]int{
		"matched":          res.Matched,
		"pinned":           res.Pinned,
		"failed":           res.Failed,
		"shed":             res.Shed,
		"is_pinned_errors": res.IsPinnedErrors,
	})
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[api] json encode error: %v", err)
	}
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%dm", h, m)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}
