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
}

func NewWeb(field *field.Field, logTail *LogTail) *Web {
	return &Web{field: field, logTail: logTail}
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
	mux.HandleFunc("GET /api/teams/{teamId}", web.teamGetHandler)
	mux.HandleFunc("GET /api/settings", web.settingsGetHandler)
	mux.HandleFunc("PUT /api/settings", web.settingsPutHandler)
	return mux
}

type statusResponse struct {
	field.Status
	Log []string `json:"log"`
}

func (web *Web) statusHandler(w http.ResponseWriter, r *http.Request) {
	writeJson(w, http.StatusOK, statusResponse{Status: web.field.Status(), Log: web.logTail.Lines()})
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
