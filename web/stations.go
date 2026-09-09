// Copyright 2026 Advait Johari. All Rights Reserved.
//
// Web routes for the station configuration page: the six team numbers and the button that pushes
// them to the radio and the switch.

package web

import (
	"encoding/json"
	"fmt"
	"github.com/AadiJo/chap-arena/field"
	"net/http"
	"strconv"
	"strings"
)

// One row of the station configuration form.
type stationView struct {
	Key      string
	Name     string
	Alliance string
	TeamId   int
	WpaKey   string
}

// Everything the station page needs to render.
type stationsPageView struct {
	pageView
	Stations     [6]stationView
	ErrorMessage string
}

// The payload polled by the station page to track hardware state.
type statusResponse struct {
	Radio        string               `json:"radio"`
	Switch       string               `json:"switch"`
	RadioEnabled bool                 `json:"radioEnabled"`
	Apply        field.ApplyStatus    `json:"apply"`
	Stations     []stationStatusEntry `json:"stations"`
}

type stationStatusEntry struct {
	Name string `json:"name"`

	// The team this station is assigned to. Zero means the station is bypassed, which the page shows
	// differently from a station that has a team but no radio link.
	ConfiguredTeamId int `json:"configuredTeamId"`

	// The team the access point reports as actually associated with this station.
	TeamId            int     `json:"teamId"`
	Linked            bool    `json:"linked"`
	SignalNoiseRatio  int     `json:"signalNoiseRatio"`
	ConnectionQuality int     `json:"connectionQuality"`
	BandwidthMbps     float64 `json:"bandwidthMbps"`
}

// Shows the station configuration page.
func (web *Web) stationsGetHandler(w http.ResponseWriter, r *http.Request) {
	if !web.userIsAdmin(w, r) {
		return
	}
	web.renderStations(w, web.field.Assignments(), "")
}

// Validates and applies the submitted station assignment.
func (web *Web) stationsApplyPostHandler(w http.ResponseWriter, r *http.Request) {
	if !web.userIsAdmin(w, r) {
		return
	}

	var assignments [6]field.Assignment
	for i, key := range field.StationKeys {
		assignments[i].WpaKey = strings.TrimSpace(r.PostFormValue(key + "Key"))

		value := r.PostFormValue(key)
		if value == "" {
			continue
		}
		teamId, err := strconv.Atoi(value)
		if err != nil {
			web.renderStations(
				w, assignments, fmt.Sprintf("%s: %q is not a team number.", field.StationNames[i], value),
			)
			return
		}
		assignments[i].TeamId = teamId
	}

	if err := web.field.Apply(assignments); err != nil {
		web.renderStations(w, assignments, err.Error())
		return
	}
	http.Redirect(w, r, "/", 303)
}

// Reports hardware state as JSON so the station page can poll it without a websocket.
func (web *Web) statusApiHandler(w http.ResponseWriter, r *http.Request) {
	if !web.userIsAdmin(w, r) {
		return
	}

	wifiStatuses := web.field.WifiStatuses()
	configured := web.field.Settings.StationTeamIds()
	response := statusResponse{
		Radio:        web.field.AccessPoint.Status,
		Switch:       web.field.Switch.Status,
		RadioEnabled: web.field.Settings.RadioEnabled,
		Apply:        web.field.ApplyStatus(),
		Stations:     make([]stationStatusEntry, 0, len(wifiStatuses)),
	}
	for i, wifiStatus := range wifiStatuses {
		response.Stations = append(
			response.Stations,
			stationStatusEntry{
				Name:              field.StationNames[i],
				ConfiguredTeamId:  configured[i],
				TeamId:            wifiStatus.TeamId,
				Linked:            wifiStatus.RadioLinked,
				SignalNoiseRatio:  wifiStatus.SignalNoiseRatio,
				ConnectionQuality: wifiStatus.ConnectionQuality,
				BandwidthMbps:     wifiStatus.MBits,
			},
		)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		handleWebErr(w, err)
	}
}

func (web *Web) renderStations(w http.ResponseWriter, assignments [6]field.Assignment, errorMessage string) {
	template, err := web.parseFiles("templates/stations.html", "templates/base.html")
	if err != nil {
		handleWebErr(w, err)
		return
	}

	data := stationsPageView{
		pageView:     pageView{EventSettings: web.field.Settings, Page: "stations"},
		ErrorMessage: errorMessage,
	}
	for i := range data.Stations {
		alliance := "red"
		if i >= 3 {
			alliance = "blue"
		}
		data.Stations[i] = stationView{
			Key:      field.StationKeys[i],
			Name:     field.StationNames[i],
			Alliance: alliance,
			TeamId:   assignments[i].TeamId,
			WpaKey:   assignments[i].WpaKey,
		}
	}

	if err = template.ExecuteTemplate(w, "base", data); err != nil {
		handleWebErr(w, err)
	}
}
