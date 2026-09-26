// Copyright 2014 Team 254. All Rights Reserved.
// Author: pat@patfairbank.com (Patrick Fairbank)
//
// Owns the field network hardware (access point, switch, SCC switches) and the team assigned to each driver station.
// It never talks to driver stations, so teams can enable their robots themselves once their radio links.

package field

import (
	"fmt"
	"github.com/Team254/cheesy-arena/model"
	"github.com/Team254/cheesy-arena/network"
	"log"
	"strings"
	"sync"
	"time"
)

// Station order used everywhere: arrays indexed 0-5 map to these names, matching the AP and switch VLAN order.
var StationNames = [6]string{"R1", "R2", "R3", "B1", "B2", "B3"}

const (
	maxTeamId       = 25599 // Highest team number that fits in a 10.TE.AM.x address.
	minWpaKeyLength = 8
	maxWpaKeyLength = 63
)

// ValidationError is returned by Apply when the assignment is rejected before anything is saved.
type ValidationError string

func (err ValidationError) Error() string {
	return string(err)
}

// Assignment is the team and WPA key for one station. A zero TeamId means the station is empty.
type Assignment struct {
	TeamId int    `json:"teamId"`
	WpaKey string `json:"wpaKey"`
}

type StationStatus struct {
	Station    string                 `json:"station"`
	Assignment Assignment             `json:"assignment"`
	Wifi       network.TeamWifiStatus `json:"wifi"`
}

type Status struct {
	Stations          [6]StationStatus `json:"stations"`
	AccessPointStatus string           `json:"accessPointStatus"`
	SwitchStatus      string           `json:"switchStatus"`
	LastApplied       time.Time        `json:"lastApplied"`
}

type Field struct {
	database *model.Database

	// Guards everything below. The access point monitoring loop writes wifiStatuses and hardware Status strings
	// without it, same as full Cheesy Arena.
	mutex         sync.Mutex
	settings      model.EventSettings
	assignments   [6]Assignment
	lastApplied   time.Time
	accessPoint   network.AccessPoint
	networkSwitch *network.Switch
	redSCC        *network.SCCSwitch
	blueSCC       *network.SCCSwitch
	wifiStatuses  [6]network.TeamWifiStatus

	// Serializes switch reconfiguration so the last apply always wins.
	switchMutex sync.Mutex
}

// Loads settings and the last applied station assignment from the database. The access point is told to expect that
// assignment, so a restart leaves linked robots alone unless the AP has drifted.
func New(database *model.Database) (*Field, error) {
	field := Field{database: database}
	settings, err := database.GetEventSettings()
	if err != nil {
		return nil, err
	}
	for i, teamId := range settings.StationTeamIds {
		if teamId == 0 {
			continue
		}
		team, err := database.GetTeamById(teamId)
		if err != nil {
			return nil, err
		}
		field.assignments[i] = Assignment{TeamId: teamId}
		if team != nil {
			field.assignments[i].WpaKey = team.WpaKey
		}
	}
	field.loadSettings(*settings)
	field.accessPoint.SetExpectedTeams(field.teams())
	return &field, nil
}

// Runs the access point monitoring loop forever. It reconfigures the AP whenever it doesn't match the last apply.
func (field *Field) Run() {
	field.accessPoint.Run()
}

func (field *Field) Settings() model.EventSettings {
	field.mutex.Lock()
	defer field.mutex.Unlock()
	return field.settings
}

// Saves the network settings and rebuilds the hardware clients. It does not push anything to the hardware; the next
// apply (or the AP monitoring loop) uses the new settings.
func (field *Field) UpdateSettings(settings model.EventSettings) error {
	field.mutex.Lock()
	defer field.mutex.Unlock()
	settings.Id = field.settings.Id
	settings.StationTeamIds = field.settings.StationTeamIds
	if err := field.database.UpdateEventSettings(&settings); err != nil {
		return err
	}
	field.loadSettings(settings)
	log.Println("Network settings saved.")
	return nil
}

// Returns the stored WPA key for a team, or ok=false if the team isn't in the database.
func (field *Field) TeamWpaKey(teamId int) (string, bool, error) {
	team, err := field.database.GetTeamById(teamId)
	if err != nil || team == nil {
		return "", false, err
	}
	return team.WpaKey, true, nil
}

// Validates and saves the assignment for all six stations, then pushes it to the AP (synchronously) and the switch
// (in the background, since the switch takes several seconds). New WPA keys are saved to each team's database record.
func (field *Field) Apply(assignments [6]Assignment) error {
	if err := validateAssignments(assignments); err != nil {
		return err
	}

	field.mutex.Lock()
	for i := range assignments {
		if assignments[i].TeamId == 0 {
			assignments[i].WpaKey = ""
			continue
		}
		if err := field.saveTeamWpaKey(assignments[i]); err != nil {
			field.mutex.Unlock()
			return err
		}
	}
	for i, assignment := range assignments {
		field.settings.StationTeamIds[i] = assignment.TeamId
	}
	if err := field.database.UpdateEventSettings(&field.settings); err != nil {
		field.mutex.Unlock()
		return err
	}
	field.assignments = assignments
	field.lastApplied = time.Now()
	teams := field.teams()
	field.mutex.Unlock()

	log.Printf("Applying stations %s", describeTeams(teams))
	go field.configureEthernet()
	if err := field.accessPoint.ConfigureTeamWifi(teams); err != nil {
		log.Printf("Failed to configure team WiFi: %v", err)
		return fmt.Errorf("saved, but the access point rejected the configuration: %w", err)
	}
	return nil
}

