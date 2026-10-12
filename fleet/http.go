package fleet

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
)

//go:embed web
var webFS embed.FS

func (s *Server) HTTPHandler() http.Handler {
	web, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(web))
	mux.HandleFunc("GET /api/agents/events", s.agentEvents)
	return mux
}

func (s *Server) agentEvents(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	for {
		agents, changed := s.feed.get()
		if agents != nil {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", agents); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-changed:
		}
	}
}
