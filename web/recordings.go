// The recordings folder as the team page's Downloads tab sees it: a JSON listing of every session and its files, with
// the station each file's team was on then, and the files themselves for download. Part of the team page's server (see
// team.go).

package web

import (
	"cmp"
	"encoding/json"
	"fmt"
	"github.com/Team254/cheesy-arena/field"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Every session in the recordings folder, newest first. In practice the folder holds only sessions and a session holds
// only team files.
type recordingsListing struct {
	Sessions []recordingSession `json:"sessions"`
}

type recordingSession struct {
	Name  string          `json:"name"`  // Folder name, e.g. 2026-10-10_143205. Files are at /recordings/<name>/<file>.
	Label string          `json:"label"` // When it started, e.g. "Today, 2:32 PM", or Name if it isn't a recorder's timestamp.
	Files []recordingFile `json:"files"` // In team number order.
}

type recordingFile struct {
	Name    string `json:"name"`    // 254.wpilog
	TeamId  int    `json:"teamId"`  // 254, from the name; 0 if the name isn't a team number.
	Station string `json:"station"` // The station the team was on when the file started, e.g. B1; "" if not noted.
	Size    string `json:"size"`    // 3.4 MB
}

// Lists every session in recordingsDir with its files.
func listRecordings(recordingsDir string) (recordingsListing, error) {
	recordings := os.DirFS(recordingsDir)
	entries, err := fs.ReadDir(recordings, ".")
	if err != nil {
		return recordingsListing{}, err
	}
	now := time.Now()
	listing := recordingsListing{Sessions: []recordingSession{}}
	// ReadDir sorts by name, which for timestamped folders is oldest first.
	for _, entry := range slices.Backward(entries) {
		if !entry.IsDir() {
			continue
		}
		session := recordingSession{Name: entry.Name(), Label: sessionLabel(entry.Name(), now), Files: []recordingFile{}}
		files, err := fs.ReadDir(recordings, entry.Name())
		if err != nil {
			return recordingsListing{}, err
		}
		stations := sessionStations(recordings, entry.Name())
		for _, file := range files {
			info, err := file.Info()
			if err != nil || !info.Mode().IsRegular() || file.Name() == field.RecordingStationsFile {
				continue
			}
			teamId, _ := strconv.Atoi(strings.TrimSuffix(file.Name(), path.Ext(file.Name())))
			session.Files = append(
				session.Files, recordingFile{
					Name: file.Name(), TeamId: teamId, Station: stations[strconv.Itoa(teamId)], Size: formatSize(info.Size()),
				},
			)
		}
		// By team number, so 254 comes ahead of 1114; files that aren't a team's go last, by name.
		order := func(file recordingFile) int {
			if file.TeamId == 0 {
				return math.MaxInt
			}
			return file.TeamId
		}
		slices.SortFunc(
			session.Files, func(a, b recordingFile) int {
				return cmp.Or(cmp.Compare(order(a), order(b)), strings.Compare(a.Name, b.Name))
			},
		)
		listing.Sessions = append(listing.Sessions, session)
	}
	return listing, nil
}

// The stations a session's teams were on, by team number, from its stations.json (see field.RecordingStationsFile).
// Empty for a session recorded before the recorder noted them, or one whose file can't be read.
func sessionStations(recordings fs.FS, session string) map[string]string {
	stations := map[string]string{}
	if data, err := fs.ReadFile(recordings, path.Join(session, field.RecordingStationsFile)); err == nil {
		_ = json.Unmarshal(data, &stations)
	}
	return stations
}

// Serves the files in recordingsDir as downloads rather than opening them in the browser. Folders aren't listed; the
// Downloads tab lists them instead.
func newRecordingFiles(recordingsDir string) http.Handler {
	recordings := os.DirFS(recordingsDir)
	files := http.FileServerFS(recordings)
	return http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			info, err := fs.Stat(recordings, strings.TrimPrefix(path.Clean(r.URL.Path), "/"))
			if err != nil || !info.Mode().IsRegular() {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Disposition", "attachment")
			files.ServeHTTP(w, r)
		},
	)
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
