// Copyright 2026 Advait Johari. All Rights Reserved.
//
// Chap Arena's core: it owns the connection to the two pieces of field hardware and applies a
// station assignment to both of them.

package field

import (
	"errors"
	"fmt"
	"github.com/AadiJo/chap-arena/model"
	"github.com/AadiJo/chap-arena/network"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The highest team number the field addressing scheme can represent. Team IPs are 10.TE.AM.x, so
// both halves of the team number have to fit in an octet.
const MaxTeamId = 25599

type Field struct {
	Database    *model.Database
	Settings    *model.EventSettings
	AccessPoint network.AccessPoint
	Switch      *network.Switch

	// Populated in place by the access point monitoring loop, one entry per alliance station.
	wifiStatuses [6]*network.TeamWifiStatus

	mutex  sync.Mutex
	status ApplyStatus
}

// One station's entry on the configuration page.
type Assignment struct {
	// The team occupying the station. Zero leaves the station out of both the radio and the switch
	// configuration, which is how a bypassed station behaves on a real field.
	TeamId int

	// Overrides the shared WPA key for this station. Blank means the station uses the shared key.
	WpaKey string
}

// The outcome of the most recent attempt to push a station assignment to the hardware.
type ApplyStatus struct {
	InProgress  bool      `json:"inProgress"`
	RadioError  string    `json:"radioError"`
	SwitchError string    `json:"switchError"`
	AppliedAt   time.Time `json:"appliedAt"`
}

// Opens the database and brings up the hardware clients using the persisted settings.
func NewField(dbPath string) (*Field, error) {
	field := new(Field)
	for i := range field.wifiStatuses {
		field.wifiStatuses[i] = new(network.TeamWifiStatus)
	}

	var err error
	if field.Database, err = model.OpenDatabase(dbPath); err != nil {
		return nil, err
	}
	if err = field.LoadSettings(); err != nil {
		return nil, err
	}
	return field, nil
}

// Reloads the settings from the database and points the hardware clients at the configured
// addresses. Safe to call again whenever the settings are edited.
func (field *Field) LoadSettings() error {
	settings, err := field.Database.GetEventSettings()
	if err != nil {
		return err
	}
	field.Settings = settings

	field.AccessPoint.SetSettings(
		settings.ApAddress, settings.ApPassword, settings.ApChannel, settings.RadioEnabled, field.wifiStatuses,
	)
	field.Switch = network.NewSwitch(settings.SwitchAddress, settings.SwitchPassword)
	return nil
}

// Returns the path of the database file currently in use.
func (field *Field) DatabasePath() string {
	return field.Database.Path
}

// Points the field at a different database file and records the choice so that it survives a
// restart. The new database is opened before the old one is closed, so a bad path leaves the field
// running on the database it already had.
//
// The new database carries its own settings and station assignment; they are loaded but not pushed
// to the hardware, so nothing on the field changes until Apply is pressed.
func (field *Field) SetDatabasePath(dbPath string) error {
	dbPath = strings.TrimSpace(dbPath)
	if dbPath == "" {
		return errors.New("the database path can't be blank")
	}
	if filepath.Clean(dbPath) == filepath.Clean(field.Database.Path) {
		return nil
	}

	// Creating the parent directory first means a path like D:\field\event.db works without having
	// to go and make the folder by hand.
	if parent := filepath.Dir(dbPath); parent != "" {
		if err := os.MkdirAll(parent, 0755); err != nil {
			return fmt.Errorf("can't create %s: %w", parent, err)
		}
	}

	database, err := model.OpenDatabase(dbPath)
	if err != nil {
		return fmt.Errorf("can't open %s: %w", dbPath, err)
	}
	if err = model.SaveBootstrap(model.Bootstrap{DatabasePath: dbPath}); err != nil {
		database.Close()
		return fmt.Errorf("can't record the new database path: %w", err)
	}

	if err = field.Database.Close(); err != nil {
		log.Printf("Failed to close the previous database: %v", err)
	}
	field.Database = database
	return field.LoadSettings()
}

// Starts the background access point monitoring loop and re-applies the persisted station
// assignment so that the field comes back up in its last known state after a restart.
func (field *Field) Run() {
	go field.AccessPoint.Run()
	field.applyAsync(field.Assignments())
}

// Returns the per-station wifi statuses reported by the access point, in alliance station order.
func (field *Field) WifiStatuses() [6]network.TeamWifiStatus {
	var statuses [6]network.TeamWifiStatus
	for i, status := range field.wifiStatuses {
		statuses[i] = *status
	}
	return statuses
}

// Returns the outcome of the most recent apply.
func (field *Field) ApplyStatus() ApplyStatus {
	field.mutex.Lock()
	defer field.mutex.Unlock()
	return field.status
}

// Validates and persists the given station assignment, then pushes it to the radio and the switch in
// the background. A team ID of zero leaves that station out of both configurations, which is how a
// bypassed station behaves on a real field: no SSID is broadcast and no VLAN is created for it.
func (field *Field) Apply(assignments [6]Assignment) error {
	if err := field.validate(assignments); err != nil {
		return err
	}

	var teamIds [6]int
	var wpaKeys [6]string
	for i, assignment := range assignments {
		teamIds[i] = assignment.TeamId
		wpaKeys[i] = assignment.WpaKey
	}
	field.Settings.SetStationTeamIds(teamIds)
	field.Settings.SetStationWpaKeys(wpaKeys)
	if err := field.Database.UpdateEventSettings(field.Settings); err != nil {
		return err
	}

	field.applyAsync(assignments)
	return nil
}

// Returns the persisted station assignment.
func (field *Field) Assignments() [6]Assignment {
	teamIds := field.Settings.StationTeamIds()
	wpaKeys := field.Settings.StationWpaKeys()

	var assignments [6]Assignment
	for i := range assignments {
		assignments[i] = Assignment{TeamId: teamIds[i], WpaKey: wpaKeys[i]}
	}
	return assignments
}

// Rejects assignments the hardware can't represent, before anything is persisted.
func (field *Field) validate(assignments [6]Assignment) error {
	needsSharedKey := false
	seen := make(map[int]struct{}, len(assignments))
	for i, assignment := range assignments {
		if assignment.TeamId == 0 {
			continue
		}
		if assignment.TeamId < 1 || assignment.TeamId > MaxTeamId {
			return fmt.Errorf(
				"%s: team %d is outside the range 1-%d", StationNames[i], assignment.TeamId, MaxTeamId,
			)
		}
		if _, duplicate := seen[assignment.TeamId]; duplicate {
			return fmt.Errorf("team %d is assigned to more than one station", assignment.TeamId)
		}
		seen[assignment.TeamId] = struct{}{}

		// Keys only matter once there's a radio to configure. A station without an override falls
		// back to the shared key, so the shared key only has to be valid if some station needs it.
		if !field.Settings.RadioEnabled {
			continue
		}
		if assignment.WpaKey == "" {
			needsSharedKey = true
		} else if !validWpaKeyLength(assignment.WpaKey) {
			return fmt.Errorf("%s: %s", StationNames[i], wpaKeyLengthError)
		}
	}

	if needsSharedKey && !validWpaKeyLength(field.Settings.TeamWpaKey) {
		return fmt.Errorf("%s Set it on the settings page, or override it per station.", wpaKeyLengthError)
	}
	return nil
}

var wpaKeyLengthError = fmt.Sprintf(
	"the WPA key must be %d-%d characters.", model.MinWpaKeyLength, model.MaxWpaKeyLength,
)

func validWpaKeyLength(wpaKey string) bool {
	return len(wpaKey) >= model.MinWpaKeyLength && len(wpaKey) <= model.MaxWpaKeyLength
}

// Configures the radio and the switch in the background, recording the outcome of each. Everything
// the background goroutine needs is captured up front, so that a settings save swapping out the
// switch client or the settings pointer can't race with an apply that's still running.
func (field *Field) applyAsync(assignments [6]Assignment) {
	teams := field.stationTeams(assignments)
	radioEnabled := field.Settings.RadioEnabled
	switchEnabled := field.Settings.SwitchEnabled
	networkSwitch := field.Switch

	field.mutex.Lock()
	field.status = ApplyStatus{InProgress: true}
	field.mutex.Unlock()

	go func() {
		var radioErr, switchErr error
		var waitGroup sync.WaitGroup

		if radioEnabled {
			waitGroup.Add(1)
			go func() {
				defer waitGroup.Done()
				if radioErr = field.AccessPoint.ConfigureTeamWifi(teams); radioErr != nil {
					log.Printf("Failed to configure team wifi: %v", radioErr)
				}
			}()
		}
		if switchEnabled {
			waitGroup.Add(1)
			go func() {
				defer waitGroup.Done()
				if switchErr = networkSwitch.ConfigureTeamEthernet(teams); switchErr != nil {
					log.Printf("Failed to configure team ethernet: %v", switchErr)
				}
			}()
		}
		waitGroup.Wait()

		field.mutex.Lock()
		field.status = ApplyStatus{
			RadioError:  errorText(radioErr),
			SwitchError: errorText(switchErr),
			AppliedAt:   time.Now(),
		}
		field.mutex.Unlock()
	}()
}

// Builds the per-station team models the network package configures from. Empty stations stay nil so
// that the access point and the switch skip them entirely, and a station without its own key gets
// the shared one.
func (field *Field) stationTeams(assignments [6]Assignment) [6]*model.Team {
	var teams [6]*model.Team
	for i, assignment := range assignments {
		if assignment.TeamId == 0 {
			continue
		}
		wpaKey := assignment.WpaKey
		if wpaKey == "" {
			wpaKey = field.Settings.TeamWpaKey
		}
		teams[i] = &model.Team{Id: assignment.TeamId, WpaKey: wpaKey}
	}
	return teams
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
