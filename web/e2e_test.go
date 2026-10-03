// End-to-end test of the real HTTP API, field controller and database against a fake access point. Writes a transcript
// of every request, the AP configurations received and the final raw database records to e2e-output/ for inspection.

package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/Team254/cheesy-arena/field"
	"github.com/Team254/cheesy-arena/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/bbolt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Fake VH-113 API: records each configuration and reports every configured station as linked.
type fakeAccessPoint struct {
	mutex          sync.Mutex
	configurations []string
	stations       map[string]map[string]any
}

func (ap *fakeAccessPoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ap.mutex.Lock()
	defer ap.mutex.Unlock()
	switch r.URL.Path {
	case "/configuration":
		body, _ := io.ReadAll(r.Body)
		ap.configurations = append(ap.configurations, string(body))
		var request struct {
			StationConfigurations map[string]struct{ Ssid string } `json:"stationConfigurations"`
		}
		_ = json.Unmarshal(body, &request)
		ap.stations = map[string]map[string]any{}
		for station, config := range request.StationConfigurations {
			ap.stations[station] = map[string]any{
				"ssid": config.Ssid, "isLinked": true, "rxRateMbps": 144.0, "txRateMbps": 130.0,
				"signalNoiseRatio": 41, "connectionQuality": "excellent",
			}
		}
		w.WriteHeader(http.StatusAccepted)
	case "/status":
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ACTIVE", "stationStatuses": ap.stations})
	default:
		http.NotFound(w, r)
	}
}

func (ap *fakeAccessPoint) configurationCount() int {
	ap.mutex.Lock()
	defer ap.mutex.Unlock()
	return len(ap.configurations)
}

// Seeds a database the way full Cheesy Arena would leave it: settings and teams with fields this app doesn't know
// about, plus tables it doesn't use at all.
func seedFullCheesyArenaDb(t *testing.T, path string) {
	db, err := bbolt.Open(path, 0644, nil)
	require.Nil(t, err)
	defer db.Close()
	require.Nil(
		t, db.Update(
			func(tx *bbolt.Tx) error {
				put := func(bucket, key, value string) {
					b, err := tx.CreateBucketIfNotExists([]byte(bucket))
					require.Nil(t, err)
					require.Nil(t, b.Put([]byte(key), []byte(value)))
				}
				put("EventSettings", "1", `{"Id":1,"Name":"Chezy Champs","TbaSecret":"secret","ApChannel":5,`+
					`"SwitchAddress":"127.0.0.1","SwitchPassword":"sw","NetworkSecurityEnabled":true}`)
				put("Team", "254", `{"Id":254,"Nickname":"The Cheesy Poofs","WpaKey":"oldkey254"}`)
				put("Match", "1", `{"Id":1,"ShortName":"Q1"}`)
				return nil
			},
		),
	)
}

func dumpRawRecords(t *testing.T, path string) string {
	db, err := bbolt.Open(path, 0644, nil)
	require.Nil(t, err)
	defer db.Close()
	var out strings.Builder
	require.Nil(
		t, db.View(
			func(tx *bbolt.Tx) error {
				return tx.ForEach(
					func(name []byte, b *bbolt.Bucket) error {
						return b.ForEach(
							func(key, value []byte) error {
								fmt.Fprintf(&out, "%s/%s %s\n", name, key, value)
								return nil
							},
						)
					},
				)
			},
		),
	)
	return out.String()
}

