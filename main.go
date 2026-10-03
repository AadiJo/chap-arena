// Copyright 2014 Team 254. All Rights Reserved.
// Author: pat@patfairbank.com (Patrick Fairbank)

package main

import (
	"flag"
	"fmt"
	"github.com/Team254/cheesy-arena/field"
	"github.com/Team254/cheesy-arena/model"
	"github.com/Team254/cheesy-arena/nt"
	"github.com/Team254/cheesy-arena/web"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

const logTailLines = 200

// Main entry point for the application. event.db, cheesy-arena.log and the recordings folder live next to the executable
// unless -db is given, in which case they go next to the database. An event.db copied from full Cheesy Arena can be
// dropped in beside the binary.
func main() {
	dbPath := flag.String("db", "", "Path to the event database (default: event.db next to the executable)")
	port := flag.Int("port", 8080, "HTTP port for the web UI")
	noBrowser := flag.Bool("no-browser", false, "Don't open the web UI in a browser on startup")
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
	field, err := field.New(
		database, field.Options{
			DriverStationPorts: field.DefaultDriverStationPorts,
			NtAddress:          nt.RobotAddress,
			RecordingsDir:      filepath.Join(filepath.Dir(*dbPath), "recordings"),
		},
	)
	if err != nil {
		log.Fatalln("Error during startup: ", err)
	}

	// Bind the port before opening the browser so the page never loads ahead of the server.
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalln("Error starting web server: ", err)
	}
	go web.NewWeb(field, logTail).Serve(listener)
	url := fmt.Sprintf("http://localhost:%d/", *port)
	log.Printf("Web UI at %s", url)
	if !*noBrowser {
		openBrowser(url)
	}

	// Run the access point monitoring loop in the main thread.
	field.Run()
}

// Opens url in the default browser. Failure (e.g. a headless machine) is logged, not fatal; the URL is in the log.
func openBrowser(url string) {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		command = exec.Command("open", url)
	default:
		command = exec.Command("xdg-open", url)
	}
	if err := command.Start(); err != nil {
		log.Printf("Couldn't open a browser (%v); open %s manually.", err, url)
		return
	}
	go command.Wait()
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
