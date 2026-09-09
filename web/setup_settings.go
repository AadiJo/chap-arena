// Copyright 2014 Team 254. All Rights Reserved.
// Author: pat@patfairbank.com (Patrick Fairbank)
//
// Web routes for the settings page: field hardware addresses, credentials, and the shared team WPA
// key.

package web

import (
	"fmt"
	"github.com/AadiJo/chap-arena/model"
	"net/http"
	"strconv"
)

type settingsPageView struct {
	pageView
	ErrorMessage  string
	StatusMessage string
}

// Shows the settings page.
func (web *Web) settingsGetHandler(w http.ResponseWriter, r *http.Request) {
	if !web.userIsAdmin(w, r) {
		return
	}
	web.renderSettings(w, "", "")
}

// Saves the settings and reconnects to the field hardware at the new addresses.
func (web *Web) settingsPostHandler(w http.ResponseWriter, r *http.Request) {
	if !web.userIsAdmin(w, r) {
		return
	}

	// Mutate a copy so that a rejected submission leaves the live settings untouched.
	settings := *web.field.Settings
	settings.Name = r.PostFormValue("name")
	settings.RadioEnabled = r.PostFormValue("radioEnabled") == "on"
	settings.ApAddress = r.PostFormValue("apAddress")
	settings.ApPassword = r.PostFormValue("apPassword")
	settings.SwitchEnabled = r.PostFormValue("switchEnabled") == "on"
	settings.SwitchAddress = r.PostFormValue("switchAddress")
	settings.SwitchPassword = r.PostFormValue("switchPassword")
	settings.AdminPassword = r.PostFormValue("adminPassword")

	channel, err := strconv.Atoi(r.PostFormValue("apChannel"))
	if err != nil {
		web.renderSettings(w, fmt.Sprintf("Radio channel %q is not a number.", r.PostFormValue("apChannel")), "")
		return
	}
	settings.ApChannel = channel

	// The WPA key is allowed to be blank so that the page can be saved before it's chosen, but any
	// other length outside the WPA2 bounds would be silently rejected by the radio.
	wpaKey := r.PostFormValue("teamWpaKey")
	if wpaKey != "" && (len(wpaKey) < model.MinWpaKeyLength || len(wpaKey) > model.MaxWpaKeyLength) {
		web.renderSettings(
			w,
			fmt.Sprintf(
				"The team WPA key must be %d-%d characters; got %d.",
				model.MinWpaKeyLength,
				model.MaxWpaKeyLength,
				len(wpaKey),
			),
			"",
		)
		return
	}
	settings.TeamWpaKey = wpaKey

	if err := web.field.Database.UpdateEventSettings(&settings); err != nil {
		handleWebErr(w, err)
		return
	}
	// Pick up the new addresses and credentials without restarting.
	if err := web.field.LoadSettings(); err != nil {
		handleWebErr(w, err)
		return
	}

	web.renderSettings(w, "", "Settings saved.")
}

func (web *Web) renderSettings(w http.ResponseWriter, errorMessage, statusMessage string) {
	template, err := web.parseFiles("templates/setup_settings.html", "templates/base.html")
	if err != nil {
		handleWebErr(w, err)
		return
	}

	data := settingsPageView{
		pageView:      pageView{EventSettings: web.field.Settings, Page: "settings"},
		ErrorMessage:  errorMessage,
		StatusMessage: statusMessage,
	}
	if err = template.ExecuteTemplate(w, "base", data); err != nil {
		handleWebErr(w, err)
	}
}
