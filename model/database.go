// Copyright 2014 Team 254. All Rights Reserved.
// Author: pat@patfairbank.com (Patrick Fairbank)
//
// Functions for manipulating the per-event Bolt datastore.
//
// Only the tables this app reads are registered. Buckets that full Cheesy Arena created (matches, rankings, etc.) are
// left untouched, so an event.db copied from a real event keeps working in both apps.

package model

import (
	"go.etcd.io/bbolt"
	"time"
)

type Database struct {
	Path               string
	bolt               *bbolt.DB
	eventSettingsTable *table[EventSettings]
	teamTable          *table[Team]
}

// Opens the Bolt database at the given path, creating it if it doesn't exist.
func OpenDatabase(filename string) (*Database, error) {
	database := Database{Path: filename}
	var err error
	database.bolt, err = bbolt.Open(database.Path, 0644, &bbolt.Options{NoSync: true, Timeout: time.Second})
	if err != nil {
		return nil, err
	}

	// Register tables.
	if database.eventSettingsTable, err = newTable[EventSettings](&database); err != nil {
		return nil, err
	}
	if database.teamTable, err = newTable[Team](&database); err != nil {
		return nil, err
	}

	return &database, nil
}

func (database *Database) Close() error {
	return database.bolt.Close()
}
