// NetworkTables telemetry: a read-only NT4 client on every robot assigned to a station, subscribed to the topics
// configured for its team, with an optional recording of every value to CSV (see recording.go).
//
// Each robot gets a topics-only subscription to everything, so the UI can list what it publishes, plus a value
// subscription to just the configured topics at the robot's full rate. Topics are configured per team and saved on the
// team record, so they follow the team to whatever station it's in.

package field

import (
	"context"
	"fmt"
	"github.com/Team254/cheesy-arena/nt"
	"log"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ntClientName        = "chap-arena"
	ntDialTimeout       = 3 * time.Second
	ntRetryPeriod       = 2 * time.Second        // After a failed dial.
	ntReconnectDelay    = 200 * time.Millisecond // After a dropped connection, which is often brief.
	ntTimeSyncPeriod    = 3 * time.Second
	ntValuePeriodSec    = 0.02 // Asks for values every 20 ms, a robot's loop period, instead of NT's default 100 ms.
	topicRateWindow     = time.Second
	maxTopicsPerTeam    = 50
	maxTopicNameLength  = 255
	ntConnected         = "connected"
	ntDisconnected      = "disconnected"
	ntOffField          = "offField"
	telemetryListLimit  = 2000 // Topics listed per robot, in case one publishes an unreasonable number.
	lastValueArrayLimit = 4
)

// TelemetryStatus is what GET /api/telemetry returns.
type TelemetryStatus struct {
	Teams []TelemetryTeam `json:"teams"`
}

// TelemetryTeam is a team that's on the field or has topics configured. Assigned teams come first, in station order.
type TelemetryTeam struct {
	TeamId  int    `json:"teamId"`
	Station string `json:"station"` // "" when not assigned.
	// "connected", "disconnected" (assigned, but its NT server can't be reached) or "offField".
	Connection string           `json:"connection"`
	Topics     []TopicStatus    `json:"topics"`    // Configured topics, in the order they were added.
	Available  []AvailableTopic `json:"available"` // Everything the robot publishes, by name. Empty unless connected.
}

type TopicStatus struct {
	Name string `json:"name"`
	Type string `json:"type"` // "" until the robot has announced it.
	Hz   int    `json:"hz"`   // Values received in the last second.
	Last string `json:"last"` // The last value, formatted for display; "" if none yet.
}

type AvailableTopic struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Supported bool   `json:"supported"` // Whether values decode (see nt.Decodable); others are recorded as raw bytes.
}

// RecordingStatus is included in the field status.
type RecordingStatus struct {
	Active    bool      `json:"active"`
	StartedAt time.Time `json:"startedAt"`
	Folder    string    `json:"folder"`
}

type telemetry struct {
	ntAddress     func(teamId int) string // nil disables NT connections.
	recordingsDir string

	// Guards everything below. Never held while doing network I/O; recording writes are buffered.
	mutex        sync.Mutex
	stationTeams [6]int
	links        map[int]*robotLink        // Assigned teams, by team id.
	topics       map[int][]string          // Configured topics, only for teams with at least one.
	types        map[int]map[string]string // Last known type of each configured topic, kept while a robot is away.
	recorder     *recorder                 // nil unless recording.
}

// One assigned robot's NT connection, kept retrying until the team leaves its station.
type robotLink struct {
	teamId        int
	ctx           context.Context
	cancel        context.CancelFunc
	topicsChanged chan struct{} // Nudges the connection to resubscribe with the team's current topics.

	// Guarded by telemetry.mutex.
	connected bool
	available map[string]string // Topic name to type, from the robot's announcements.
	stats     map[string]*topicStats
}

type topicStats struct {
	last     string
	received []time.Time // Local receive times within the last topicRateWindow.
}

func newTelemetry(ntAddress func(teamId int) string, recordingsDir string, topics map[int][]string) *telemetry {
	telemetry := &telemetry{
		ntAddress: ntAddress, recordingsDir: recordingsDir, links: map[int]*robotLink{}, topics: map[int][]string{},
		types: map[int]map[string]string{},
	}
	for teamId, teamTopics := range topics {
		if len(teamTopics) > 0 {
			telemetry.topics[teamId] = teamTopics
		}
	}
	return telemetry
}

