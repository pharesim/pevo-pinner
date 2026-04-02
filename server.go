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

// StatusResponse is the JSON shape returned by /api/status.
type StatusResponse struct {
	TotalDiscovered int    `json:"total_discovered"`
	PinnedCount     int    `json:"pinned_count"`
	NextRefresh     string `json:"next_refresh"`
	Mode            string `json:"mode"`
}

// Server handles the management UI and API.
type Server struct {
	discovery *Discovery
	backend   IPFSBackend
	startTime time.Time
	refresh   time.Duration
	mux       *http.ServeMux
}

// NewServer creates the HTTP server with all routes.
func NewServer(discovery *Discovery, backend IPFSBackend, refreshInterval time.Duration) *Server {
	s := &Server{
		discovery: discovery,
		backend:   backend,
		startTime: time.Now(),
		refresh:   refreshInterval,
		mux:       http.NewServeMux(),
	}

	// API routes
	s.mux.HandleFunc("GET /api/papers", s.handlePapers)
	s.mux.HandleFunc("POST /api/pin/{cid}", s.handlePin)
	s.mux.HandleFunc("POST /api/unpin/{cid}", s.handleUnpin)
	s.mux.HandleFunc("POST /api/pin-all", s.handlePinAll)
	s.mux.HandleFunc("GET /api/status", s.handleStatus)

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
	items := s.discovery.Items()
	ctx := r.Context()

	pinned := 0
	failed := 0
	for _, item := range items {
		if err := s.backend.Pin(ctx, item.CID); err != nil {
			log.Printf("[api] pin-all: failed to pin %s: %v", item.CID, err)
			failed++
		} else {
			pinned++
		}
	}

	writeJSON(w, map[string]int{"pinned": pinned, "failed": failed})
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
		TotalDiscovered: len(items),
		PinnedCount:     pinnedCount,
		NextRefresh:     formatDuration(remaining),
	})
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
