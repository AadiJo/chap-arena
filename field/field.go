// Copyright 2026 Advait Johari. All Rights Reserved.
//
// Chap Arena's core: it owns the connection to the two pieces of field hardware and applies a
// station assignment to both of them.

package field

import (
	"fmt"
	"github.com/AadiJo/chap-arena/model"
	"github.com/AadiJo/chap-arena/network"
	"log"
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

// Starts the background access point monitoring loop and re-applies the persisted station
// assignment so that the field comes back up in its last known state after a restart.
func (field *Field) Run() {
	go field.AccessPoint.Run()
	field.applyAsync(field.Settings.StationTeamIds())
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
func (field *Field) Apply(teamIds [6]int) error {
	if err := field.validate(teamIds); err != nil {
		return err
	}

	field.Settings.SetStationTeamIds(teamIds)
	if err := field.Database.UpdateEventSettings(field.Settings); err != nil {
		return err
	}

	field.applyAsync(teamIds)
	return nil
}

// Rejects assignments the hardware can't represent, before anything is persisted.
func (field *Field) validate(teamIds [6]int) error {
	occupied := 0
	seen := make(map[int]struct{}, len(teamIds))
	for i, teamId := range teamIds {
		if teamId == 0 {
			continue
		}
		occupied++
		if teamId < 1 || teamId > MaxTeamId {
			return fmt.Errorf("%s: team %d is outside the range 1-%d", StationNames[i], teamId, MaxTeamId)
		}
		if _, duplicate := seen[teamId]; duplicate {
			return fmt.Errorf("team %d is assigned to more than one station", teamId)
		}
		seen[teamId] = struct{}{}
	}

	// The WPA key only matters once there's at least one radio to configure.
	if occupied > 0 && field.Settings.RadioEnabled {
		length := len(field.Settings.TeamWpaKey)
		if length < model.MinWpaKeyLength || length > model.MaxWpaKeyLength {
			return fmt.Errorf(
				"the team WPA key must be %d-%d characters; set it on the settings page",
				model.MinWpaKeyLength,
				model.MaxWpaKeyLength,
			)
		}
	}
	return nil
}

// Configures the radio and the switch in the background, recording the outcome of each. Everything
// the background goroutine needs is captured up front, so that a settings save swapping out the
// switch client or the settings pointer can't race with an apply that's still running.
func (field *Field) applyAsync(teamIds [6]int) {
	teams := field.stationTeams(teamIds)
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
// that the access point and the switch skip them entirely.
func (field *Field) stationTeams(teamIds [6]int) [6]*model.Team {
	var teams [6]*model.Team
	for i, teamId := range teamIds {
		if teamId == 0 {
			continue
		}
		teams[i] = &model.Team{Id: teamId, WpaKey: field.Settings.TeamWpaKey}
	}
	return teams
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