func TestEndToEnd(t *testing.T) {
	var transcript strings.Builder
	note := func(format string, args ...any) { fmt.Fprintf(&transcript, format+"\n", args...) }
	t.Cleanup(
		func() {
			outputDir := filepath.Join("..", "e2e-output")
			_ = os.MkdirAll(outputDir, 0755)
			_ = os.WriteFile(filepath.Join(outputDir, "transcript.txt"), []byte(transcript.String()), 0644)
		},
	)

	logTail := NewLogTail(200)
	log.SetOutput(io.MultiWriter(os.Stderr, logTail))
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	ap := &fakeAccessPoint{}
	apServer := httptest.NewServer(ap)
	defer apServer.Close()
	apAddress := strings.TrimPrefix(apServer.URL, "http://")

	dbPath := filepath.Join(t.TempDir(), "event.db")
	seedFullCheesyArenaDb(t, dbPath)
	note("== Seeded database\n%s", dumpRawRecords(t, dbPath))

	// Starts the app on the given database the same way main does, minus the fixed port.
	startApp := func() (*model.Database, *httptest.Server) {
		database, err := model.OpenDatabase(dbPath)
		require.Nil(t, err)
		fieldController, err := field.New(
			database,
			field.Options{DriverStationPorts: field.DefaultDriverStationPorts, RecordingsDir: t.TempDir()},
		)
		require.Nil(t, err)
		go fieldController.Run()
		return database, httptest.NewServer(NewWeb(fieldController, logTail).newHandler())
	}
	database, server := startApp()

	call := func(method, path string, body any) (int, map[string]any) {
		var reader io.Reader
		requestJson := ""
		if body != nil {
			b, _ := json.Marshal(body)
			requestJson = string(b)
			reader = bytes.NewReader(b)
		}
		request, _ := http.NewRequest(method, server.URL+path, reader)
		response, err := http.DefaultClient.Do(request)
		require.Nil(t, err)
		defer response.Body.Close()
		responseJson, _ := io.ReadAll(response.Body)
		note("== %s %s %s\n-> %d %s", method, path, requestJson, response.StatusCode, responseJson)
		var decoded map[string]any
		_ = json.Unmarshal(responseJson, &decoded)
		return response.StatusCode, decoded
	}

	// The page itself is served from the embedded files.
	response, err := http.Get(server.URL + "/")
	require.Nil(t, err)
	page, _ := io.ReadAll(response.Body)
	response.Body.Close()
	assert.Contains(t, string(page), "FMS Config")

	// Settings from the copied database are visible and can be pointed at the (fake) AP.
	status, settings := call("GET", "/api/settings", nil)
	assert.Equal(t, 200, status)
	assert.Equal(t, "sw", settings["SwitchPassword"])
	settings["ApAddress"] = apAddress
	status, _ = call("PUT", "/api/settings", settings)
	assert.Equal(t, 200, status)

	// A team from the database prefills its password; an unknown team 404s.
	status, team := call("GET", "/api/teams/254", nil)
	assert.Equal(t, 200, status)
	assert.Equal(t, "oldkey254", team["wpaKey"])
	status, _ = call("GET", "/api/teams/9999", nil)
	assert.Equal(t, 404, status)

	// Invalid assignments are rejected without touching the AP.
	empty := field.Assignment{}
	for _, invalid := range [][6]field.Assignment{
		{{TeamId: 254, WpaKey: "12345678"}, empty, empty, {TeamId: 254, WpaKey: "12345678"}, empty, empty},
		{{TeamId: 254, WpaKey: "short"}, empty, empty, empty, empty, empty},
		{{TeamId: 25600, WpaKey: "12345678"}, empty, empty, empty, empty, empty},
		// No default WPA key is set yet, so a blank key is still too short.
		{{TeamId: 254}, empty, empty, empty, empty, empty},
	} {
		status, _ = call("PUT", "/api/stations", invalid)
		assert.Equal(t, 400, status)
	}
	assert.Equal(t, 0, ap.configurationCount())

	// A valid apply saves new keys (existing and new teams) and configures the AP.
	status, _ = call(
		"PUT", "/api/stations", [6]field.Assignment{
			{TeamId: 254, WpaKey: "newkey254"}, empty, empty, empty, {TeamId: 1114, WpaKey: "simbotics"}, empty,
		},
	)
	assert.Equal(t, 200, status)
	require.Equal(t, 1, ap.configurationCount())
	note("== AP configuration received\n%s", ap.configurations[0])
	assert.JSONEq(
		t,
		`{"channel":5,"stationConfigurations":{"red1":{"ssid":"254","wpaKey":"newkey254"},`+
			`"blue2":{"ssid":"1114","wpaKey":"simbotics"}}}`,
		ap.configurations[0],
	)

	// The AP monitoring loop picks up the linked radios.
	var liveStatus map[string]any
	assert.Eventually(
		t, func() bool {
			_, liveStatus = call("GET", "/api/status", nil)
			stations := liveStatus["stations"].([]any)
			wifi := stations[0].(map[string]any)["wifi"].(map[string]any)
			return liveStatus["accessPointStatus"] == "ACTIVE" && wifi["RadioLinked"] == true && wifi["TeamId"] == 254.0
		}, 5*time.Second, 250*time.Millisecond,
	)
	assert.Contains(t, strings.Join(logTail.Lines(), "\n"), "Applying stations R1=254 R2=- R3=- B1=- B2=1114 B3=-")

	// A default WPA key must itself be a valid key; a rejected one leaves the stored settings alone.
	settings["DefaultWpaKey"] = "short"
	status, _ = call("PUT", "/api/settings", settings)
	assert.Equal(t, 400, status)
	_, stored := call("GET", "/api/settings", nil)
	assert.Equal(t, "", stored["DefaultWpaKey"])
	settings["DefaultWpaKey"] = "fieldkey123"
	status, _ = call("PUT", "/api/settings", settings)
	assert.Equal(t, 200, status)

	// With a default set, a team with a blank key gets the default (on the AP and in its team record), a typed key still
	// wins, and empty stations stay empty.
	status, applied := call(
		"PUT", "/api/stations", [6]field.Assignment{
			{TeamId: 254, WpaKey: "newkey254"}, empty, empty, empty, {TeamId: 1114, WpaKey: "simbotics"}, {TeamId: 2056},
		},
	)
	assert.Equal(t, 200, status)
	appliedB3 := applied["stations"].([]any)[5].(map[string]any)["assignment"].(map[string]any)
	assert.Equal(t, map[string]any{"teamId": 2056.0, "wpaKey": "fieldkey123"}, appliedB3)
	require.Equal(t, 2, ap.configurationCount())
	note("== AP configuration received\n%s", ap.configurations[1])
	assert.JSONEq(
		t,
		`{"channel":5,"stationConfigurations":{"red1":{"ssid":"254","wpaKey":"newkey254"},`+
			`"blue2":{"ssid":"1114","wpaKey":"simbotics"},"blue3":{"ssid":"2056","wpaKey":"fieldkey123"}}}`,
		ap.configurations[1],
	)
	status, team = call("GET", "/api/teams/2056", nil)
	assert.Equal(t, 200, status)
	assert.Equal(t, "fieldkey123", team["wpaKey"])

	// After a restart the assignment is restored and the matching AP is left alone.
	server.Close()
	require.Nil(t, database.Close())
	note("== Restart")
	database, server = startApp()
	defer server.Close()
	defer database.Close()
	_, liveStatus = call("GET", "/api/status", nil)
	restored := liveStatus["stations"].([]any)[4].(map[string]any)["assignment"].(map[string]any)
	assert.Equal(t, map[string]any{"teamId": 1114.0, "wpaKey": "simbotics"}, restored)
	time.Sleep(2500 * time.Millisecond)
	assert.Equal(t, 2, ap.configurationCount(), "restart should not reconfigure a matching AP")

	// Fields and tables from full Cheesy Arena survive.
	require.Nil(t, database.Close())
	records := dumpRawRecords(t, dbPath)
	note("== Final database\n%s", records)
	assert.Contains(t, records, `"Name":"Chezy Champs"`)
	assert.Contains(t, records, `"TbaSecret":"secret"`)
	assert.Contains(t, records, `"Nickname":"The Cheesy Poofs"`)
	assert.Contains(t, records, `"WpaKey":"newkey254"`)
	assert.Contains(t, records, `"DefaultWpaKey":"fieldkey123"`)
	assert.Contains(t, records, `Match/1 {"Id":1,"ShortName":"Q1"}`)
	note("== Log tail\n%s", strings.Join(logTail.Lines(), "\n"))
}