func (field *Field) Status() Status {
	field.mutex.Lock()
	defer field.mutex.Unlock()
	status := Status{
		AccessPointStatus: field.accessPoint.Status,
		SwitchStatus:      field.networkSwitch.Status,
		LastApplied:       field.lastApplied,
	}
	for i := range status.Stations {
		status.Stations[i] = StationStatus{
			Station:    StationNames[i],
			Assignment: field.assignments[i],
			Wifi:       field.wifiStatuses[i],
		}
	}
	return status
}

// Rebuilds the hardware clients from the given settings. Caller must hold the mutex (or be constructing the field).
func (field *Field) loadSettings(settings model.EventSettings) {
	field.settings = settings
	wifiStatuses := [6]*network.TeamWifiStatus{}
	for i := range field.wifiStatuses {
		wifiStatuses[i] = &field.wifiStatuses[i]
	}
	field.accessPoint.SetSettings(settings.ApAddress, settings.ApPassword, settings.ApChannel, wifiStatuses)
	field.networkSwitch = network.NewSwitch(settings.SwitchAddress, settings.SwitchPassword)
	upCommands := strings.Split(settings.SCCUpCommands, "\n")
	downCommands := strings.Split(settings.SCCDownCommands, "\n")
	field.redSCC = network.NewSCCSwitch(
		settings.RedSCCAddress, settings.SCCUsername, settings.SCCPassword, upCommands, downCommands,
	)
	field.blueSCC = network.NewSCCSwitch(
		settings.BlueSCCAddress, settings.SCCUsername, settings.SCCPassword, upCommands, downCommands,
	)
}

// Creates or updates the team's database record if its WPA key changed. Caller must hold the mutex.
func (field *Field) saveTeamWpaKey(assignment Assignment) error {
	team, err := field.database.GetTeamById(assignment.TeamId)
	if err != nil {
		return err
	}
	if team == nil {
		return field.database.CreateTeam(&model.Team{Id: assignment.TeamId, WpaKey: assignment.WpaKey})
	}
	if team.WpaKey == assignment.WpaKey {
		return nil
	}
	team.WpaKey = assignment.WpaKey
	return field.database.UpdateTeam(team)
}

// Builds the team array the network package expects. Caller must hold the mutex (or be constructing the field).
func (field *Field) teams() [6]*model.Team {
	var teams [6]*model.Team
	for i, assignment := range field.assignments {
		if assignment.TeamId != 0 {
			teams[i] = &model.Team{Id: assignment.TeamId, WpaKey: assignment.WpaKey}
		}
	}
	return teams
}

// Reconfigures the switch VLANs for the current assignment, disabling SCC team ports while it does so. Waits for any
// in-progress switch configuration first, then reads the latest assignment so a quick second apply isn't overwritten.
func (field *Field) configureEthernet() {
	field.switchMutex.Lock()
	defer field.switchMutex.Unlock()

	field.mutex.Lock()
	teams := field.teams()
	networkSwitch := field.networkSwitch
	sccEnabled := field.settings.SCCManagementEnabled
	redSCC, blueSCC := field.redSCC, field.blueSCC
	field.mutex.Unlock()

	if sccEnabled {
		setSCCEthernetEnabled(redSCC, blueSCC, false)
	}
	if err := networkSwitch.ConfigureTeamEthernet(teams); err != nil {
		log.Printf("Failed to configure team Ethernet: %v", err)
	} else {
		log.Printf("Switch configured for stations %s", describeTeams(teams))
	}
	if sccEnabled {
		setSCCEthernetEnabled(redSCC, blueSCC, true)
	}
}

// Enables or disables the team ethernet ports on both SCCs in parallel.
func setSCCEthernetEnabled(redSCC, blueSCC *network.SCCSwitch, enabled bool) {
	var wg sync.WaitGroup
	configureSCC := func(scc *network.SCCSwitch, name string) {
		defer wg.Done()
		if err := scc.SetTeamEthernetEnabled(enabled); err != nil {
			log.Printf("Failed to set %s SCC enabled state to %t: %v", name, enabled, err)
		}
	}
	wg.Add(2)
	go configureSCC(redSCC, "red")
	go configureSCC(blueSCC, "blue")
	wg.Wait()
}

func validateAssignments(assignments [6]Assignment) error {
	seen := map[int]string{}
	for i, assignment := range assignments {
		station := StationNames[i]
		if assignment.TeamId == 0 {
			continue
		}
		if assignment.TeamId < 0 || assignment.TeamId > maxTeamId {
			return ValidationError(fmt.Sprintf("%s: team number must be between 1 and %d", station, maxTeamId))
		}
		if otherStation, ok := seen[assignment.TeamId]; ok {
			return ValidationError(fmt.Sprintf("%s: team %d is already in %s", station, assignment.TeamId, otherStation))
		}
		seen[assignment.TeamId] = station
		if len(assignment.WpaKey) < minWpaKeyLength || len(assignment.WpaKey) > maxWpaKeyLength {
			return ValidationError(
				fmt.Sprintf("%s: password must be %d to %d characters", station, minWpaKeyLength, maxWpaKeyLength),
			)
		}
	}
	return nil
}

// Formats teams as "R1=254 R2=- ..." for the log.
func describeTeams(teams [6]*model.Team) string {
	parts := make([]string, len(teams))
	for i, team := range teams {
		teamId := "-"
		if team != nil {
			teamId = fmt.Sprint(team.Id)
		}
		parts[i] = StationNames[i] + "=" + teamId
	}
	return strings.Join(parts, " ")
}
