// Copyright 2014 Team 254. All Rights Reserved.
// Author: pat@patfairbank.com (Patrick Fairbank)
//
// Methods for configuring a Cisco Switch 3500-series switch for team VLANs.

package network

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"github.com/Team254/cheesy-arena/model"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
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

// Reads the switch's running config and returns which stations' VLANs don't match the given teams, which are the ones
// ConfigureTeamEthernet needs to rewrite. It's read every time rather than remembered because the switch only keeps its
// startup config across a reboot, so a switch that power-cycled has lost whatever was applied since it was last saved.
func (sw *Switch) StationsToConfigure(teams [6]*model.Team) ([6]bool, error) {
	sw.mutex.Lock()
	defer sw.mutex.Unlock()
	var stations [6]bool
	output, err := sw.runCommand("show running-config\n")
	if err != nil {
		sw.Status = "ERROR"
		return stations, err
	}
	current, ok := parseTeamVlans(output)
	if !ok {
		sw.Status = "ERROR"
		return stations, errors.New("its running config has no team VLAN interfaces; check the switch password")
	}
	for i, team := range teams {
		stations[i] = !teamVlanFor(team).matches(current[i])
	}
	if stations == [6]bool{} {
		sw.Status = "ACTIVE"
	}
	return stations, nil
}

// Sets up wired networks for the given set of teams, rewriting only the VLANs of the given stations (see
// StationsToConfigure), so driver stations at the other stations keep their gateway. Does nothing if no station is
// given.
func (sw *Switch) ConfigureTeamEthernet(teams [6]*model.Team, stations [6]bool) error {
	// Make sure multiple configurations aren't being set at the same time.
	sw.mutex.Lock()
	defer sw.mutex.Unlock()
	if stations == [6]bool{} {
		return nil
	}
	sw.Status = "CONFIGURING"

	// Remove the stations' old team VLANs.
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

	// Create the new team VLANs.
	addTeamVlansCommand := ""
	for i, vlan := range teamVlans {
		team := teams[i]
		if !stations[i] || team == nil {
			continue
		}
		teamPartialIp := fmt.Sprintf("10.%d.%d", team.Id/100, team.Id%100)
		config := teamVlanFor(team)
		addTeamVlansCommand += fmt.Sprintf(
			"ip dhcp excluded-address %[1]s.1 %[1]s.19\nip dhcp excluded-address %[1]s.200 %[1]s.254\n"+
				"ip dhcp pool dhcp%[2]d\n%[3]s\ninterface Vlan%[2]d\nip address %[4]s\n",
			teamPartialIp, vlan, strings.Join(config.poolLines, "\n"), config.gateway,
		)
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

	sw.Status = "ACTIVE"
	return nil
}

// A team VLAN as the switch's running config has it: the address on its interface, e.g. "10.2.54.4 255.255.255.0" (""
// for none), and the lines of its DHCP pool (nil for no pool). The DHCP excluded addresses aren't included; they're
// global, and adding them again is harmless.
type teamVlanConfig struct {
	gateway   string
	poolLines []string
}

// What a station's VLAN should be for the given team, or for an empty station if it's nil. The pool also hands out a
// route to the FMS subnet (10.0.100.0/24) through the team's gateway, as DHCP options 121 and 249 (Microsoft's older code
// for the same thing). A driver station also on Wi-Fi would otherwise send FMS traffic out its Wi-Fi default route; the
// more specific route wins over any default. The value is the prefix length (0x18 = 24), the prefix's significant
// octets (10.0.100) and then the gateway.
func teamVlanFor(team *model.Team) teamVlanConfig {
	if team == nil {
		return teamVlanConfig{}
	}
	gateway := fmt.Sprintf("10.%d.%d.%d", team.Id/100, team.Id%100, switchTeamGatewayAddress)
	fmsRoute := fmt.Sprintf("180a.0064.0a%02x.%02x%02x", team.Id/100, team.Id%100, switchTeamGatewayAddress)
	return teamVlanConfig{
		gateway: gateway + " 255.255.255.0",
		poolLines: []string{
			fmt.Sprintf("network 10.%d.%d.0 255.255.255.0", team.Id/100, team.Id%100),
			"default-router " + gateway,
			"option 121 hex " + fmsRoute,
			"option 249 hex " + fmsRoute,
			"lease 7",
		},
	}
}

// Whether the switch's config for a VLAN is the same as config, ignoring the order and case of the pool's lines.
func (config teamVlanConfig) matches(current teamVlanConfig) bool {
	if config.gateway != current.gateway || (config.poolLines == nil) != (current.poolLines == nil) {
		return false
	}
	normalize := func(lines []string) []string {
		normalized := make([]string, len(lines))
		for i, line := range lines {
			normalized[i] = strings.ToLower(line)
		}
		slices.Sort(normalized)
		return normalized
	}
	return slices.Equal(normalize(config.poolLines), normalize(current.poolLines))
}

var (
	vlanInterfaceRe = regexp.MustCompile(`^interface Vlan(\d+)$`)
	dhcpPoolRe      = regexp.MustCompile(`^ip dhcp pool dhcp(\d+)$`)
)

// Reads each station's VLAN from the output of "show running-config", which IOS prints with a section's lines indented
// under it:
//
//	ip dhcp pool dhcp20
//	   network 10.26.87.0 255.255.255.0
//	   default-router 10.26.87.4
//	interface Vlan20
//	 ip address 10.26.87.4 255.255.255.0
//
// ok is false if any team VLAN's interface is missing, which means the output isn't this switch's running config (for
// example because logging in failed).
func parseTeamVlans(output string) (vlans [6]teamVlanConfig, ok bool) {
	station := map[string]int{}
	for i, vlan := range teamVlans {
		station[strconv.Itoa(vlan)] = i
	}
	var found [6]bool
	current, inPool := -1, false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r ")
		if !strings.HasPrefix(line, " ") {
			current = -1
			if match := vlanInterfaceRe.FindStringSubmatch(line); match != nil {
				if i, isTeamVlan := station[match[1]]; isTeamVlan {
					current, inPool, found[i] = i, false, true
				}
			} else if match := dhcpPoolRe.FindStringSubmatch(line); match != nil {
				if i, isTeamVlan := station[match[1]]; isTeamVlan {
					current, inPool = i, true
					vlans[i].poolLines = []string{}
				}
			}
			continue
		}
		if current < 0 {
			continue
		}
		line = strings.TrimSpace(line)
		if inPool {
			vlans[current].poolLines = append(vlans[current].poolLines, line)
		} else if gateway, isAddress := strings.CutPrefix(line, "ip address "); isAddress {
			vlans[current].gateway = gateway
		}
	}
	return vlans, found == [6]bool{true, true, true, true, true, true}
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
