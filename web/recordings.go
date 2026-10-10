// Read-only page for driver stations to browse and download recordings. It runs on its own port, apart from the admin
// UI, so the field switch can let team networks reach it without exposing FMS control, WPA keys or passwords.

package web

import (
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
)

// Serves recordingsDir as browsable folder listings, one folder per session. Files download rather than open in the
// browser. Only GET and HEAD are allowed; nothing else in the app is reachable through this handler.
func NewRecordingsHandler(recordingsDir string) http.Handler {
	recordings := os.DirFS(recordingsDir)
	files := http.FileServerFS(recordings)
	mux := http.NewServeMux()
	mux.HandleFunc(
		"GET /", func(w http.ResponseWriter, r *http.Request) {
			name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
			if info, err := fs.Stat(recordings, name); err == nil && info.Mode().IsRegular() {
				w.Header().Set("Content-Disposition", "attachment")
			}
			w.Header().Set("Cache-Control", "no-store")
			files.ServeHTTP(w, r)
		},
	)
	return mux
}
