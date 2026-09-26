// Copyright 2014 Team 254. All Rights Reserved.
// Author: pat@patfairbank.com (Patrick Fairbank)
//
// Model and datastore CRUD methods for a team at an event.

package model

// Team holds the only team fields this app uses. Other fields stored by full Cheesy Arena (name, city, etc.) are kept
// intact on update by the table layer.
type Team struct {
	Id     int `db:"id,manual"`
	WpaKey string
}

func (database *Database) CreateTeam(team *Team) error {
	return database.teamTable.create(team)
}

func (database *Database) GetTeamById(id int) (*Team, error) {
	return database.teamTable.getById(id)
}

func (database *Database) UpdateTeam(team *Team) error {
	return database.teamTable.update(team)
}
