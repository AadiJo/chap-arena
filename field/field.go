// Copyright 2014 Team 254. All Rights Reserved.
// Author: pat@patfairbank.com (Patrick Fairbank)
//
// Owns the field network hardware (access point, switch, SCC switches), the team assigned to each driver station, the
// optional FMS connection to the driver stations (see driver_station.go), and NetworkTables recording from the robots
// (see telemetry.go). The FMS connection starts off, so teams can enable their robots themselves once their radio
// links.

package field

import (
	"errors"
	"fmt"
	"github.com/Team254/cheesy-arena/model"
	"github.com/Team254/cheesy-arena/network"
	"log"
	"slices"
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
	Station       string                 `json:"station"`
	Assignment    Assignment             `json:"assignment"`
	Wifi          network.TeamWifiStatus `json:"wifi"`
	DriverStation DriverStationStatus    `json:"driverStation"`
}

type Status struct {
	Stations          [6]StationStatus  `json:"stations"`
	AccessPointStatus string            `json:"accessPointStatus"`
	SwitchStatus      string            `json:"switchStatus"`
	LastApplied       time.Time         `json:"lastApplied"`
	DriverStationMode DriverStationMode `json:"driverStationMode"`
	Recording         RecordingStatus   `json:"recording"`
}

// Options are the parts of the field that differ between a real field and tests.
type Options struct {
	// Ports driver stations connect to while FMS is on.
	DriverStationPorts DriverStationPorts
	// Where each team's robot serves NetworkTables, e.g. nt.RobotAddress. nil means never connect to robots.
	NtAddress func(teamId int) string
	// Recording sessions are created as folders in here.
	RecordingsDir string
	// Overrides for pointing the switch and SCCs at fakes in tests. Zero values mean the real hardware.
	Switch      network.SwitchOptions
	RedSCCPort  int
	BlueSCCPort int
}

type Field struct {
	database *model.Database
	options  Options

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

	// Each has its own lock, always taken after mutex when both are needed.
	driverStations *driverStations
	telemetry      *telemetry

	// Serializes switch reconfiguration so the last apply always wins.
	switchMutex sync.Mutex

	// Serializes Apply and ReassignStation, so a team changing its station from the stations it read can't undo an apply
	// that landed in between. Taken before mutex.
	applyMutex sync.Mutex
}

