// Read-only page for driver stations to browse and download recordings. It runs on its own port, apart from the admin
// UI, so the field switch can let team networks reach it without exposing FMS control, WPA keys or passwords. The markup
// and styles are in recordings.html, embedded in the binary.

package web

import (
	"cmp"
	_ "embed"
	"fmt"
	"github.com/Team254/cheesy-arena/field"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"slices"
	"strings"
	"time"
)

//go:embed recordings.html
var recordingsPageHtml string

var recordingsPage = template.Must(template.New("recordings").Parse(recordingsPageHtml))

// What recordings.html renders for one folder: the session folders in it and the files in it. In practice the
// recordings folder holds only sessions and a session holds only team files.
type recordingsListing struct {
	Session  *recordingSession // The open session, or nil for the recordings folder itself.
	Sessions []recordingSession
	Files    []recordingFile
}

type recordingSession struct {
	Name  string // Folder name and link, e.g. 2026-10-10_143205.
	Label string // When it started, e.g. "Today, 2:32 PM", or Name if it isn't a recorder's timestamp.
}

type recordingFile struct {
	Name, Stem, Ext string // 254.wpilog, 254, .wpilog
	Size            string // 3.4 MB
}

// Serves recordingsDir as styled folder listings, one folder per session. Files download rather than open in the
// browser. Only GET and HEAD are allowed; nothing else in the app is reachable through this handler.
func NewRecordingsHandler(recordingsDir string) http.Handler {
	recordings := os.DirFS(recordingsDir)
	files := http.FileServerFS(recordings)
	mux := http.NewServeMux()
	mux.HandleFunc(
		"GET /", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
			if name == "" {
				name = "."
			}
			info, err := fs.Stat(recordings, name)
			// The file server still handles a folder without its trailing slash, by redirecting to add it, so relative
			// links on the page resolve inside the folder.
			if err == nil && info.IsDir() && strings.HasSuffix(r.URL.Path, "/") {
				writeRecordingsListing(w, recordings, name)
				return
			}
			if err == nil && info.Mode().IsRegular() {
				w.Header().Set("Content-Disposition", "attachment")
			}
			files.ServeHTTP(w, r)
		},
	)
	return mux
}

// Renders the folder at name: newest session first, and files in team number order.
func writeRecordingsListing(w http.ResponseWriter, recordings fs.FS, name string) {
	entries, err := fs.ReadDir(recordings, name)
	if err != nil {
		http.Error(w, "Couldn't read the recordings folder", http.StatusInternalServerError)
		return
	}
	now := time.Now()
	var listing recordingsListing
	if name != "." {
		listing.Session = &recordingSession{Name: path.Base(name), Label: sessionLabel(path.Base(name), now)}
	}
	for _, entry := range entries {
		if entry.IsDir() {
			listing.Sessions = append(
				listing.Sessions, recordingSession{Name: entry.Name(), Label: sessionLabel(entry.Name(), now)},
			)
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		ext := path.Ext(entry.Name())
		listing.Files = append(
			listing.Files, recordingFile{
				Name: entry.Name(), Stem: strings.TrimSuffix(entry.Name(), ext), Ext: ext, Size: formatSize(info.Size()),
			},
		)
	}
	// ReadDir sorts by name, which for timestamped folders is oldest first.
	slices.Reverse(listing.Sessions)
	// Stems are team numbers, so shorter first puts 254 ahead of 1114.
	slices.SortFunc(
		listing.Files, func(a, b recordingFile) int {
			return cmp.Or(cmp.Compare(len(a.Stem), len(b.Stem)), strings.Compare(a.Name, b.Name))
		},
	)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := recordingsPage.Execute(w, listing); err != nil {
		log.Printf("Recordings page: %v", err)
	}
}

// Labels a session folder by when it started ("Today, 2:32 PM", "Yesterday, 4:30 PM" or "Oct 8, 4:30 PM"), read from
// its name. A folder the recorder didn't name keeps its own name.
func sessionLabel(name string, now time.Time) string {
	layout := field.RecordingFolderLayout
	if len(name) < len(layout) {
		return name
	}
	startedAt, err := time.ParseInLocation(layout, name[:len(layout)], time.Local)
	if err != nil {
		return name
	}
	clock := startedAt.Format("3:04 PM")
	switch startedAt.Format(time.DateOnly) {
	case now.Format(time.DateOnly):
		return "Today, " + clock
	case now.AddDate(0, 0, -1).Format(time.DateOnly):
		return "Yesterday, " + clock
	}
	return startedAt.Format("Jan 2, ") + clock
}

// Formats a file size in binary units with one decimal, e.g. "3.4 MB".
func formatSize(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	size, unit := float64(bytes)/1024, "KB"
	for _, next := range []string{"MB", "GB"} {
		if size < 1024 {
			break
		}
		size, unit = size/1024, next
	}
	return fmt.Sprintf("%.1f %s", size, unit)
}
