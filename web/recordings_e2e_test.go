// End-to-end test of the read-only recordings page that driver stations reach through the field switch, over a real
// listener. Writes a transcript of every request and response to e2e-output/recordings-page.txt, and the two rendered
// listings to recordings-page-logs.html and recordings-page-session.html to open in a browser.
//
// Ways this can fail, each checked below:
//  1. A session folder or a file in one isn't listed, or the listing is Go's unstyled default instead of the logs page.
//  2. Sessions aren't newest first, or files aren't in team number order (alphabetical puts 1114 before 254).
//  3. A session is labeled with the wrong day or time, or its size or file name is wrong.
//  4. A file comes back with different bytes than were recorded.
//  5. A file opens in the browser (a CSV shows as text) instead of downloading.
//  6. The admin API is reachable on this port, letting a driver station enable robots or read WPA keys and passwords.
//  7. A write method is accepted.
//  8. A path with .. (plain or percent-encoded) escapes the recordings folder and serves event.db beside it.
//  9. "Logs" in a session's title doesn't lead back to the list of sessions.

package web

import (
	"bufio"
	"fmt"
	"github.com/Team254/cheesy-arena/field"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestRecordingsPageEndToEnd(t *testing.T) {
	var transcript strings.Builder
	var logsPage, sessionPage string
	note := func(format string, args ...any) { fmt.Fprintf(&transcript, format+"\n", args...) }
	t.Cleanup(
		func() {
			outputDir := filepath.Join("..", "e2e-output")
			_ = os.MkdirAll(outputDir, 0755)
			_ = os.WriteFile(filepath.Join(outputDir, "recordings-page.txt"), []byte(transcript.String()), 0644)
			_ = os.WriteFile(filepath.Join(outputDir, "recordings-page-logs.html"), []byte(logsPage), 0644)
			_ = os.WriteFile(filepath.Join(outputDir, "recordings-page-session.html"), []byte(sessionPage), 0644)
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

	server := httptest.NewServer(NewRecordingsHandler(recordingsDir))
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

	// Asserts each part is in body, after the one before it.
	assertInOrder := func(body string, parts ...string) {
		rest := body
		for _, part := range parts {
			i := strings.Index(rest, part)
			if !assert.GreaterOrEqual(t, i, 0, "%q missing or out of order", part) {
				return
			}
			rest = rest[i+len(part):]
		}
	}

	// 1, 2 and 3. Listings: every session newest first, each followed by its label.
	resp, body := request("GET", "/")
	logsPage = body
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/html; charset=utf-8", resp.Header.Get("Content-Type"))
	assert.Empty(t, resp.Header.Get("Content-Disposition"))
	assertInOrder(
		body, "<title>Logs</title>", "<style>", "<h1>Logs</h1>",
		`href="`+emptySession+`/"`, "Today, 2:32 PM", emptySession,
		`href="`+session+`/"`, "Today, 2:32 PM", session,
		`href="`+yesterdaySession+`/"`, "Yesterday, 4:30 PM", yesterdaySession,
		`href="`+olderSession+`/"`, older.Format("Jan 2, 3:04 PM"), olderSession,
	)

	// A session's files in team number order, each with its size, linking back to the list of sessions.
	resp, body = request("GET", "/"+session+"/")
	sessionPage = body
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assertInOrder(
		body, "<title>Today, 2:32 PM - Logs</title>", `<a href="/">`, "Logs", "Today, 2:32 PM",
		`href="254.wpilog" download`, "<b>254</b>", ".wpilog", fmt.Sprintf("%d B", len(wpilog)),
		`href="971.wpilog" download`, "<b>971</b>", "3.4 MB",
		`href="1114.csv" download`, "<b>1114</b>", ".csv", fmt.Sprintf("%d B", len(csv)),
	)

	// 9. Following "Logs" from inside the session, resolved the way a browser resolves it, lands on the list.
	logsLink := regexp.MustCompile(`<a href="([^"]*)">Logs</a>`).FindStringSubmatch(body)
	if assert.NotNil(t, logsLink, "no Logs link in the session's title") {
		target, err := resp.Request.URL.Parse(logsLink[1])
		require.Nil(t, err)
		resp, body = request("GET", target.RequestURI())
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assertInOrder(body, "<title>Logs</title>", "<h1>Logs</h1>", `href="`+emptySession+`/"`, `href="`+session+`/"`)
	}

	resp, body = request("GET", "/"+emptySession+"/")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, "Nothing recorded in this session yet.")
	// Without the trailing slash, the file links would resolve outside the folder, so the server redirects to add it.
	resp, body = request("GET", "/"+session)
	assert.Equal(t, "/"+session+"/", resp.Request.URL.Path)
	assert.Contains(t, body, `href="254.wpilog"`)

	// 4 and 5. Downloads.
	for name, contents := range map[string]string{"254.wpilog": wpilog, "1114.csv": csv} {
		resp, body = request("GET", "/"+session+"/"+name)
		assert.Equal(t, http.StatusOK, resp.StatusCode, name)
		assert.Equal(t, contents, body, name)
		assert.Equal(t, "attachment", resp.Header.Get("Content-Disposition"), name)
	}

	// 6. Admin API.
	for _, path := range []string{"/api/status", "/api/settings", "/api/teams/254", "/api/telemetry"} {
		resp, _ = request("GET", path)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, path)
		assert.Empty(t, resp.Header.Get("Content-Disposition"), path)
	}

	// 7. Writes.
	for _, path := range []string{"/api/driver-stations", "/api/stations", "/" + session + "/254.wpilog"} {
		resp, _ = request("PUT", path)
		assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode, path)
	}

	// 8. Traversal, sent over raw TCP so no client cleans the path first.
	for _, path := range []string{"/../event.db", "/%2e%2e/event.db", "/" + session + "/../../event.db"} {
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
