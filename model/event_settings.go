// Copyright 2014 Team 254. All Rights Reserved.
// Author: pat@patfairbank.com (Patrick Fairbank)
//
// Model and datastore read/write methods for event-level configuration.

package model

import "strings"

// Configured here to avoid circular import dependencies.
var (
	sccDefaultUpCommands = []string{
		"configure terminal",
		"interface range gigabitEthernet 1/2-4",
		"no shutdown",
		"exit",
		"exit",
		"exit",
	}
	sccDefaultDownCommands = []string{
		"configure terminal",
		"interface range gigabitEthernet 1/2-4",
		"shutdown",
		"exit",
		"exit",
		"exit",
	}
)

// EventSettings holds the network settings shared with full Cheesy Arena, plus the current station assignment.
// Fields full Cheesy Arena stores that aren't listed here are kept intact on update by the table layer.
type EventSettings struct {
	Id                   int `db:"id"`
	ApAddress            string
	ApPassword           string
	ApChannel            int
	SwitchAddress        string
	SwitchPassword       string
	SCCManagementEnabled bool
	RedSCCAddress        string
	BlueSCCAddress       string
	SCCUsername          string
	SCCPassword          string
	SCCUpCommands        string
	SCCDownCommands      string

	// Team IDs last applied to R1, R2, R3, B1, B2, B3 (0 means empty). Restored on startup so a restart doesn't wipe
	// the access point.
	StationTeamIds [6]int
}

func (database *Database) GetEventSettings() (*EventSettings, error) {
	allEventSettings, err := database.eventSettingsTable.getAll()
	if err != nil {
		return nil, err
	}
	if len(allEventSettings) == 1 {
		eventSettings := allEventSettings[0]
		return &eventSettings, nil
	}

	// Database record doesn't exist yet; create it now.
	eventSettings := EventSettings{
		ApChannel:       36,
		SCCUpCommands:   strings.Join(sccDefaultUpCommands, "\n"),
		SCCDownCommands: strings.Join(sccDefaultDownCommands, "\n"),
	}

	if err := database.eventSettingsTable.create(&eventSettings); err != nil {
		return nil, err
	}
	return &eventSettings, nil
}

func (database *Database) UpdateEventSettings(eventSettings *EventSettings) error {
	return database.eventSettingsTable.update(eventSettings)
}
