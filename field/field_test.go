// Copyright 2026 Advait Johari. All Rights Reserved.

package field

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestStationTeamsTreatsEmptyStationsAsBypassed(t *testing.T) {
	field := SetupTestField(t)
	field.Settings.TeamWpaKey = "chapsrule"

	teams := field.stationTeams([6]int{254, 0, 1678, 0, 0, 604})

	// Empty stations stay nil so the access point and switch skip them entirely.
	assert.Nil(t, teams[1])
	assert.Nil(t, teams[3])
	assert.Nil(t, teams[4])

	// Occupied stations all carry the one shared key.
	for _, i := range []int{0, 2, 5} {
		if assert.NotNil(t, teams[i]) {
			assert.Equal(t, "chapsrule", teams[i].WpaKey)
		}
	}
	assert.Equal(t, 254, teams[0].Id)
	assert.Equal(t, 1678, teams[2].Id)
	assert.Equal(t, 604, teams[5].Id)
}

func TestValidate(t *testing.T) {
	field := SetupTestField(t)
	field.Settings.RadioEnabled = true
	field.Settings.TeamWpaKey = "chapsrule"

	assert.Nil(t, field.validate([6]int{254, 0, 1678, 0, 0, 604}))

	err := field.validate([6]int{25600, 0, 0, 0, 0, 0})
	assert.ErrorContains(t, err, "Red 1: team 25600 is outside the range 1-25599")

	err = field.validate([6]int{254, 0, 0, 254, 0, 0})
	assert.ErrorContains(t, err, "team 254 is assigned to more than one station")

	// The key only has to be valid once there is a radio to configure.
	field.Settings.TeamWpaKey = "short"
	assert.ErrorContains(t, field.validate([6]int{254, 0, 0, 0, 0, 0}), "must be 8-63 characters")
	assert.Nil(t, field.validate([6]int{}))

	// A blank key is fine when the radio isn't being configured at all.
	field.Settings.RadioEnabled = false
	field.Settings.TeamWpaKey = ""
	assert.Nil(t, field.validate([6]int{254, 0, 0, 0, 0, 0}))
}

func TestApplyPersistsStationAssignment(t *testing.T) {
	field := SetupTestField(t)

	assert.Nil(t, field.Apply([6]int{254, 0, 1678, 0, 0, 604}))
	assert.Equal(t, [6]int{254, 0, 1678, 0, 0, 604}, field.Settings.StationTeamIds())

	// A restart reads the assignment back out of the database.
	assert.Nil(t, field.LoadSettings())
	assert.Equal(t, [6]int{254, 0, 1678, 0, 0, 604}, field.Settings.StationTeamIds())
}

func TestApplyRejectsInvalidAssignmentWithoutPersisting(t *testing.T) {
	field := SetupTestField(t)

	assert.ErrorContains(t, field.Apply([6]int{0, 0, 0, 0, 0, 99999}), "outside the range")
	assert.Nil(t, field.LoadSettings())
	assert.Equal(t, [6]int{}, field.Settings.StationTeamIds())
}