// Cleans up topic names as entered: trims whitespace and drops blanks and duplicates. Returns a ValidationError if
// there are too many or one is too long.
func normalizeTopics(topics []string) ([]string, error) {
	seen := map[string]bool{}
	var normalized []string
	for _, topic := range topics {
		topic = strings.TrimSpace(topic)
		if topic == "" || seen[topic] {
			continue
		}
		if len(topic) > maxTopicNameLength {
			return nil, ValidationError(fmt.Sprintf("topic names can be at most %d characters", maxTopicNameLength))
		}
		seen[topic] = true
		normalized = append(normalized, topic)
	}
	if len(normalized) > maxTopicsPerTeam {
		return nil, ValidationError(fmt.Sprintf("at most %d topics per team", maxTopicsPerTeam))
	}
	return normalized, nil
}

// Connects to newly assigned robots and disconnects from robots whose team left the field.
func (telemetry *telemetry) setStations(teamIds [6]int) {
	telemetry.mutex.Lock()
	defer telemetry.mutex.Unlock()
	telemetry.stationTeams = teamIds
	if telemetry.ntAddress == nil {
		return
	}
	assigned := map[int]bool{}
	for _, teamId := range teamIds {
		if teamId == 0 {
			continue
		}
		assigned[teamId] = true
		if telemetry.links[teamId] == nil {
			ctx, cancel := context.WithCancel(context.Background())
			link := &robotLink{
				teamId: teamId, ctx: ctx, cancel: cancel, topicsChanged: make(chan struct{}, 1),
				stats: map[string]*topicStats{},
			}
			telemetry.links[teamId] = link
			go telemetry.run(link, telemetry.ntAddress(teamId))
		}
	}
	for teamId, link := range telemetry.links {
		if !assigned[teamId] {
			link.cancel()
			delete(telemetry.links, teamId)
		}
	}
}

// Replaces a team's configured topics (already normalized) and resubscribes if its robot is connected.
func (telemetry *telemetry) setTopics(teamId int, topics []string) {
	telemetry.mutex.Lock()
	defer telemetry.mutex.Unlock()
	if len(topics) == 0 {
		delete(telemetry.topics, teamId)
	} else {
		telemetry.topics[teamId] = topics
	}
	if link := telemetry.links[teamId]; link != nil {
		for _, name := range topics {
			if typeName, ok := link.available[name]; ok {
				telemetry.rememberType(teamId, name, typeName)
			}
		}
		for name := range link.stats {
			if !slices.Contains(topics, name) {
				delete(link.stats, name)
			}
		}
		select {
		case link.topicsChanged <- struct{}{}:
		default:
		}
	}
}

// Starts or stops recording. Starting while already recording does nothing.
func (telemetry *telemetry) setRecording(active bool) error {
	telemetry.mutex.Lock()
	defer telemetry.mutex.Unlock()
	if active && telemetry.recorder == nil {
		recorder, err := startRecorder(telemetry.recordingsDir)
		if err != nil {
			return err
		}
		telemetry.recorder = recorder
		go telemetry.flushPeriodically(recorder)
		log.Printf("Recording NetworkTables to %s", recorder.dir)
	} else if !active && telemetry.recorder != nil {
		telemetry.stopRecording("")
	}
	return nil
}

// Closes the recording, with reason in the log if it stopped on its own. Caller holds the mutex.
func (telemetry *telemetry) stopRecording(reason string) {
	recorder := telemetry.recorder
	telemetry.recorder = nil
	err := recorder.close()
	if reason != "" {
		log.Printf("Recording stopped: %s", reason)
	}
	if err != nil {
		log.Printf("Error finishing recording: %v", err)
	}
	log.Printf("Recorded %d values to %s", recorder.rows, recorder.dir)
}

// Flushes the recording once a second, so a crash loses at most a second of data.
func (telemetry *telemetry) flushPeriodically(recorder *recorder) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for range ticker.C {
		telemetry.mutex.Lock()
		if telemetry.recorder != recorder {
			telemetry.mutex.Unlock()
			return
		}
		if err := recorder.flush(); err != nil {
			telemetry.stopRecording(err.Error())
		}
		telemetry.mutex.Unlock()
	}
}

func (telemetry *telemetry) recordingStatus() RecordingStatus {
	telemetry.mutex.Lock()
	defer telemetry.mutex.Unlock()
	status := RecordingStatus{Active: telemetry.recorder != nil}
	if status.Active {
		status.StartedAt = telemetry.recorder.startedAt
		status.Folder = telemetry.recorder.dir
	}
	return status
}

