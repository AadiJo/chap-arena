// Copyright 2014 Team 254. All Rights Reserved.
// Author: pat@patfairbank.com (Patrick Fairbank)

package main

import (
	"flag"
	"github.com/Team254/cheesy-arena/field"
	"github.com/Team254/cheesy-arena/model"
	"github.com/Team254/cheesy-arena/web"
	"io"
	"log"
	"os"
	"path/filepath"
)

const logTailLines = 200

// Main entry point for the application. event.db and cheesy-arena.log live next to the executable unless -db is given,
// so an event.db copied from full Cheesy Arena can be dropped in beside the binary.
func main() {
	dbPath := flag.String("db", "", "Path to the event database (default: event.db next to the executable)")
	port := flag.Int("port", 8080, "HTTP port for the web UI")
	flag.Parse()
	if *dbPath == "" {
		*dbPath = filepath.Join(executableDir(), "event.db")
	}

	// Log to the console, a file that survives restarts, and the tail shown in the UI.
	logPath := filepath.Join(filepath.Dir(*dbPath), "cheesy-arena.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatalln("Error opening log file: ", err)
	}
	logTail := web.NewLogTail(logTailLines)
	log.SetOutput(io.MultiWriter(os.Stderr, logFile, logTail))
	log.Printf("Using database %s, logging to %s", *dbPath, logPath)

	database, err := model.OpenDatabase(*dbPath)
	if err != nil {
		log.Fatalln("Error opening database: ", err)
	}
	field, err := field.New(database)
	if err != nil {
		log.Fatalln("Error during startup: ", err)
	}

	go web.NewWeb(field, logTail).ServeWebInterface(*port)

	// Run the access point monitoring loop in the main thread.
	field.Run()
}

func executableDir() string {
	executable, err := os.Executable()
	if err != nil {
		log.Fatalln("Error locating executable: ", err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		log.Fatalln("Error locating executable: ", err)
	}
	return filepath.Dir(executable)
}
