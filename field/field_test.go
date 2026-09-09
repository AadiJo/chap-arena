// Copyright 2026 Advait Johari. All Rights Reserved.

package field

import (
	"github.com/AadiJo/chap-arena/model"
	"github.com/stretchr/testify/assert"
	"path/filepath"
	"testing"
)

// Builds an assignment array from team numbers alone, with no per-station key overrides.
func assign(teamIds ...int) [6]Assignment {
	var assignments [6]Assignment
	for i, teamId := range teamIds {
		assignments[i] = Assignment{TeamId: teamId}
	}
	return assignments
}

func TestStationTeamsTreatsEmptyStationsAsBypassed(t *testing.T) {
	field := SetupTestField(t)
	field.Settings.TeamWpaKey = "chapsrule"

	teams := field.stationTeams(assign(254, 0, 1678, 0, 0, 604))

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

func TestStationTeamsAppliesPerStationKeyOverride(t *testing.T) {
	field := SetupTestField(t)
	field.Settings.TeamWpaKey = "chapsrule"

	teams := field.stationTeams([6]Assignment{
		{TeamId: 254},
		{TeamId: 1678, WpaKey: "ownkey12345"},
		{},
		{},
		{},
		{},
	})

	assert.Equal(t, "chapsrule", teams[0].WpaKey)
	assert.Equal(t, "ownkey12345", teams[1].WpaKey)
}

func TestValidateAcceptsOverrideWithoutSharedKey(t *testing.T) {
	field := SetupTestField(t)
	field.Settings.RadioEnabled = true
	field.Settings.TeamWpaKey = ""

	// Every occupied station brings its own key, so the shared key is never consulted.
	assert.Nil(t, field.validate([6]Assignment{{TeamId: 254, WpaKey: "ownkey12345"}}))

	// Adding a station that relies on the shared key makes the missing shared key an error again.
	err := field.validate([6]Assignment{{TeamId: 254, WpaKey: "ownkey12345"}, {TeamId: 1678}})
	assert.ErrorContains(t, err, "must be 8-63 characters")

	// A too-short override is rejected and names the station.
	err = field.validate([6]Assignment{{TeamId: 254, WpaKey: "short"}})
	assert.ErrorContains(t, err, "Red 1: the WPA key must be 8-63 characters")
}

func TestValidate(t *testing.T) {
	field := SetupTestField(t)
	field.Settings.RadioEnabled = true
	field.Settings.TeamWpaKey = "chapsrule"

	assert.Nil(t, field.validate(assign(254, 0, 1678, 0, 0, 604)))

	err := field.validate(assign(25600))
	assert.ErrorContains(t, err, "Red 1: team 25600 is outside the range 1-25599")

	err = field.validate(assign(254, 0, 0, 254))
	assert.ErrorContains(t, err, "team 254 is assigned to more than one station")

	// The key only has to be valid once there is a radio to configure.
	field.Settings.TeamWpaKey = "short"
	assert.ErrorContains(t, field.validate(assign(254)), "must be 8-63 characters")
	assert.Nil(t, field.validate([6]Assignment{}))

	// A blank key is fine when the radio isn't being configured at all.
	field.Settings.RadioEnabled = false
	field.Settings.TeamWpaKey = ""
	assert.Nil(t, field.validate(assign(254)))
}

func TestApplyPersistsStationAssignment(t *testing.T) {
	field := SetupTestField(t)

	assert.Nil(t, field.Apply(assign(254, 0, 1678, 0, 0, 604)))
	assert.Equal(t, [6]int{254, 0, 1678, 0, 0, 604}, field.Settings.StationTeamIds())

	// A restart reads the assignment back out of the database.
	assert.Nil(t, field.LoadSettings())
	assert.Equal(t, [6]int{254, 0, 1678, 0, 0, 604}, field.Settings.StationTeamIds())
}

func TestApplyRejectsInvalidAssignmentWithoutPersisting(t *testing.T) {
	field := SetupTestField(t)

	assert.ErrorContains(t, field.Apply(assign(0, 0, 0, 0, 0, 99999)), "outside the range")
	assert.Nil(t, field.LoadSettings())
	assert.Equal(t, [6]int{}, field.Settings.StationTeamIds())
}

func TestSetDatabasePath(t *testing.T) {
	field := SetupTestField(t)
	original := field.DatabasePath()

	assert.Nil(t, field.Apply(assign(254)))

	// Switching to a new file gets a fresh database, not the previous one's assignment.
	moved := filepath.Join(t.TempDir(), "nested", "moved.db")
	assert.Nil(t, field.SetDatabasePath(moved))
	assert.Equal(t, moved, field.DatabasePath())
	assert.Equal(t, [6]int{}, field.Settings.StationTeamIds())

	// The choice is recorded so that it survives a restart.
	bootstrap, err := model.LoadBootstrap()
	assert.Nil(t, err)
	assert.Equal(t, moved, bootstrap.DatabasePath)

	// Switching back finds the assignment that was saved there.
	assert.Nil(t, field.SetDatabasePath(original))
	assert.Equal(t, [6]int{254, 0, 0, 0, 0, 0}, field.Settings.StationTeamIds())
}

func TestSetDatabasePathRejectsBadPathWithoutSwitching(t *testing.T) {
	field := SetupTestField(t)
	original := field.DatabasePath()

	assert.ErrorContains(t, field.SetDatabasePath("  "), "can't be blank")

	// A path that can't be opened leaves the field on the database it already had, still usable.
	assert.NotNil(t, field.SetDatabasePath(filepath.Join(original, "not-a-directory", "x.db")))
	assert.Equal(t, original, field.DatabasePath())
	assert.Nil(t, field.Apply(assign(971)))
}
