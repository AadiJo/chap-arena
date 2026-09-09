// Copyright 2026 Advait Johari. All Rights Reserved.

package model

import (
	"github.com/stretchr/testify/assert"
	"os"
	"path/filepath"
	"testing"
)

func TestBootstrapDefaultsWhenMissing(t *testing.T) {
	BootstrapDir = t.TempDir()

	bootstrap, err := LoadBootstrap()
	assert.Nil(t, err)
	assert.Equal(t, DefaultDatabasePath, bootstrap.DatabasePath)
}

func TestBootstrapReadWrite(t *testing.T) {
	BootstrapDir = t.TempDir()

	assert.Nil(t, SaveBootstrap(Bootstrap{DatabasePath: "/srv/field/event.db"}))
	bootstrap, err := LoadBootstrap()
	assert.Nil(t, err)
	assert.Equal(t, "/srv/field/event.db", bootstrap.DatabasePath)

	// A file that exists but names no database falls back to the default rather than an empty path.
	assert.Nil(t, os.WriteFile(filepath.Join(BootstrapDir, BootstrapFileName), []byte(`{}`), 0644))
	bootstrap, err = LoadBootstrap()
	assert.Nil(t, err)
	assert.Equal(t, DefaultDatabasePath, bootstrap.DatabasePath)
}
