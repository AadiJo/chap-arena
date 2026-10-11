// End-to-end test of the team page that driver stations reach through the field switch: the real team handler, field
// controller and database against a fake access point, with each request made from the address a computer on that
// network would have. The admin API is used alongside it, the way the field operator would. Writes a transcript of every
// request, response and AP configuration to e2e-output/team-page.txt.
//
// Ways this can fail, each checked below:
//  1. A request from a station's network is matched to the wrong station, or to none (10.2.54.x is team 254, 10.1.18.x
//     is 118, 10.1.0.x is 100 and 10.0.5.x is 5).
//  2. A request from the admin network (10.0.100.x, which reads as team 100 unless the last two digits are checked, and
//     100 is on a station here), localhost, or a team that isn't on a station is matched to one, or can change a
//     station or topics.
//  3. The station view leaves out the station's own WPA key (the team needs it for its radio), or gives away another
//     station's key or team, or a settings password.
//  4. A team changing its station's number or password changes another station, on the AP or in the database.
//  5. A blank password doesn't use the team's saved one (so a team keeps its key, and a returning team gets its own), or
//     doesn't fall back to the default for a team without one.
//  6. A rejected change (a team on another station, a short password, no team, too big a number) changes anything.
//  7. A team's request after the operator moved it off its station still changes the station it used to hold.
//  8. A team saves topics for another team, or its topics don't reach telemetry.
//  9. The admin API answers on this port.
// 10. The page or one of its scripts isn't served.

