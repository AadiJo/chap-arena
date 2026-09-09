// Copyright 2014 Team 254. All Rights Reserved.
// Author: pat@patfairbank.com (Patrick Fairbank)

package main

import (
	"flag"
	"github.com/AadiJo/chap-arena/field"
	"github.com/AadiJo/chap-arena/model"
	"github.com/AadiJo/chap-arena/web"
	"log"
)

// Main entry point for the application.
func main() {
	httpPort := flag.Int("port", 8080, "Port to serve the web interface on")
	dbPath := flag.String("db", "", "Database file to open, overriding the one recorded in "+model.BootstrapFileName)
	flag.Parse()

	if *dbPath == "" {
		bootstrap, err := model.LoadBootstrap()
		if err != nil {
			log.Fatalln("Error reading "+model.BootstrapFileName+": ", err)
		}
		*dbPath = bootstrap.DatabasePath
	}

	arenaField, err := field.NewField(*dbPath)
	if err != nil {
		log.Fatalln("Error during startup: ", err)
	}
	log.Printf("Using database %s", arenaField.DatabasePath())

	// Start monitoring the access point and re-apply the last saved station assignment.
	arenaField.Run()

	web.NewWeb(arenaField).ServeWebInterface(*httpPort)
}
