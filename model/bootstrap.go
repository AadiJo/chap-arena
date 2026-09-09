// Copyright 2026 Advait Johari. All Rights Reserved.
//
// Records which database file to open. This can't be stored in the database itself, since it's the
// thing that says which database to read, so it lives in a small JSON file alongside it.

package model

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	BootstrapFileName   = "chap-arena.json"
	DefaultDatabasePath = "event.db"
)

// Directory holding the bootstrap file. Mutable so that tests don't write one into the repo.
var BootstrapDir = "."

type Bootstrap struct {
	DatabasePath string `json:"databasePath"`
}

// Returns the path of the bootstrap file, which sits in the working directory next to the binary.
func BootstrapPath() string {
	return filepath.Join(BootstrapDir, BootstrapFileName)
}

// Reads the bootstrap file. A missing file is not an error; it yields the default database path, so
// that a fresh install works with no configuration.
func LoadBootstrap() (Bootstrap, error) {
	bootstrap := Bootstrap{DatabasePath: DefaultDatabasePath}

	contents, err := os.ReadFile(BootstrapPath())
	if errors.Is(err, fs.ErrNotExist) {
		return bootstrap, nil
	}
	if err != nil {
		return bootstrap, err
	}
	if err = json.Unmarshal(contents, &bootstrap); err != nil {
		return bootstrap, err
	}
	if bootstrap.DatabasePath == "" {
		bootstrap.DatabasePath = DefaultDatabasePath
	}
	return bootstrap, nil
}

// Writes the bootstrap file.
func SaveBootstrap(bootstrap Bootstrap) error {
	contents, err := json.MarshalIndent(bootstrap, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(BootstrapPath(), append(contents, '\n'), 0644)
}