func (telemetry *telemetry) status() TelemetryStatus {
	telemetry.mutex.Lock()
	defer telemetry.mutex.Unlock()
	status := TelemetryStatus{Teams: []TelemetryTeam{}}
	listed := map[int]bool{}
	for i, teamId := range telemetry.stationTeams {
		if teamId != 0 {
			status.Teams = append(status.Teams, telemetry.teamStatus(teamId, StationNames[i]))
			listed[teamId] = true
		}
	}
	var others []int
	for teamId := range telemetry.topics {
		if !listed[teamId] {
			others = append(others, teamId)
		}
	}
	sort.Ints(others)
	for _, teamId := range others {
		status.Teams = append(status.Teams, telemetry.teamStatus(teamId, ""))
	}
	return status
}

// Caller holds the mutex.
func (telemetry *telemetry) teamStatus(teamId int, station string) TelemetryTeam {
	team := TelemetryTeam{
		TeamId: teamId, Station: station, Connection: ntOffField, Topics: []TopicStatus{}, Available: []AvailableTopic{},
	}
	link := telemetry.links[teamId]
	if link != nil {
		team.Connection = ntDisconnected
		if link.connected {
			team.Connection = ntConnected
		}
		for name, typeName := range link.available {
			team.Available = append(team.Available, AvailableTopic{name, typeName, nt.Decodable(typeName)})
		}
		sort.Slice(team.Available, func(i, j int) bool { return team.Available[i].Name < team.Available[j].Name })
		team.Available = team.Available[:min(len(team.Available), telemetryListLimit)]
	}
	now := time.Now()
	for _, name := range telemetry.topics[teamId] {
		topic := TopicStatus{Name: name, Type: telemetry.types[teamId][name]}
		if typeName, ok := link.availableType(name); ok {
			topic.Type = typeName
		}
		if link != nil {
			if stats := link.stats[name]; stats != nil {
				topic.Hz = stats.rate(now)
				topic.Last = stats.last
			}
		}
		team.Topics = append(team.Topics, topic)
	}
	return team
}

// Keeps a connection to the robot until the team leaves its station: reconnects straight away after a drop, then
// retries every few seconds. Logs only when the connection comes up, goes down, or first fails, so an unreachable robot
// doesn't fill the log.
func (telemetry *telemetry) run(link *robotLink, address string) {
	failing := false
	for {
		retryDelay := ntRetryPeriod
		ctx, cancel := context.WithTimeout(link.ctx, ntDialTimeout)
		conn, err := nt.Dial(ctx, address, ntClientName)
		cancel()
		if link.ctx.Err() != nil {
			if conn != nil {
				conn.Close()
			}
			return
		}
		if err != nil {
			if !failing {
				log.Printf("Can't reach team %d's NetworkTables at %s (will keep trying): %v", link.teamId, address, err)
				failing = true
			}
		} else {
			failing = false
			log.Printf("Connected to team %d's NetworkTables at %s.", link.teamId, address)
			err = telemetry.serve(link, conn)
			conn.Close()
			telemetry.mutex.Lock()
			link.connected = false
			link.available = nil
			telemetry.mutex.Unlock()
			if link.ctx.Err() != nil {
				log.Printf("Disconnected from team %d's NetworkTables.", link.teamId)
				return
			}
			log.Printf("Lost team %d's NetworkTables: %v", link.teamId, err)
			retryDelay = ntReconnectDelay
		}
		select {
		case <-link.ctx.Done():
			return
		case <-time.After(retryDelay):
		}
	}
}

