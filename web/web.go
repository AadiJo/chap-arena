// Copyright 2014 Team 254. All Rights Reserved.
// Author: pat@patfairbank.com (Patrick Fairbank)
//
// Serves the single-page UI (embedded in the binary) and the JSON API it uses.

package web

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Team254/cheesy-arena/field"
	"github.com/Team254/cheesy-arena/model"
	"github.com/Team254/cheesy-arena/update"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strconv"
)

//go:embed static
var staticFiles embed.FS

type Web struct {
	field   *field.Field
	logTail *LogTail
	updater *update.Updater
}

func NewWeb(field *field.Field, logTail *LogTail, updater *update.Updater) *Web {
	return &Web{field: field, logTail: logTail, updater: updater}
}

// Serves HTTP on an already-open listener and blocks forever. Taking a listener lets the caller know the port is bound
// before it points a browser at it.
func (web *Web) Serve(listener net.Listener) {
	log.Printf("Serving HTTP requests on %s", listener.Addr())
	log.Fatal(http.Serve(listener, web.newHandler()))
}

func (web *Web) newHandler() http.Handler {
	static, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /api/status", web.statusHandler)
	mux.HandleFunc("PUT /api/stations", web.stationsPutHandler)
	mux.HandleFunc("PUT /api/driver-stations", web.driverStationsPutHandler)
	mux.HandleFunc("PUT /api/recording", web.recordingPutHandler)
	mux.HandleFunc("GET /api/telemetry", web.telemetryGetHandler)
	mux.HandleFunc("PUT /api/teams/{teamId}/topics", web.teamTopicsPutHandler)
	mux.HandleFunc("GET /api/teams/{teamId}", web.teamGetHandler)
	mux.HandleFunc("GET /api/settings", web.settingsGetHandler)
	mux.HandleFunc("PUT /api/settings", web.settingsPutHandler)
	mux.HandleFunc("POST /api/update", web.updatePostHandler)
	return mux
}

type statusResponse struct {
	field.Status
	Log    []string      `json:"log"`
	Update update.Status `json:"update"`
}

func (web *Web) statusHandler(w http.ResponseWriter, r *http.Request) {
	writeJson(
		w,
		http.StatusOK,
		statusResponse{Status: web.field.Status(), Log: web.logTail.Lines(), Update: web.updater.Status()},
	)
}

// Applies the posted assignment for all six stations. Responds 400 if the assignment was rejected before saving, 500
// for any later failure (e.g. saved, but the access point was unreachable).
func (web *Web) stationsPutHandler(w http.ResponseWriter, r *http.Request) {
	var assignments [6]field.Assignment
	if err := json.NewDecoder(r.Body).Decode(&assignments); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := web.field.Apply(assignments); err != nil {
		writeFieldError(w, err)
		return
	}
	web.statusHandler(w, r)
}

// Sets the driver station mode from {"mode": "off" | "disabled" | "enabled"} and responds with the new status. Responds
// 400 for an unknown mode and 500 if the FMS ports couldn't be opened.
func (web *Web) driverStationsPutHandler(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Mode field.DriverStationMode `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if request.Mode == "" {
		writeError(w, http.StatusBadRequest, errors.New("mode is required"))
		return
	}
	if err := web.field.SetDriverStationMode(request.Mode); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	web.statusHandler(w, r)
}

// Starts or stops recording from {"active": true | false} and responds with the new status. Responds 500 if the
// recording folder couldn't be created.
func (web *Web) recordingPutHandler(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Active *bool `json:"active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if request.Active == nil {
		writeError(w, http.StatusBadRequest, errors.New("active is required"))
		return
	}
	if err := web.field.SetRecording(*request.Active); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	web.statusHandler(w, r)
}

// Lists the teams on the field or with topics configured, with their robots' topics and live values.
func (web *Web) telemetryGetHandler(w http.ResponseWriter, r *http.Request) {
	writeJson(w, http.StatusOK, web.field.Telemetry())
}

// Replaces what a team records from {"topics": [...]}, a list of topic names, sections and exclusions (see
// field/telemetry.go), and responds with the telemetry listing. Responds 400 for a bad team number or list.
func (web *Web) teamTopicsPutHandler(w http.ResponseWriter, r *http.Request) {
	teamId, err := strconv.Atoi(r.PathValue("teamId"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var request struct {
		Topics []string `json:"topics"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := web.field.SetTeamTopics(teamId, request.Topics); err != nil {
		writeFieldError(w, err)
		return
	}
	web.telemetryGetHandler(w, r)
}

// Looks up a team's stored WPA key so the UI can prefill it. Responds 404 if the team isn't in the database.
func (web *Web) teamGetHandler(w http.ResponseWriter, r *http.Request) {
	teamId, err := strconv.Atoi(r.PathValue("teamId"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	wpaKey, ok, err := web.field.TeamWpaKey(teamId)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("team %d not found", teamId))
		return
	}
	writeJson(w, http.StatusOK, field.Assignment{TeamId: teamId, WpaKey: wpaKey})
}

func (web *Web) settingsGetHandler(w http.ResponseWriter, r *http.Request) {
	writeJson(w, http.StatusOK, web.field.Settings())
}

func (web *Web) settingsPutHandler(w http.ResponseWriter, r *http.Request) {
	var settings model.EventSettings
	if err := json.NewDecoder(r.Body).Decode(&settings); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := web.field.UpdateSettings(settings); err != nil {
		writeFieldError(w, err)
		return
	}
	web.settingsGetHandler(w, r)
}

// Installs the available update and restarts into it. The install continues in the background and shows in the status
// this responds with. Responds 409 while updating isn't safe (see updateBlocker), when there's nothing to install, or
// when an install is already running.
func (web *Web) updatePostHandler(w http.ResponseWriter, r *http.Request) {
	if err := web.updateBlocker(); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	if err := web.updater.Install(web.updateBlocker); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	web.statusHandler(w, r)
}

// Restarting drops every driver station and comes back with FMS off, and would cut a recording short, so updates wait
// until both are off. Checked when an update starts and again right before the restart.
func (web *Web) updateBlocker() error {
	status := web.field.Status()
	if status.DriverStationMode != field.DriverStationsOff {
		return errors.New("turn FMS off to update")
	}
	if status.Recording.Active {
		return errors.New("stop recording to update")
	}
	return nil
}

func writeJson(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("Failed to write HTTP response: %v", err)
	}
}

// Responds 400 for a field.ValidationError (rejected before anything was saved) and 500 for anything else.
func writeFieldError(w http.ResponseWriter, err error) {
	var validationErr field.ValidationError
	if errors.As(err, &validationErr) {
		writeError(w, http.StatusBadRequest, err)
	} else {
		writeError(w, http.StatusInternalServerError, err)
	}
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJson(w, status, map[string]string{"error": err.Error()})
}
