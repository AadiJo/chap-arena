// Copyright 2014 Team 254. All Rights Reserved.
// Author: pat@patfairbank.com (Patrick Fairbank)

package model

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestGetNonexistentTeam(t *testing.T) {
	db := setupTestDb(t)

	team, err := db.GetTeamById(1114)
	assert.Nil(t, err)
	assert.Nil(t, team)
}

func TestTeamCrud(t *testing.T) {
	db := setupTestDb(t)

	team := Team{Id: 254, WpaKey: "12345678"}
	assert.Nil(t, db.CreateTeam(&team))
	team2, err := db.GetTeamById(254)
	assert.Nil(t, err)
	assert.Equal(t, team, *team2)

	team.WpaKey = "87654321"
	assert.Nil(t, db.UpdateTeam(&team))
	team2, err = db.GetTeamById(254)
	assert.Nil(t, err)
	assert.Equal(t, team, *team2)
}
