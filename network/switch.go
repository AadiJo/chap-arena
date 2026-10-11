// Copyright 2014 Team 254. All Rights Reserved.
// Author: pat@patfairbank.com (Patrick Fairbank)
//
// Methods for configuring a Cisco Switch 3500-series switch for team VLANs.

package network

import (
	"bufio"
	"bytes"
	"fmt"
	"github.com/Team254/cheesy-arena/model"
	"net"
	"sync"
	"time"
)

const (
	switchConfigBackoffDurationSec = 5
	switchConfigPauseDurationSec   = 2
	switchTeamGatewayAddress       = 4
	switchTelnetPort               = 23
	switchDialTimeoutSec           = 5
	switchCommandTimeoutSec        = 30
)

const (
	red1Vlan  = 10
	red2Vlan  = 20
	red3Vlan  = 30
	blue1Vlan = 40
	blue2Vlan = 50
	blue3Vlan = 60
)

// VLAN for each station, in station order.
var teamVlans = [6]int{red1Vlan, red2Vlan, red3Vlan, blue1Vlan, blue2Vlan, blue3Vlan}

type Switch struct {
	address               string
	port                  int
	password              string
	mutex                 sync.Mutex
	configBackoffDuration time.Duration
	configPauseDuration   time.Duration
	Status                string

	// Team on each station's VLAN (0 for none) as of the last successful configuration. Until one succeeds, and again
	// after one fails, configured is false and the next configuration rewrites every VLAN.
	configuredTeamIds [6]int
	configured        bool
}

// Overrides for pointing a Switch at a fake in tests. Zero values mean the real switch's Telnet port and waits.
type SwitchOptions struct {
	Port            int
	PauseDuration   time.Duration // Between removing old VLANs and adding new ones.
	BackoffDuration time.Duration // After adding them, before another configuration can start.
}

func NewSwitch(address, password string, options SwitchOptions) *Switch {
	sw := &Switch{
		address:               address,
		port:                  switchTelnetPort,
		password:              password,
		configBackoffDuration: switchConfigBackoffDurationSec * time.Second,
		configPauseDuration:   switchConfigPauseDurationSec * time.Second,
		Status:                "UNKNOWN",
	}
	if options.Port != 0 {
		sw.port = options.Port
	}
	if options.PauseDuration != 0 {
		sw.configPauseDuration = options.PauseDuration
	}
	if options.BackoffDuration != 0 {
		sw.configBackoffDuration = options.BackoffDuration
	}
	return sw
}

// Returns which stations' VLANs ConfigureTeamEthernet would rewrite for the given teams: those whose team changed since
// the last successful configuration, or all six if there hasn't been one.
func (sw *Switch) StationsToConfigure(teams [6]*model.Team) [6]bool {
	sw.mutex.Lock()
	defer sw.mutex.Unlock()
	return sw.stationsToConfigure(teams)
}