package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/Team254/cheesy-arena/field"
	"github.com/Team254/cheesy-arena/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTeamPageEndToEnd(t *testing.T) {
	var transcript strings.Builder
	note := func(format string, args ...any) { fmt.Fprintf(&transcript, format+"\n", args...) }
	t.Cleanup(
		func() {
			outputDir := filepath.Join("..", "e2e-output")
			_ = os.MkdirAll(outputDir, 0755)
			_ = os.WriteFile(filepath.Join(outputDir, "team-page.txt"), []byte(transcript.String()), 0644)
		},
	)

	ap := &fakeAccessPoint{}
	apServer := httptest.NewServer(ap)
	defer apServer.Close()

	// Passwords saved at earlier events.
	database, err := model.OpenDatabase(filepath.Join(t.TempDir(), "event.db"))
	require.Nil(t, err)
	defer database.Close()
	savedKeys := map[int]string{
		254: "poofs254key", 1678: "citrus1678", 100: "team100key", 118: "robonauts118", 5: "team5key1", 971: "spartan971",
	}
	for teamId, wpaKey := range savedKeys {
		require.Nil(t, database.CreateTeam(&model.Team{Id: teamId, WpaKey: wpaKey}))
	}
	recordingsDir := t.TempDir()
	fieldController, err := field.New(
		database, field.Options{DriverStationPorts: field.DefaultDriverStationPorts, RecordingsDir: recordingsDir},
	)
	require.Nil(t, err)
	go fieldController.Run()
	admin := httptest.NewServer(NewWeb(fieldController, NewLogTail(200)).newHandler())
	defer admin.Close()
	team := NewTeamHandler(fieldController, recordingsDir)

	// The operator's side, over the admin API.
	adminCall := func(method, path string, body any) (int, string) {
		b, _ := json.Marshal(body)
		request, _ := http.NewRequest(method, admin.URL+path, bytes.NewReader(b))
		response, err := http.DefaultClient.Do(request)
		require.Nil(t, err)
		defer response.Body.Close()
		responseBody, _ := io.ReadAll(response.Body)
		note("== admin %s %s %s\n-> %d %s", method, path, b, response.StatusCode, responseBody)
		return response.StatusCode, string(responseBody)
	}
	// A request from the computer at address, as the field switch routes it: the server sees the computer's own address.
	from := func(address, method, path string, body any) (int, string) {
		var reader io.Reader = http.NoBody
		requestJson := ""
		if body != nil {
			b, _ := json.Marshal(body)
			requestJson = string(b)
			reader = bytes.NewReader(b)
		}
		request := httptest.NewRequest(method, path, reader)
		request.RemoteAddr = address + ":51234"
		recorder := httptest.NewRecorder()
		team.ServeHTTP(recorder, request)
		note("== %s %s %s from %s\n-> %d %s", method, path, requestJson, address, recorder.Code, recorder.Body)
		return recorder.Code, recorder.Body.String()
	}
	stationFrom := func(address string) (stationView, string) {
		status, body := from(address, "GET", "/api/station", nil)
		require.Equal(t, http.StatusOK, status, address)
		var view stationView
		require.Nil(t, json.Unmarshal([]byte(body), &view), body)
		return view, body
	}
	assignments := func() [6]field.Assignment {
		var result [6]field.Assignment
		for i, station := range fieldController.Status().Stations {
			result[i] = station.Assignment
		}
		return result
	}
	// The AP's view of the stations, from the last configuration it received.
	lastApConfiguration := func() map[string]stationConfigurationJson {
		ap.mutex.Lock()
		defer ap.mutex.Unlock()
		require.NotEmpty(t, ap.configurations)
		last := ap.configurations[len(ap.configurations)-1]
		note("== AP configuration received\n%s", last)
		var request struct {
			StationConfigurations map[string]stationConfigurationJson `json:"stationConfigurations"`
		}
		require.Nil(t, json.Unmarshal([]byte(last), &request))
		return request.StationConfigurations
	}

	// The operator points the app at the AP, sets a default password and fills four stations.
	status, body := adminCall("GET", "/api/settings", nil)
	require.Equal(t, http.StatusOK, status)
	var settings model.EventSettings
	require.Nil(t, json.Unmarshal([]byte(body), &settings))
	settings.ApAddress = strings.TrimPrefix(apServer.URL, "http://")
	settings.ApPassword = "apsecret"
	settings.SwitchPassword = "switchsecret"
	settings.DefaultWpaKey = "fieldkey123"
	status, _ = adminCall("PUT", "/api/settings", settings)
	require.Equal(t, http.StatusOK, status)
	empty := field.Assignment{}
	initial := [6]field.Assignment{
		{TeamId: 254, WpaKey: "poofs254key"}, {TeamId: 1678, WpaKey: "citrus1678"}, {TeamId: 100, WpaKey: "team100key"},
		{TeamId: 118, WpaKey: "robonauts118"}, {TeamId: 5, WpaKey: "team5key1"}, empty,
	}
	status, _ = adminCall("PUT", "/api/stations", initial)
	require.Equal(t, http.StatusOK, status)

	// 10. The page and everything it loads.
	status, body = from("10.2.54.20", "GET", "/", nil)
	assert.Equal(t, http.StatusOK, status)
	for _, script := range []string{"/topics.js", "/demo.js", "/helpers.js", "/topic-tree.js", "/team.js"} {
		assert.Contains(t, body, fmt.Sprintf(`<script src="%s"`, script))
		status, _ = from("10.2.54.20", "GET", script, nil)
		assert.Equal(t, http.StatusOK, status, script)
	}
	assert.Contains(t, body, `href="/topic-tree.css"`)
	status, _ = from("10.2.54.20", "GET", "/topic-tree.css", nil)
	assert.Equal(t, http.StatusOK, status)

	// 1 and 3. Each station's network sees its own station, team and password, and nothing else secret.
	for address, want := range map[string]struct {
		station string
		teamId  int
	}{
		"10.2.54.20": {"R1", 254}, "10.16.78.31": {"R2", 1678}, "10.1.0.20": {"R3", 100}, "10.1.18.199": {"B1", 118},
		"10.0.5.22": {"B2", 5},
	} {
		view, raw := stationFrom(address)
		assert.Equal(t, want.station, view.Station, address)
		assert.Equal(t, want.teamId, view.TeamId, address)
		assert.Equal(t, savedKeys[want.teamId], view.WpaKey, address)
		if assert.NotNil(t, view.Telemetry, address) {
			assert.Equal(t, want.teamId, view.Telemetry.TeamId, address)
		}
		for _, secret := range []string{
			"poofs254key", "citrus1678", "team100key", "robonauts118", "team5key1", "fieldkey123", "apsecret", "switchsecret",
		} {
			if secret != savedKeys[want.teamId] {
				assert.NotContains(t, raw, secret, address)
			}
		}
		for _, other := range []string{"254", "1678", "118"} {
			if other != fmt.Sprint(want.teamId) {
				assert.NotContains(t, raw, other, "%s sees team %s", address, other)
			}
		}
	}

	// 2. Nowhere else is a station, and nothing from there can change one.
	for _, address := range []string{"10.0.100.25", "127.0.0.1", "10.99.99.20", "192.168.1.40"} {
		view, _ := stationFrom(address)
		assert.Equal(t, "", view.Station, address)
		assert.Equal(t, "", view.WpaKey, address)
		assert.Nil(t, view.Telemetry, address)
		status, _ = from(address, "PUT", "/api/station", field.Assignment{TeamId: 9999, WpaKey: "takeover99"})
		assert.Equal(t, http.StatusForbidden, status, address)
		status, _ = from(address, "PUT", "/api/topics", map[string]any{"topics": []string{"/Stolen"}})
		assert.Equal(t, http.StatusForbidden, status, address)
	}
	assert.Equal(t, initial, assignments())
	status, body = adminCall("GET", "/api/telemetry", nil)
	assert.NotContains(t, body, "/Stolen")

	// 4. Team 254's computer in R1 hands the station to 2056 with a new password. Only R1 changes.
	status, _ = from("10.2.54.20", "PUT", "/api/station", field.Assignment{TeamId: 2056, WpaKey: "ofs2056key"})
	assert.Equal(t, http.StatusOK, status)
	want := initial
	want[0] = field.Assignment{TeamId: 2056, WpaKey: "ofs2056key"}
	assert.Equal(t, want, assignments())
	assert.Equal(
		t, map[string]stationConfigurationJson{
			"red1": {"2056", "ofs2056key"}, "red2": {"1678", "citrus1678"}, "red3": {"100", "team100key"},
			"blue1": {"118", "robonauts118"}, "blue2": {"5", "team5key1"},
		}, lastApConfiguration(),
	)
	for teamId, wpaKey := range savedKeys {
		saved, _, err := fieldController.TeamWpaKey(teamId)
		require.Nil(t, err)
		assert.Equal(t, wpaKey, saved, "team %d's saved password", teamId)
	}
	saved, _, _ := fieldController.TeamWpaKey(2056)
	assert.Equal(t, "ofs2056key", saved)
	// 254's computer is still on 254's network until it's replugged, and that network no longer has a station.
	view, _ := stationFrom("10.2.54.20")
	assert.Equal(t, "", view.Station)
	view, _ = stationFrom("10.20.56.20")
	assert.Equal(t, "R1", view.Station)

	// 5. Blank passwords: the same team keeps the key it just changed to.
	status, _ = from("10.16.78.31", "PUT", "/api/station", field.Assignment{TeamId: 1678, WpaKey: "citrusnew1"})
	assert.Equal(t, http.StatusOK, status)
	status, _ = from("10.16.78.31", "PUT", "/api/station", field.Assignment{TeamId: 1678})
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, field.Assignment{TeamId: 1678, WpaKey: "citrusnew1"}, assignments()[1])
	assert.Equal(t, stationConfigurationJson{"1678", "citrusnew1"}, lastApConfiguration()["red2"])
	// A new team with a saved password gets it.
	status, _ = from("10.20.56.20", "PUT", "/api/station", field.Assignment{TeamId: 971})
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, field.Assignment{TeamId: 971, WpaKey: "spartan971"}, assignments()[0])
	// A new team without one gets the default.
	status, _ = from("10.9.71.20", "PUT", "/api/station", field.Assignment{TeamId: 4414})
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, field.Assignment{TeamId: 4414, WpaKey: "fieldkey123"}, assignments()[0])
	assert.Equal(t, stationConfigurationJson{"4414", "fieldkey123"}, lastApConfiguration()["red1"])
	view, _ = stationFrom("10.44.14.20")
	assert.Equal(t, "fieldkey123", view.WpaKey, "a station on the default key shows it")

	// 6. Rejected changes leave the stations and the AP alone.
	before := assignments()
	configurations := ap.configurationCount()
	for _, invalid := range []field.Assignment{
		{TeamId: 1678}, {TeamId: 4414, WpaKey: "short"}, {TeamId: 0}, {TeamId: 25600, WpaKey: "longenough"},
	} {
		status, _ = from("10.44.14.20", "PUT", "/api/station", invalid)
		assert.Equal(t, http.StatusBadRequest, status, "%+v", invalid)
	}
	assert.Equal(t, before, assignments())
	assert.Equal(t, configurations, ap.configurationCount())

	// 7. The operator puts 254 back in R1. 4414's computer, still on 4414's network, can't change R1 any more.
	moved := before
	moved[0] = field.Assignment{TeamId: 254, WpaKey: "poofs254key"}
	status, _ = adminCall("PUT", "/api/stations", moved)
	require.Equal(t, http.StatusOK, status)
	status, _ = from("10.44.14.20", "PUT", "/api/station", field.Assignment{TeamId: 4414, WpaKey: "fieldkey123"})
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, moved, assignments())

	// 8. Topics go to the team the request comes from, whatever team the body names.
	entries := []string{"/AdvantageKit/RealOutputs/Odometry/", "!/AdvantageKit/RealOutputs/Odometry/Trajectory"}
	status, body = from("10.2.54.20", "PUT", "/api/topics", map[string]any{"teamId": 1678, "topics": entries})
	assert.Equal(t, http.StatusOK, status)
	var afterTopics stationView
	require.Nil(t, json.Unmarshal([]byte(body), &afterTopics))
	if assert.NotNil(t, afterTopics.Telemetry) {
		assert.Equal(t, entries, afterTopics.Telemetry.Entries)
	}
	status, body = adminCall("GET", "/api/telemetry", nil)
	var telemetry field.TelemetryStatus
	require.Nil(t, json.Unmarshal([]byte(body), &telemetry))
	recorded := map[int][]string{}
	for _, team := range telemetry.Teams {
		recorded[team.TeamId] = team.Entries
	}
	assert.Equal(t, entries, recorded[254])
	assert.Empty(t, recorded[1678])
	tooMany := make([]string, 51)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("/Topic%d", i)
	}
	status, _ = from("10.2.54.20", "PUT", "/api/topics", map[string]any{"topics": tooMany})
	assert.Equal(t, http.StatusBadRequest, status)
	_, body = adminCall("GET", "/api/telemetry", nil)
	assert.NotContains(t, body, "/Topic0")

	// 9. None of the admin API is here.
	for _, call := range []struct{ method, path string }{
		{"GET", "/api/status"}, {"GET", "/api/settings"}, {"GET", "/api/teams/254"}, {"GET", "/api/telemetry"},
		{"PUT", "/api/stations"}, {"PUT", "/api/settings"}, {"PUT", "/api/driver-stations"}, {"PUT", "/api/recording"},
		{"PUT", "/api/teams/254/topics"}, {"GET", "/app.js"}, {"GET", "/index.html"},
	} {
		status, body = from("10.2.54.20", call.method, call.path, nil)
		assert.Contains(t, []int{http.StatusNotFound, http.StatusMethodNotAllowed}, status, "%s %s", call.method, call.path)
		assert.NotContains(t, body, "apsecret", "%s %s", call.method, call.path)
	}
	assert.Equal(t, moved, assignments())
}

// One station in an AP configuration request.
type stationConfigurationJson struct {
	Ssid   string `json:"ssid"`
	WpaKey string `json:"wpaKey"`
}
