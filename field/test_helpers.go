// Copyright 2026 Advait Johari. All Rights Reserved.
//
// Helper methods for use in tests in this package and others.

package field

import (
	"github.com/AadiJo/chap-arena/model"
	"github.com/stretchr/testify/assert"
	"path/filepath"
	"testing"
)

// Builds a Field backed by a temporary database, with both pieces of hardware disabled so that
// applying a station assignment doesn't try to reach the network.
func SetupTestField(t *testing.T) *Field {
	model.BaseDir = ".."
	dir := t.TempDir()
	model.BootstrapDir = dir
	field, err := NewField(filepath.Join(dir, "test.db"))
	assert.Nil(t, err)
	t.Cleanup(
		func() {
			field.Database.Close()
		},
	)

	field.Settings.RadioEnabled = false
	field.Settings.SwitchEnabled = false
	return field
}
