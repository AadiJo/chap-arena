// The team page: the server on its own port (80 by default) that the field switch lets team networks reach, apart
// from the admin UI. From a station's network, a team can change that station's team number and radio password, pick
// what's recorded from its robot, and download every recording. The station comes from the request's address: the
// switch gives each assigned team the 10.TE.AM.0/24 network (10.2.54.x is team 254), and the network only exists while
// the team is on a station. From anywhere else (the admin network, this computer) the page can only download.
//
// The page is in team/, embedded in the binary, and shares topics.js, helpers.js, topic-tree.js, topic-tree.css and
// demo.js with the admin UI in static/.

package web

import (
	"embed"
	"encoding/json"
	"errors"
	"github.com/Team254/cheesy-arena/field"
	"github.com/Team254/cheesy-arena/network"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
)

//go:embed team
var teamFiles embed.FS

// What the team page shows the computer that asked: its station, team, radio password and link, and what's recorded
// from its robot. Station is "" and the rest empty when the request didn't come from a station's network. The password
// is the one the app last sent the AP for this station (the AP only reports a hash of it); no other station's shows.
type stationView struct {
	Station   string                  `json:"station"`
	TeamId    int                     `json:"teamId"`
	WpaKey    string                  `json:"wpaKey"`
	Wifi      *network.TeamWifiStatus `json:"wifi"`
	Telemetry *field.TelemetryTeam    `json:"telemetry"`
}

type teamPage struct {
	field         *field.Field
	recordingsDir string
}

var errNotOnStation = errors.New("this computer isn't on a station's network")

// Builds the team page's handler. Only these routes exist, so nothing from the admin API is reachable through it.
func NewTeamHandler(field *field.Field, recordingsDir string) http.Handler {
	page := &teamPage{field: field, recordingsDir: recordingsDir}
	team, err := fs.Sub(teamFiles, "team")
	if err != nil {
		panic(err)
	}
	static, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /{$}", serveFile(team, "index.html"))
	mux.Handle("GET /team.js", serveFile(team, "team.js"))
	for _, name := range []string{"topics.js", "helpers.js", "topic-tree.js", "topic-tree.css", "demo.js", "favicon.png"} {
		mux.Handle("GET /"+name, serveFile(static, name))
	}
	mux.HandleFunc("GET /api/station", page.stationGetHandler)
	mux.HandleFunc("PUT /api/station", page.stationPutHandler)
	mux.HandleFunc("PUT /api/topics", page.topicsPutHandler)
	mux.HandleFunc("GET /api/recordings", page.recordingsGetHandler)
	mux.Handle("GET /recordings/", http.StripPrefix("/recordings", newRecordingFiles(recordingsDir)))
	return mux
}

func serveFile(fsys fs.FS, name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.ServeFileFS(w, r, fsys, name) })
}

// The team whose station network an address is on: 10.TE.AM.x is team TEAM (10.2.54.x is 254, 10.0.5.x is 5).
// Returns 0 for anything else, including the admin network 10.0.100.x, since 100 can't be a team's last two digits.
func teamOfAddress(remoteAddr string) int {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return 0
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return 0
	}
	if address = address.Unmap(); !address.Is4() {
		return 0
	}
	octets := address.As4()
	if octets[0] != 10 || octets[2] > 99 {
		return 0
	}
	return int(octets[1])*100 + int(octets[2])
}

// The station view for the computer at remoteAddr.
func (page *teamPage) view(remoteAddr string) stationView {
	teamId := teamOfAddress(remoteAddr)
	if teamId == 0 {
		return stationView{}
	}
	for _, station := range page.field.Status().Stations {
		if station.Assignment.TeamId != teamId {
			continue
		}
		view := stationView{
			Station: station.Station, TeamId: teamId, WpaKey: station.Assignment.WpaKey, Wifi: &station.Wifi,
		}
		for _, team := range page.field.Telemetry().Teams {
			if team.TeamId == teamId {
				view.Telemetry = &team
				break
			}
		}
		return view
	}
	return stationView{}
}

func (page *teamPage) stationGetHandler(w http.ResponseWriter, r *http.Request) {
	writeJson(w, http.StatusOK, page.view(r.RemoteAddr))
}

// Changes the requesting computer's station to {"teamId": 1678, "wpaKey": "..."}, where a blank key uses the team's
// saved one (see field.ReassignStation), and responds with the station view. That's empty once the team number
// changes, since the computer stays on its old team's network until its Ethernet is replugged. Responds 403 unless the
// request came from a station's network, and 400 for a change field.ReassignStation rejects.
func (page *teamPage) stationPutHandler(w http.ResponseWriter, r *http.Request) {
	view := page.view(r.RemoteAddr)
	if view.Station == "" {
		writeError(w, http.StatusForbidden, errNotOnStation)
		return
	}
	var assignment field.Assignment
	if err := json.NewDecoder(r.Body).Decode(&assignment); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := page.field.ReassignStation(view.TeamId, assignment); err != nil {
		writeFieldError(w, err)
		return
	}
	page.stationGetHandler(w, r)
}

// Replaces what's recorded from the requesting station's robot with {"topics": [...]} (see field.SetTeamTopics) and
// responds with the station view. Responds 403 unless the request came from a station's network, and 400 for a list
// field.SetTeamTopics rejects.
func (page *teamPage) topicsPutHandler(w http.ResponseWriter, r *http.Request) {
	view := page.view(r.RemoteAddr)
	if view.Station == "" {
		writeError(w, http.StatusForbidden, errNotOnStation)
		return
	}
	var request struct {
		Topics []string `json:"topics"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := page.field.SetTeamTopics(view.TeamId, request.Topics); err != nil {
		writeFieldError(w, err)
		return
	}
	page.stationGetHandler(w, r)
}

func (page *teamPage) recordingsGetHandler(w http.ResponseWriter, r *http.Request) {
	listing, err := listRecordings(page.recordingsDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJson(w, http.StatusOK, listing)
}
