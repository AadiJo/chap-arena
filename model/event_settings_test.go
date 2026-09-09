// Copyright 2014 Team 254. All Rights Reserved.
// Author: pat@patfairbank.com (Patrick Fairbank)

package model

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestEventSettingsReadWrite(t *testing.T) {
	db := setupTestDb(t)
	defer db.Close()

	eventSettings, err := db.GetEventSettings()
	assert.Nil(t, err)
	assert.Equal(
		t,
		EventSettings{
			Id:            1,
			Name:          "Untitled Event",
			RadioEnabled:  true,
			ApAddress:     "10.0.100.2",
			ApChannel:     36,
			SwitchEnabled: true,
			SwitchAddress: "10.0.100.2",
		},
		*eventSettings,
	)

	eventSettings.Name = "Chap Practice Field"
	eventSettings.TeamWpaKey = "chapsrule"
	eventSettings.Red1TeamId = 254
	eventSettings.Blue3TeamId = 1678
	err = db.UpdateEventSettings(eventSettings)
	assert.Nil(t, err)
	eventSettings2, err := db.GetEventSettings()
	assert.Nil(t, err)
	assert.Equal(t, eventSettings, eventSettings2)
}

func TestEventSettingsStationTeamIds(t *testing.T) {
	var eventSettings EventSettings
	eventSettings.SetStationTeamIds([6]int{254, 0, 1678, 971, 0, 604})
	assert.Equal(t, 254, eventSettings.Red1TeamId)
	assert.Equal(t, 0, eventSettings.Red2TeamId)
	assert.Equal(t, 1678, eventSettings.Red3TeamId)
	assert.Equal(t, 971, eventSettings.Blue1TeamId)
	assert.Equal(t, 0, eventSettings.Blue2TeamId)
	assert.Equal(t, 604, eventSettings.Blue3TeamId)
	assert.Equal(t, [6]int{254, 0, 1678, 971, 0, 604}, eventSettings.StationTeamIds())
}