// Sets up wired networks for the given set of teams. Only the VLANs of stations returned by StationsToConfigure are
// rewritten, so driver stations at the other stations keep their gateway. Does nothing if no station changed.
func (sw *Switch) ConfigureTeamEthernet(teams [6]*model.Team) error {
	// Make sure multiple configurations aren't being set at the same time.
	sw.mutex.Lock()
	defer sw.mutex.Unlock()
	stations := sw.stationsToConfigure(teams)
	if stations == [6]bool{} {
		return nil
	}
	sw.Status = "CONFIGURING"
	sw.configured = false

	// Remove the changed stations' old team VLANs.
	removeTeamVlansCommand := ""
	for i, vlan := range teamVlans {
		if stations[i] {
			removeTeamVlansCommand += fmt.Sprintf(
				"interface Vlan%d\nno ip address\nno ip dhcp pool dhcp%d\n", vlan, vlan,
			)
		}
	}
	_, err := sw.runConfigCommand(removeTeamVlansCommand)
	if err != nil {
		sw.Status = "ERROR"
		return err
	}
	time.Sleep(sw.configPauseDuration)

	// Create the new team VLANs. Each pool also hands out a route to the FMS subnet (10.0.100.0/24) through the team's
	// gateway, as DHCP options 121 and 249 (Microsoft's older code for the same thing). A driver station also on Wi-Fi
	// would otherwise send FMS traffic out its Wi-Fi default route; the more specific route wins over any default.
	// The value is the prefix length (0x18 = 24), the prefix's significant octets (10.0.100) and then the gateway.
	addTeamVlansCommand := ""
	addTeamVlan := func(team *model.Team, vlan int) {
		if team == nil {
			return
		}
		teamPartialIp := fmt.Sprintf("%d.%d", team.Id/100, team.Id%100)
		fmsRoute := fmt.Sprintf("180a.0064.0a%02x.%02x%02x", team.Id/100, team.Id%100, switchTeamGatewayAddress)
		addTeamVlansCommand += fmt.Sprintf(
			"ip dhcp excluded-address 10.%s.1 10.%s.19\n"+
				"ip dhcp excluded-address 10.%s.200 10.%s.254\n"+
				"ip dhcp pool dhcp%d\n"+
				"network 10.%s.0 255.255.255.0\n"+
				"default-router 10.%s.%d\n"+
				"option 121 hex %s\n"+
				"option 249 hex %s\n"+
				"lease 7\n"+
				"interface Vlan%d\nip address 10.%s.%d 255.255.255.0\n",
			teamPartialIp,
			teamPartialIp,
			teamPartialIp,
			teamPartialIp,
			vlan,
			teamPartialIp,
			teamPartialIp,
			switchTeamGatewayAddress,
			fmsRoute,
			fmsRoute,
			vlan,
			teamPartialIp,
			switchTeamGatewayAddress,
		)
	}
	for i, vlan := range teamVlans {
		if stations[i] {
			addTeamVlan(teams[i], vlan)
		}
	}
	if len(addTeamVlansCommand) > 0 {
		_, err = sw.runConfigCommand(addTeamVlansCommand)
		if err != nil {
			sw.Status = "ERROR"
			return err
		}
	}

	// Give some time for the configuration to take before another one can be attempted.
	time.Sleep(sw.configBackoffDuration)

	sw.configuredTeamIds = teamIds(teams)
	sw.configured = true
	sw.Status = "ACTIVE"
	return nil
}

// Caller must hold the mutex.
func (sw *Switch) stationsToConfigure(teams [6]*model.Team) [6]bool {
	var stations [6]bool
	ids := teamIds(teams)
	for i := range stations {
		stations[i] = !sw.configured || sw.configuredTeamIds[i] != ids[i]
	}
	return stations
}

func teamIds(teams [6]*model.Team) [6]int {
	var ids [6]int
	for i, team := range teams {
		if team != nil {
			ids[i] = team.Id
		}
	}
	return ids
}

// Logs into the switch via Telnet and runs the given command in user exec mode. Reads the output and
// returns it as a string.
func (sw *Switch) runCommand(command string) (string, error) {
	// Open a Telnet connection to the switch.
	conn, err := net.DialTimeout(
		"tcp", fmt.Sprintf("%s:%d", sw.address, sw.port), switchDialTimeoutSec*time.Second,
	)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	// Bound the whole exchange so an unresponsive switch can't block later configurations indefinitely.
	if err = conn.SetDeadline(time.Now().Add(switchCommandTimeoutSec * time.Second)); err != nil {
		return "", err
	}

	// Login to the AP, send the command, and log out all at once.
	writer := bufio.NewWriter(conn)
	_, err = writer.WriteString(
		fmt.Sprintf(
			"%s\nenable\n%s\nterminal length 0\n%sexit\n", sw.password, sw.password,
			command,
		),
	)
	if err != nil {
		return "", err
	}
	err = writer.Flush()
	if err != nil {
		return "", err
	}

	// Read the response.
	var reader bytes.Buffer
	_, err = reader.ReadFrom(conn)
	if err != nil {
		return "", err
	}
	return reader.String(), nil
}

// Logs into the switch via Telnet and runs the given command in global configuration mode. Reads the output
// and returns it as a string.
func (sw *Switch) runConfigCommand(command string) (string, error) {
	return sw.runCommand(fmt.Sprintf("config terminal\n%send\n", command))
}
