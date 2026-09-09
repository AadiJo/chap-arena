// Copyright 2014 Team 254. All Rights Reserved.
// Author: pat@patfairbank.com (Patrick Fairbank)

package main

import (
	"flag"
	"github.com/AadiJo/chap-arena/field"
	"github.com/AadiJo/chap-arena/web"
	"log"
)

const eventDbPath = "./event.db"

// Main entry point for the application.
func main() {
	httpPort := flag.Int("port", 8080, "Port to serve the web interface on")
	flag.Parse()

	arenaField, err := field.NewField(eventDbPath)
	if err != nil {
		log.Fatalln("Error during startup: ", err)
	}

	// Start monitoring the access point and re-apply the last saved station assignment.
	arenaField.Run()

	web.NewWeb(arenaField).ServeWebInterface(*httpPort)
}
