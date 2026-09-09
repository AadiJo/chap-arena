// Copyright 2014 Team 254. All Rights Reserved.
// Author: pat@patfairbank.com (Patrick Fairbank)
//
// Model and datastore read/write methods for the single configuration record that Chap Arena keeps:
// the addresses and credentials of the field hardware, the WPA key shared by every team radio, and
// the team currently assigned to each of the six alliance stations.

package model

const (
	// Bounds imposed by WPA2 on the pre-shared key.
	MinWpaKeyLength = 8
	MaxWpaKeyLength = 63
)

type EventSettings struct {
	Id   int `db:"id"`
	Name string

	// Team radio: a Vivid-Hosting VH-113 access point running OpenWRT.
	RadioEnabled bool
	ApAddress    string
	ApPassword   string
	ApChannel    int

	// The WPA key handed to every team radio that doesn't override it. Cheesy Arena generates a
	// distinct key per team; at an at-home field a single shared key is far easier to distribute.
	TeamWpaKey string

	// Team ethernet: a Cisco Catalyst 3500-series switch reached over Telnet.
	SwitchEnabled  bool
	SwitchAddress  string
	SwitchPassword string

	// The team assigned to each alliance station. Zero means the station is empty, which is treated
	// the same way a bypassed station is: no SSID and no VLAN are configured for it.
	Red1TeamId  int
	Red2TeamId  int
	Red3TeamId  int
	Blue1TeamId int
	Blue2TeamId int
	Blue3TeamId int

	// Per-station override of TeamWpaKey, for a team whose radio is already flashed with a key of
	// its own. Blank means the station uses the shared key.
	Red1WpaKey  string
	Red2WpaKey  string
	Red3WpaKey  string
	Blue1WpaKey string
	Blue2WpaKey string
	Blue3WpaKey string

	// Guards the web interface. Blank disables authentication entirely.
	AdminPassword string
}

// Returns the six station assignments in alliance station order, matching the ordering that the
// access point and switch configuration methods expect.
func (settings *EventSettings) StationTeamIds() [6]int {
	return [6]int{
		settings.Red1TeamId,
		settings.Red2TeamId,
		settings.Red3TeamId,
		settings.Blue1TeamId,
		settings.Blue2TeamId,
		settings.Blue3TeamId,
	}
}

// Overwrites the six station assignments from an array in alliance station order.
func (settings *EventSettings) SetStationTeamIds(teamIds [6]int) {
	settings.Red1TeamId = teamIds[0]
	settings.Red2TeamId = teamIds[1]
	settings.Red3TeamId = teamIds[2]
	settings.Blue1TeamId = teamIds[3]
	settings.Blue2TeamId = teamIds[4]
	settings.Blue3TeamId = teamIds[5]
}

// Returns the six per-station WPA key overrides in alliance station order. A blank entry means the
// station falls back to the shared key.
func (settings *EventSettings) StationWpaKeys() [6]string {
	return [6]string{
		settings.Red1WpaKey,
		settings.Red2WpaKey,
		settings.Red3WpaKey,
		settings.Blue1WpaKey,
		settings.Blue2WpaKey,
		settings.Blue3WpaKey,
	}
}

// Overwrites the six per-station WPA key overrides from an array in alliance station order.
func (settings *EventSettings) SetStationWpaKeys(wpaKeys [6]string) {
	settings.Red1WpaKey = wpaKeys[0]
	settings.Red2WpaKey = wpaKeys[1]
	settings.Red3WpaKey = wpaKeys[2]
	settings.Blue1WpaKey = wpaKeys[3]
	settings.Blue2WpaKey = wpaKeys[4]
	settings.Blue3WpaKey = wpaKeys[5]
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
		Name:          "Untitled Event",
		RadioEnabled:  true,
		ApAddress:     "10.0.100.2",
		ApChannel:     36,
		SwitchEnabled: true,
		SwitchAddress: "10.0.100.2",
	}

	if err := database.eventSettingsTable.create(&eventSettings); err != nil {
		return nil, err
	}
	return &eventSettings, nil
}

func (database *Database) UpdateEventSettings(eventSettings *EventSettings) error {
	return database.eventSettingsTable.update(eventSettings)
}
