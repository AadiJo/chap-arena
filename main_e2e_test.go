// End-to-end test of updating from the web UI's API: builds this app twice, as v0.7.1 and v0.8.0, publishes v0.8.0 on a
// fake GitHub, and has the running v0.7.1 replace and restart itself. Writes a transcript of every request and both
// runs' output to e2e-output/self-update.txt for inspection.
//
// Ways this can fail, each checked below:
//  1. The newer release isn't offered (version not stamped by -ldflags, or the asset name doesn't match release.yml).
//  2. An update starts while recording, and the restart cuts the recording short.
//  3. A download that doesn't match GitHub's sha256 digest gets installed.
//  4. A failed install changes or loses the executable, or leaves temporary files next to it.
//  5. The new executable isn't put in place, or the old one isn't kept as a backup.
//  6. The process doesn't restart into the new version, e.g. because event.db or the port is still held.
//  7. The restarted process opens another browser window (-no-browser isn't passed on).
//  8. Settings in event.db don't survive the update.

package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/Team254/cheesy-arena/update"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Collects a child process's output, which keeps arriving on the same pipe after it restarts itself.
type lockedBuffer struct {
	mutex  sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.buffer.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.buffer.String()
}

func TestSelfUpdateEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows restarts by starting a new process, which this test can't follow")
	}
	var transcript strings.Builder
	note := func(format string, args ...any) { fmt.Fprintf(&transcript, format+"\n", args...) }
	output := &lockedBuffer{}
	t.Cleanup(
		func() {
			note("\n== Output of both runs\n%s", output.String())
			outputDir := "e2e-output"
			_ = os.MkdirAll(outputDir, 0755)
			_ = os.WriteFile(filepath.Join(outputDir, "self-update.txt"), []byte(transcript.String()), 0644)
		},
	)

	// Fake GitHub, serving v0.8.0 as the latest release. While corrupt is set, downloads come back damaged, as if
	// mangled in transit.
	assetName, ok := update.AssetName("v0.8.0")
	require.True(t, ok)
	var githubMutex sync.Mutex
	var bundle []byte
	var digest string
	corrupt := true
	github := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				githubMutex.Lock()
				defer githubMutex.Unlock()
				note("GitHub: %s %s", r.Method, r.URL.Path)
				switch r.URL.Path {
				case "/releases/latest":
					_ = json.NewEncoder(w).Encode(
						map[string]any{
							"tag_name":     "v0.8.0",
							"published_at": "2026-10-08T12:00:00Z",
							"assets": []map[string]any{
								{
									"name":                 assetName,
									"browser_download_url": "http://" + r.Host + "/download/" + assetName,
									"size":                 len(bundle),
									"digest":               digest,
								},
							},
						},
					)
				case "/download/" + assetName:
					served := slices.Clone(bundle)
					if corrupt {
						served[len(served)/2] ^= 0xff
					}
					_, _ = w.Write(served)
				default:
					http.NotFound(w, r)
				}
			},
		),
	)
	defer github.Close()

	// Build both versions the way release.yml does, pointed at the fake GitHub.
	build := func(version, path string) []byte {
		ldflags := fmt.Sprintf("-X main.version=%s -X main.releasesUrl=%s/releases/latest", version, github.URL)
		out, err := exec.Command("go", "build", "-buildvcs=false", "-ldflags", ldflags, "-o", path, ".").CombinedOutput()
		require.Nil(t, err, string(out))
		contents, err := os.ReadFile(path)
		require.Nil(t, err)
		return contents
	}
	appDir := t.TempDir()
	executable := filepath.Join(appDir, "chap-arena")
	oldBinary := build("v0.7.1", executable)
	newBinary := build("v0.8.0", filepath.Join(t.TempDir(), "chap-arena"))
	var zipped bytes.Buffer
	archive := zip.NewWriter(&zipped)
	file, err := archive.Create("chap-arena/chap-arena")
	require.Nil(t, err)
	_, err = file.Write(newBinary)
	require.Nil(t, err)
	require.Nil(t, archive.Close())
	sum := sha256.Sum256(zipped.Bytes())
	githubMutex.Lock()
	bundle = zipped.Bytes()
	digest = "sha256:" + hex.EncodeToString(sum[:])
	githubMutex.Unlock()
	note("== Built v0.7.1 (%d bytes) and v0.8.0 (%d bytes); bundle %s is %d bytes", len(oldBinary), len(newBinary),
		assetName, len(bundle))

	freePort := func() string {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.Nil(t, err)
		defer listener.Close()
		return fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
	}
	port := freePort()
	app := exec.Command(executable, "-port", port, "-team-port", freePort(), "-no-browser")
	app.Stdout, app.Stderr = output, output
	require.Nil(t, app.Start())
	t.Cleanup(
		func() {
			_ = app.Process.Kill()
			_ = app.Wait()
		},
	)

	baseUrl := "http://127.0.0.1:" + port
	request := func(method, path string, body any) (int, string, error) {
		var reader io.Reader
		if body != nil {
			encoded, _ := json.Marshal(body)
			reader = bytes.NewReader(encoded)
		}
		req, err := http.NewRequest(method, baseUrl+path, reader)
		require.Nil(t, err)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		responseBody, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(responseBody), nil
	}
	call := func(method, path string, body any) (int, string) {
		code, responseBody, err := request(method, path, body)
		require.Nil(t, err)
		note("%s %s -> %d %s", method, path, code, strings.TrimSpace(responseBody))
		return code, responseBody
	}
	updateStatus := func() (update.Status, error) {
		_, responseBody, err := request("GET", "/api/status", nil)
		if err != nil {
			return update.Status{}, err
		}
		var status struct {
			Update update.Status `json:"update"`
		}
		err = json.Unmarshal([]byte(responseBody), &status)
		return status.Update, err
	}
	// Polls the status (riding out the restart) until done says so.
	waitForUpdate := func(what string, done func(update.Status) bool) update.Status {
		var status update.Status
		var err error
		require.Eventually(
			t, func() bool {
				status, err = updateStatus()
				return err == nil && done(status)
			}, 30*time.Second, 100*time.Millisecond, "%s; last status %+v, error %v", what, status, err,
		)
		encoded, _ := json.Marshal(status)
		note("Update status once %s: %s", what, encoded)
		return status
	}
	listAppDir := func() []string {
		entries, err := os.ReadDir(appDir)
		require.Nil(t, err)
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		return names
	}

	note("\n== 1. v0.7.1 offers v0.8.0")
	status := waitForUpdate(
		"v0.8.0 is offered", func(s update.Status) bool { return s.Available != nil && s.Available.Version == "v0.8.0" },
	)
	assert.Equal(t, "v0.7.1", status.Current)
	assert.Equal(t, int64(len(bundle)), status.Available.Size)

	// Save a setting to check it survives the update.
	_, settingsBody := call("GET", "/api/settings", nil)
	var settings map[string]any
	require.Nil(t, json.Unmarshal([]byte(settingsBody), &settings))
	settings["DefaultWpaKey"] = "survives-update"
	code, _ := call("PUT", "/api/settings", settings)
	require.Equal(t, http.StatusOK, code)

	note("\n== 2. Refused while recording")
	code, _ = call("PUT", "/api/recording", map[string]any{"active": true})
	require.Equal(t, http.StatusOK, code)
	code, body := call("POST", "/api/update", nil)
	assert.Equal(t, http.StatusConflict, code)
	assert.Contains(t, body, "stop recording to update")
	code, _ = call("PUT", "/api/recording", map[string]any{"active": false})
	require.Equal(t, http.StatusOK, code)

	note("\n== 3 and 4. A corrupted download isn't installed")
	code, _ = call("POST", "/api/update", nil)
	require.Equal(t, http.StatusOK, code)
	status = waitForUpdate("the bad download fails", func(s update.Status) bool { return s.State == update.Failed })
	assert.Contains(t, status.Error, "doesn't match its sha256 digest")
	contents, err := os.ReadFile(executable)
	require.Nil(t, err)
	assert.True(t, bytes.Equal(oldBinary, contents), "executable changed by a failed install")
	note("Files next to the executable: %v", listAppDir())
	assert.Equal(t, []string{"chap-arena", "cheesy-arena.log", "event.db", "recordings"}, listAppDir())

	note("\n== 5 to 8. Retried with a good download, it installs and restarts into v0.8.0")
	githubMutex.Lock()
	corrupt = false
	githubMutex.Unlock()
	code, _ = call("POST", "/api/update", nil)
	require.Equal(t, http.StatusOK, code)
	status = waitForUpdate("running v0.8.0", func(s update.Status) bool { return s.Current == "v0.8.0" })
	assert.Nil(t, status.Available, "v0.8.0 is the latest, so nothing should be offered")
	assert.Equal(t, update.Idle, status.State)

	// Restarting on Linux and macOS replaces the process in place, so the original process is the one serving v0.8.0.
	assert.Nil(t, app.Process.Signal(syscall.Signal(0)), "original process exited")
	if runtime.GOOS == "linux" {
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", app.Process.Pid))
		require.Nil(t, err)
		args := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
		note("Restarted with arguments %q", args)
		assert.Equal(t, 1, strings.Count(strings.Join(args, " "), "-no-browser"), "-no-browser should be passed on once")
	}
	contents, err = os.ReadFile(executable)
	require.Nil(t, err)
	assert.True(t, bytes.Equal(newBinary, contents), "executable isn't v0.8.0")
	contents, err = os.ReadFile(executable + ".old")
	require.Nil(t, err)
	assert.True(t, bytes.Equal(oldBinary, contents), "chap-arena.old isn't v0.7.1")
	note("Files next to the executable: %v", listAppDir())
	assert.Equal(
		t, []string{"chap-arena", "chap-arena.old", "cheesy-arena.log", "event.db", "recordings"}, listAppDir(),
	)
	_, settingsBody = call("GET", "/api/settings", nil)
	assert.Contains(t, settingsBody, `"DefaultWpaKey":"survives-update"`)
}