// Loads settings and the last applied station assignment from the database. The access point is told to expect that
// assignment, so a restart leaves linked robots alone unless the AP has drifted. Starts connecting to the assigned
// robots' NetworkTables right away; driver stations are listened for once SetDriverStationMode turns FMS on.
func New(database *model.Database, options Options) (*Field, error) {
	settings, err := database.GetEventSettings()
	if err != nil {
		return nil, err
	}
	teams, err := database.GetAllTeams()
	if err != nil {
		return nil, err
	}
	topics := map[int][]string{}
	for _, team := range teams {
		topics[team.Id] = team.NtTopics
	}
	field := Field{
		database:       database,
		options:        options,
		driverStations: newDriverStations(options.DriverStationPorts),
		telemetry:      newTelemetry(options.NtAddress, options.RecordingsDir, topics),
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
	field.loadSettings(withDefaults(*settings))
	field.accessPoint.SetExpectedTeams(field.teams())
	field.driverStations.setTeams(settings.StationTeamIds)
	field.telemetry.setStations(settings.StationTeamIds)
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

// Saves the settings and rebuilds the hardware clients. It does not push anything to the hardware; the next apply (or
// the AP monitoring loop) uses the new settings, and the next recording uses the new format. Blank settings that have
// defaults get them (see withDefaults). Returns a ValidationError, saving nothing, if the default WPA key isn't a usable
// key or the recording format is unknown.
func (field *Field) UpdateSettings(settings model.EventSettings) error {
	if key := settings.DefaultWpaKey; key != "" && (len(key) < minWpaKeyLength || len(key) > maxWpaKeyLength) {
		return ValidationError(
			fmt.Sprintf("default password must be %d to %d characters", minWpaKeyLength, maxWpaKeyLength),
		)
	}
	settings = withDefaults(settings)
	if !validRecordingFormat(settings.RecordingFormat) {
		return ValidationError(fmt.Sprintf("unknown recording format %q", settings.RecordingFormat))
	}
	field.mutex.Lock()
	defer field.mutex.Unlock()
	settings.Id = field.settings.Id
	settings.StationTeamIds = field.settings.StationTeamIds
	if err := field.database.UpdateEventSettings(&settings); err != nil {
		return err
	}
	field.loadSettings(settings)
	log.Println("Settings saved.")
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
// (in the background, since the switch takes several seconds). Assigned stations with a blank WPA key get the default
// key from settings. New WPA keys are saved to each team's database record.
func (field *Field) Apply(assignments [6]Assignment) error {
	field.applyMutex.Lock()
	defer field.applyMutex.Unlock()
	return field.apply(assignments)
}

// Gives the station teamId is on to a new team and WPA key, for a team changing its own station from the team page,
// and applies it like Apply with the other five stations as they are. A blank key uses the new team's saved key (so a
// team keeps its own), else the default. Returns a ValidationError, changing nothing, if teamId isn't on a station, the
// new team number is missing, or Apply would reject the result.
func (field *Field) ReassignStation(teamId int, assignment Assignment) error {
	field.applyMutex.Lock()
	defer field.applyMutex.Unlock()
	field.mutex.Lock()
	assignments := field.assignments
	field.mutex.Unlock()
	station := slices.IndexFunc(assignments[:], func(a Assignment) bool { return a.TeamId == teamId })
	if teamId == 0 || station == -1 {
		return ValidationError(fmt.Sprintf("team %d isn't on a station", teamId))
	}
	if assignment.TeamId == 0 {
		return ValidationError("enter a team number")
	}
	if assignment.WpaKey == "" {
		savedKey, _, err := field.TeamWpaKey(assignment.TeamId)
		if err != nil {
			return err
		}
		assignment.WpaKey = savedKey
	}
	assignments[station] = assignment
	err := field.apply(assignments)
	if !errors.As(err, new(ValidationError)) {
		log.Printf("Team %d changed %s to team %d from the team page.", teamId, StationNames[station], assignment.TeamId)
	}
	return err
}

// Apply without applyMutex, which the caller holds.
func (field *Field) apply(assignments [6]Assignment) error {
	field.mutex.Lock()
	defaultWpaKey := field.settings.DefaultWpaKey
	field.mutex.Unlock()
	for i := range assignments {
		if assignments[i].TeamId != 0 && assignments[i].WpaKey == "" {
			assignments[i].WpaKey = defaultWpaKey
		}
	}
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
	field.driverStations.setTeams(field.settings.StationTeamIds)
	field.telemetry.setStations(field.settings.StationTeamIds)
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

// Turns the FMS connection to driver stations off, or on with every robot disabled or enabled. See DriverStationMode.
func (field *Field) SetDriverStationMode(mode DriverStationMode) error {
	return field.driverStations.setMode(mode)
}

// Saves what to record from a team's robot, replacing what it had, and resubscribes if its robot is connected. Each
// entry is a topic name, a section ending in "/", or either with "!" in front to exclude it (see telemetry.go). Blank
// and duplicate entries are dropped; an empty list stops recording the team. Returns a ValidationError, saving nothing,
// for an invalid team number or too many entries.
func (field *Field) SetTeamTopics(teamId int, topics []string) error {
	if teamId < 1 || teamId > maxTeamId {
		return ValidationError(fmt.Sprintf("team number must be between 1 and %d", maxTeamId))
	}
	topics, err := normalizeTopics(topics)
	if err != nil {
		return err
	}
	field.mutex.Lock()
	defer field.mutex.Unlock()
	team, err := field.database.GetTeamById(teamId)
	if err != nil {
		return err
	}
	if team == nil {
		if len(topics) > 0 {
			err = field.database.CreateTeam(&model.Team{Id: teamId, NtTopics: topics})
		}
	} else {
		team.NtTopics = topics
		err = field.database.UpdateTeam(team)
	}
	if err != nil {
		return err
	}
	field.telemetry.setTopics(teamId, topics)
	return nil
}

// Starts or stops recording every team's topics to a new folder under Options.RecordingsDir, in the format chosen in
// settings.
func (field *Field) SetRecording(active bool) error {
	return field.telemetry.setRecording(active, field.recordingFormat())
}

func (field *Field) Telemetry() TelemetryStatus {
	return field.telemetry.status(field.recordingFormat())
}

func (field *Field) recordingFormat() string {
	field.mutex.Lock()
	defer field.mutex.Unlock()
	return field.settings.RecordingFormat
}

func (field *Field) Status() Status {
	field.mutex.Lock()
	defer field.mutex.Unlock()
	driverStationMode, driverStationStatuses := field.driverStations.status()
	status := Status{
		AccessPointStatus: field.accessPoint.Status,
		SwitchStatus:      field.networkSwitch.Status,
		LastApplied:       field.lastApplied,
		DriverStationMode: driverStationMode,
		Recording:         field.telemetry.recordingStatus(),
	}
	for i := range status.Stations {
		status.Stations[i] = StationStatus{
			Station:       StationNames[i],
			Assignment:    field.assignments[i],
			Wifi:          field.wifiStatuses[i],
			DriverStation: driverStationStatuses[i],
		}
	}
	return status
}

// Rebuilds the hardware clients from the given settings. The switch is kept, along with what it knows it has
// configured, unless its address or password changed. Caller must hold the mutex (or be constructing the field).
func (field *Field) loadSettings(settings model.EventSettings) {
	if field.networkSwitch == nil || settings.SwitchAddress != field.settings.SwitchAddress ||
		settings.SwitchPassword != field.settings.SwitchPassword {
		field.networkSwitch = network.NewSwitch(settings.SwitchAddress, settings.SwitchPassword, field.options.Switch)
	}
	field.settings = settings
	wifiStatuses := [6]*network.TeamWifiStatus{}
	for i := range field.wifiStatuses {
		wifiStatuses[i] = &field.wifiStatuses[i]
	}
	field.accessPoint.SetSettings(settings.ApAddress, settings.ApPassword, settings.ApChannel, wifiStatuses)
	stationPorts := [3]string{settings.SCCStation1Port, settings.SCCStation2Port, settings.SCCStation3Port}
	field.redSCC = network.NewSCCSwitch(
		settings.RedSCCAddress, field.options.RedSCCPort, settings.SCCUsername, settings.SCCPassword, stationPorts,
	)
	field.blueSCC = network.NewSCCSwitch(
		settings.BlueSCCAddress, field.options.BlueSCCPort, settings.SCCUsername, settings.SCCPassword, stationPorts,
	)
}

// Fills in settings left blank that have defaults: the recording format and the SCC station ports.
func withDefaults(settings model.EventSettings) model.EventSettings {
	if settings.RecordingFormat == "" {
		settings.RecordingFormat = DefaultRecordingFormat
	}
	for i, port := range []*string{&settings.SCCStation1Port, &settings.SCCStation2Port, &settings.SCCStation3Port} {
		if *port == "" {
			*port = network.DefaultSCCStationPorts[i]
		}
	}
	return settings
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

// Reconfigures the switch VLANs for the current assignment, turning off the SCC ports of the same stations while it
// does so. Only stations whose team changed since the switch was last configured are touched, so driver stations at
// the others stay connected; a key-only change touches nothing. Waits for any in-progress switch configuration first,
// then reads the latest assignment so a quick second apply isn't overwritten.
func (field *Field) configureEthernet() {
	field.switchMutex.Lock()
	defer field.switchMutex.Unlock()

	field.mutex.Lock()
	teams := field.teams()
	networkSwitch := field.networkSwitch
	sccEnabled := field.settings.SCCManagementEnabled
	redSCC, blueSCC := field.redSCC, field.blueSCC
	field.mutex.Unlock()

	stations := networkSwitch.StationsToConfigure(teams)
	if stations == [6]bool{} {
		log.Printf("Switch already configured for stations %s", describeTeams(teams))
		return
	}
	if sccEnabled {
		setSCCStationPortsEnabled(redSCC, blueSCC, stations, false)
	}
	if err := networkSwitch.ConfigureTeamEthernet(teams); err != nil {
		log.Printf("Failed to configure team Ethernet: %v", err)
	} else {
		log.Printf("Switch configured for stations %s (changed %s)", describeTeams(teams), describeStations(stations))
	}
	if sccEnabled {
		setSCCStationPortsEnabled(redSCC, blueSCC, stations, true)
	}
}

// Turns the given stations' ports on both SCCs on or off in parallel. An SCC with none of them isn't contacted.
func setSCCStationPortsEnabled(redSCC, blueSCC *network.SCCSwitch, stations [6]bool, enabled bool) {
	var wg sync.WaitGroup
	configureSCC := func(scc *network.SCCSwitch, name string, stations [3]bool) {
		defer wg.Done()
		if err := scc.SetStationPortsEnabled(stations, enabled); err != nil {
			log.Printf("Failed to set %s SCC station ports enabled state to %t: %v", name, enabled, err)
		}
	}
	wg.Add(2)
	go configureSCC(redSCC, "red", [3]bool(stations[:3]))
	go configureSCC(blueSCC, "blue", [3]bool(stations[3:]))
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

// Formats the selected stations as "R3 B1" for the log.
func describeStations(stations [6]bool) string {
	var names []string
	for i, selected := range stations {
		if selected {
			names = append(names, StationNames[i])
		}
	}
	return strings.Join(names, " ")
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
