// End-to-end test of the recordings that driver stations download from the team page's Downloads tab, over a real
// listener on the team page's handler. Writes a transcript of every request and response to
// e2e-output/recordings.txt.
//
// Ways this can fail, each checked below:
//  1. A session folder or a file in one isn't listed.
//  2. Sessions aren't newest first, or files aren't in team number order (alphabetical puts 1114 before 254).
//  3. A session is labeled with the wrong day or time, a file's size or name is wrong, or its team isn't read from its
//     name (the page marks the team's own file with it).
//  4. A file comes back with different bytes than were recorded.
//  5. A file opens in the browser (a CSV shows as text) instead of downloading.
//  6. A folder serves Go's unstyled default listing instead of nothing.
//  7. A write method is accepted.
//  8. A path with .. (plain or percent-encoded) escapes the recordings folder and serves event.db beside it.
//  9. A file isn't given the station its team was on when it was recorded, from the session's stations.json (so the page
//     can't color the team's own file by that alliance), or stations.json is listed as a download.

package web

import (
	"bufio"
	"encoding/json"
	"fmt"
	"github.com/Team254/cheesy-arena/field"
	"github.com/Team254/cheesy-arena/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecordingsEndToEnd(t *testing.T) {
	var transcript strings.Builder
	note := func(format string, args ...any) { fmt.Fprintf(&transcript, format+"\n", args...) }
	t.Cleanup(
		func() {
			outputDir := filepath.Join("..", "e2e-output")
			_ = os.MkdirAll(outputDir, 0755)
			_ = os.WriteFile(filepath.Join(outputDir, "recordings.txt"), []byte(transcript.String()), 0644)
		},
	)

	// Lay out the folder next to the database the way main and the recorder do, with sessions named relative to today
	// so their labels are known: one eight days ago, one yesterday, and two today that started in the same second.
	baseDir := t.TempDir()
	secret := "event.db contents: WpaKey=secret254"
	require.Nil(t, os.WriteFile(filepath.Join(baseDir, "event.db"), []byte(secret), 0644))
	recordingsDir := filepath.Join(baseDir, "recordings")
	now := time.Now()
	startedAt := func(daysAgo, hour, minute int) time.Time {
		return time.Date(now.Year(), now.Month(), now.Day()-daysAgo, hour, minute, 5, 0, time.Local)
	}
	older := startedAt(8, 9, 0)
	olderSession := older.Format(field.RecordingFolderLayout)
	yesterdaySession := startedAt(1, 16, 30).Format(field.RecordingFolderLayout)
	session := startedAt(0, 14, 32).Format(field.RecordingFolderLayout)
	emptySession := session + "-2"
	for _, name := range []string{olderSession, yesterdaySession, session, emptySession} {
		require.Nil(t, os.MkdirAll(filepath.Join(recordingsDir, name), 0755))
	}
	sessionDir := filepath.Join(recordingsDir, session)
	wpilog := "WPILOG\x00\x01\x00\x00binary bytes"
	csv := "Timestamp,Key,Value\n0.02,NT:/Robot/Pose/translation/x,1.5\n"
	require.Nil(t, os.WriteFile(filepath.Join(sessionDir, "254.wpilog"), []byte(wpilog), 0644))
	require.Nil(t, os.WriteFile(filepath.Join(sessionDir, "1114.csv"), []byte(csv), 0644))
	// A match-sized file, sparse so it costs no disk: 3.4 MB.
	require.Nil(t, os.WriteFile(filepath.Join(sessionDir, "971.wpilog"), nil, 0644))
	require.Nil(t, os.Truncate(filepath.Join(sessionDir, "971.wpilog"), 3_565_568))
	// The recorder noted 254 and 971's stations; 1114's file is from before it did.
	stations := `{"254":"B1","971":"R3"}`
	require.Nil(t, os.WriteFile(filepath.Join(sessionDir, field.RecordingStationsFile), []byte(stations), 0644))

	database, err := model.OpenDatabase(filepath.Join(t.TempDir(), "event.db"))
	require.Nil(t, err)
	defer database.Close()
	fieldController, err := field.New(
		database, field.Options{DriverStationPorts: field.DefaultDriverStationPorts, RecordingsDir: recordingsDir},
	)
	require.Nil(t, err)
	server := httptest.NewServer(NewTeamHandler(fieldController, recordingsDir))
	defer server.Close()

	request := func(method, path string) (*http.Response, string) {
		req, err := http.NewRequest(method, server.URL+path, nil)
		require.Nil(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.Nil(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.Nil(t, err)
		note(
			"== %s %s -> %d (Content-Type: %q, Content-Disposition: %q)\n%s\n", method, path, resp.StatusCode,
			resp.Header.Get("Content-Type"), resp.Header.Get("Content-Disposition"), body,
		)
		return resp, string(body)
	}

	// 1, 2, 3 and 9. Every session newest first with its label, and each one's files in team number order with the
	// stations their teams were on.
	resp, body := request("GET", "/api/recordings")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var listing recordingsListing
	require.Nil(t, json.Unmarshal([]byte(body), &listing))
	assert.Equal(
		t, recordingsListing{
			Sessions: []recordingSession{
				{Name: emptySession, Label: "Today, 2:32 PM", Files: []recordingFile{}},
				{
					Name: session, Label: "Today, 2:32 PM", Files: []recordingFile{
						{Name: "254.wpilog", TeamId: 254, Station: "B1", Size: fmt.Sprintf("%d B", len(wpilog))},
						{Name: "971.wpilog", TeamId: 971, Station: "R3", Size: "3.4 MB"},
						{Name: "1114.csv", TeamId: 1114, Size: fmt.Sprintf("%d B", len(csv))},
					},
				},
				{Name: yesterdaySession, Label: "Yesterday, 4:30 PM", Files: []recordingFile{}},
				{Name: olderSession, Label: older.Format("Jan 2, 3:04 PM"), Files: []recordingFile{}},
			},
		}, listing,
	)

	// 4 and 5. Downloads, from the path the page links each file to.
	for name, contents := range map[string]string{"254.wpilog": wpilog, "1114.csv": csv} {
		resp, body = request("GET", "/recordings/"+session+"/"+name)
		assert.Equal(t, http.StatusOK, resp.StatusCode, name)
		assert.Equal(t, contents, body, name)
		assert.Equal(t, "attachment", resp.Header.Get("Content-Disposition"), name)
	}

	// 6. Folders.
	for _, path := range []string{"/recordings/", "/recordings/" + session + "/", "/recordings/" + session} {
		resp, body = request("GET", path)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, path)
		assert.NotContains(t, body, "254.wpilog", path)
	}

	// 7. Writes.
	for _, path := range []string{"/api/recordings", "/recordings/" + session + "/254.wpilog"} {
		for _, method := range []string{"PUT", "POST", "DELETE"} {
			resp, _ = request(method, path)
			assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode, "%s %s", method, path)
		}
	}
	resp, body = request("GET", "/recordings/"+session+"/254.wpilog")
	assert.Equal(t, wpilog, body)

	// 8. Traversal, sent over raw TCP so no client cleans the path first.
	for _, path := range []string{
		"/recordings/../event.db", "/recordings/%2e%2e/event.db", "/recordings/" + session + "/../../event.db",
		"/../event.db", "/recordings/..%2fevent.db",
	} {
		conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
		require.Nil(t, err)
		_, err = fmt.Fprintf(conn, "GET %s HTTP/1.0\r\nHost: 10.0.100.5\r\n\r\n", path)
		require.Nil(t, err)
		raw, err := io.ReadAll(bufio.NewReader(conn))
		require.Nil(t, err)
		conn.Close()
		header, _, _ := strings.Cut(string(raw), "\r\n\r\n")
		note("== raw GET %s\n%s\n", path, header)
		assert.NotContains(t, string(raw), secret, path)
	}
}
