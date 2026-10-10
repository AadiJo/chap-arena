// End-to-end test of the read-only recordings page that driver stations reach through the field switch, over a real
// listener. Writes a transcript of every request and response to e2e-output/recordings-page.txt for inspection.
//
// Ways this can fail, each checked below:
//  1. The session folders or the files in them aren't listed.
//  2. A file comes back with different bytes than were recorded.
//  3. A file opens in the browser (a CSV shows as text) instead of downloading.
//  4. The admin API is reachable on this port, letting a driver station enable robots or read WPA keys and passwords.
//  5. A write method is accepted.
//  6. A path with .. (plain or percent-encoded) escapes the recordings folder and serves event.db beside it.

package web

import (
	"bufio"
	"fmt"
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
)

func TestRecordingsPageEndToEnd(t *testing.T) {
	var transcript strings.Builder
	note := func(format string, args ...any) { fmt.Fprintf(&transcript, format+"\n", args...) }
	t.Cleanup(
		func() {
			outputDir := filepath.Join("..", "e2e-output")
			_ = os.MkdirAll(outputDir, 0755)
			_ = os.WriteFile(filepath.Join(outputDir, "recordings-page.txt"), []byte(transcript.String()), 0644)
		},
	)

	// Lay out the folder next to the database the way main and the recorder do.
	baseDir := t.TempDir()
	secret := "event.db contents: WpaKey=secret254"
	require.Nil(t, os.WriteFile(filepath.Join(baseDir, "event.db"), []byte(secret), 0644))
	recordingsDir := filepath.Join(baseDir, "recordings")
	sessionDir := filepath.Join(recordingsDir, "2026-10-10_150405")
	require.Nil(t, os.MkdirAll(sessionDir, 0755))
	wpilog := "WPILOG\x00\x01\x00\x00binary bytes"
	csv := "Timestamp,Key,Value\n0.02,NT:/Robot/Pose/translation/x,1.5\n"
	require.Nil(t, os.WriteFile(filepath.Join(sessionDir, "254.wpilog"), []byte(wpilog), 0644))
	require.Nil(t, os.WriteFile(filepath.Join(sessionDir, "1114.csv"), []byte(csv), 0644))

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

	// 1. Listings.
	resp, body := request("GET", "/")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, `href="2026-10-10_150405/"`)
	assert.Empty(t, resp.Header.Get("Content-Disposition"))
	resp, body = request("GET", "/2026-10-10_150405/")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, `href="254.wpilog"`)
	assert.Contains(t, body, `href="1114.csv"`)

	// 2 and 3. Downloads.
	for name, contents := range map[string]string{"254.wpilog": wpilog, "1114.csv": csv} {
		resp, body = request("GET", "/2026-10-10_150405/"+name)
		assert.Equal(t, http.StatusOK, resp.StatusCode, name)
		assert.Equal(t, contents, body, name)
		assert.Equal(t, "attachment", resp.Header.Get("Content-Disposition"), name)
	}

	// 4. Admin API.
	for _, path := range []string{"/api/status", "/api/settings", "/api/teams/254", "/api/telemetry"} {
		resp, _ = request("GET", path)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, path)
		assert.Empty(t, resp.Header.Get("Content-Disposition"), path)
	}

	// 5. Writes.
	for _, path := range []string{"/api/driver-stations", "/api/stations", "/2026-10-10_150405/254.wpilog"} {
		resp, _ = request("PUT", path)
		assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode, path)
	}

	// 6. Traversal, sent over raw TCP so no client cleans the path first.
	for _, path := range []string{"/../event.db", "/%2e%2e/event.db", "/2026-10-10_150405/../../event.db"} {
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
