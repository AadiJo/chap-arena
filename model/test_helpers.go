// Copyright 2017 Team 254. All Rights Reserved.
// Author: pat@patfairbank.com (Patrick Fairbank)
//
// Helper methods for use in tests in this package and others.

package model

import (
	"github.com/stretchr/testify/assert"
	"path/filepath"
	"testing"
)

func SetupTestDb(t *testing.T) *Database {
	database, err := OpenDatabase(filepath.Join(t.TempDir(), "test.db"))
	assert.Nil(t, err)
	t.Cleanup(
		func() {
			database.Close()
		},
	)
	return database
}