// Subscribes and handles events until the connection ends or the team leaves its station.
func (telemetry *telemetry) serve(link *robotLink, conn *nt.Conn) error {
	ctx := link.ctx
	telemetry.mutex.Lock()
	link.connected = true
	link.available = map[string]string{}
	telemetry.mutex.Unlock()
	if _, err := conn.Subscribe(ctx, []string{""}, nt.SubscribeOptions{TopicsOnly: true, Prefix: true}); err != nil {
		return err
	}
	valueSubuid := 0
	subscribeValues := func() error {
		if valueSubuid != 0 {
			if err := conn.Unsubscribe(ctx, valueSubuid); err != nil {
				return err
			}
			valueSubuid = 0
		}
		telemetry.mutex.Lock()
		topics := telemetry.topics[link.teamId]
		telemetry.mutex.Unlock()
		if len(topics) == 0 {
			return nil
		}
		var err error
		valueSubuid, err = conn.Subscribe(ctx, topics, nt.SubscribeOptions{Periodic: ntValuePeriodSec, All: true})
		return err
	}
	if err := subscribeValues(); err != nil {
		return err
	}

	timeSync := time.NewTicker(ntTimeSyncPeriod)
	defer timeSync.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-link.topicsChanged:
			if err := subscribeValues(); err != nil {
				return err
			}
		case <-timeSync.C:
			if err := conn.SyncTime(ctx); err != nil {
				return err
			}
		case event, ok := <-conn.Events():
			if !ok {
				return conn.Err()
			}
			telemetry.handle(link, event)
		}
	}
}

func (telemetry *telemetry) handle(link *robotLink, event nt.Event) {
	telemetry.mutex.Lock()
	defer telemetry.mutex.Unlock()
	switch event := event.(type) {
	case nt.Announce:
		link.available[event.Topic.Name] = event.Topic.Type
		if slices.Contains(telemetry.topics[link.teamId], event.Topic.Name) {
			telemetry.rememberType(link.teamId, event.Topic.Name, event.Topic.Type)
		}
	case nt.Unannounce:
		delete(link.available, event.Topic.Name)
	case nt.Value:
		// A value can still arrive for a topic that was just removed, before the robot sees the unsubscribe.
		if !slices.Contains(telemetry.topics[link.teamId], event.Topic.Name) {
			return
		}
		stats := link.stats[event.Topic.Name]
		if stats == nil {
			stats = &topicStats{}
			link.stats[event.Topic.Name] = stats
		}
		now := time.Now()
		stats.received = append(stats.received, now)
		stats.trim(now)
		stats.last = formatValue(event.Data)
		if telemetry.recorder != nil {
			if err := telemetry.recorder.write(link.teamId, event); err != nil {
				telemetry.stopRecording(err.Error())
			}
		}
	}
}

// Keeps a configured topic's type so it still shows while the robot is away. Caller holds the mutex.
func (telemetry *telemetry) rememberType(teamId int, name, typeName string) {
	if telemetry.types[teamId] == nil {
		telemetry.types[teamId] = map[string]string{}
	}
	telemetry.types[teamId][name] = typeName
}

// The type the robot announced for a topic. Works on a nil link. Caller holds the mutex.
func (link *robotLink) availableType(name string) (string, bool) {
	if link == nil {
		return "", false
	}
	typeName, ok := link.available[name]
	return typeName, ok
}

// Values received in the last topicRateWindow.
func (stats *topicStats) rate(now time.Time) int {
	stats.trim(now)
	return len(stats.received)
}

func (stats *topicStats) trim(now time.Time) {
	cutoff := now.Add(-topicRateWindow)
	i := 0
	for i < len(stats.received) && stats.received[i].Before(cutoff) {
		i++
	}
	stats.received = stats.received[i:]
}

// Formats a decoded value for the UI: poses as "x, y, degrees°", long arrays summarized.
func formatValue(data any) string {
	number := func(value float64) string { return strconv.FormatFloat(value, 'f', -1, 64) }
	switch value := data.(type) {
	case nt.Pose2d:
		return fmt.Sprintf("%.2f, %.2f, %.1f°", value.X, value.Y, value.Rotation*180/math.Pi)
	case []nt.Pose2d:
		return fmt.Sprintf("%d poses", len(value))
	case float64:
		return strconv.FormatFloat(value, 'g', 6, 64)
	case string:
		return strconv.Quote(value)
	case []byte:
		return fmt.Sprintf("%d bytes", len(value))
	case []float64:
		return formatArray(value, func(item float64) string { return number(math.Round(item*100) / 100) })
	case []int64:
		return formatArray(value, func(item int64) string { return strconv.FormatInt(item, 10) })
	case []bool:
		return formatArray(value, strconv.FormatBool)
	case []string:
		return formatArray(value, strconv.Quote)
	}
	return fmt.Sprint(data)
}

func formatArray[T any](values []T, format func(T) string) string {
	if len(values) > lastValueArrayLimit {
		return fmt.Sprintf("%d values", len(values))
	}
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = format(value)
	}
	return strings.Join(parts, ", ")
}
