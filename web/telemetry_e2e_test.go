// End-to-end test of NetworkTables recording: the real HTTP API, field controller and NT4 client against fake robots
// that speak NT4 over a real WebSocket. Writes a transcript to e2e-output/telemetry.txt and copies the recordings to
// e2e-output/recording/wpilog/ and e2e-output/recording/csv/ for inspection (or opening in AdvantageScope).
//
// Ways this can fail, each checked below:
//  1. The client doesn't offer the NT4 subprotocol, so a real robot rejects it (the fake requires it).
//  2. Value subscriptions use the default 100 ms periodic or latest-only, so recordings come out at 10 Hz.
//  3. Topics that weren't configured get subscribed to or recorded.
//  4. Several messages in one binary frame are decoded wrong (the fake batches every topic into one frame).
//  5. Robot timestamps are converted to local time with the wrong sign or offset (the fake's clock starts at boot).
//  6. Pose2d fields are decoded in the wrong order or endianness.
//  7. Changing a team's topics mid-connection doesn't change the subscription.
//  8. A dropped connection isn't retried, or loses the subscription when it comes back.
//  9. Moving a team off its station leaves its robot connection open.
// 10. Stopping a recording loses buffered values, or file names and headers are wrong.
// 11. Topics are saved in a way that loses the team's other fields.
// 12. Struct schemas aren't subscribed to or recorded, so AdvantageScope can't decode structs. Robots send them once per
//     connection, usually before recording starts, and AdvantageKit keeps its own under /AdvantageKit.
// 13. The recording format setting is ignored, or a CSV isn't in the layout AdvantageScope imports.
// 14. A saved section misses topics the robot starts publishing after the subscription.
// 15. An excluded topic or section under a saved section is recorded anyway, or a topic saved by name inside an excluded
//     section is dropped.
// 16. A section puts types a CSV can't hold into one as raw bytes (a topic saved by name still keeps them).
// 17. A session doesn't note the station each team was on when its file started (stations.json), so the team page can't
//     color the team's own file by the alliance it played on, or a team moving mid-session rewrites it.

package web

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"github.com/Team254/cheesy-arena/field"
	"github.com/Team254/cheesy-arena/model"
	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmihailenco/msgpack/v5"
	"go.etcd.io/bbolt"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A topic on a fake robot. value returns the NT4 data type index and the value for the n-th sample.
type fakeTopic struct {
	id    int
	name  string
	typ   string
	value func(n int) (int, any)
	once  bool // Sent once per connection, like a schema, rather than every 20 ms.
}

// Schema topics as WPILib publishes them, under prefix, starting at topic id firstId.
func fakeSchemas(firstId int, prefix string, schemas ...[2]string) []fakeTopic {
	var topics []fakeTopic
	for i, schema := range schemas {
		definition := []byte(schema[1])
		topics = append(
			topics, fakeTopic{
				firstId + i, prefix + "/.schema/struct:" + schema[0], "structschema",
				func(n int) (int, any) { return 5, definition }, true,
			},
		)
	}
	return topics
}

var pose2dSchemas = [][2]string{
	{"Pose2d", "Translation2d translation;Rotation2d rotation"}, {"Translation2d", "double x;double y"},
	{"Rotation2d", "double value"},
}

// A sample the fake robot sent: its robot timestamp and the local wall time it corresponds to. n counts the robot's
// samples across connections.
type fakeSample struct {
	robotTime int64
	wallTime  time.Time
	n         int
}

// Plays a robot's NT4 server: announces topics, answers time sync, and sends every value of subscribed topics every
// 20 ms, all topics batched into one binary frame.
type fakeRobot struct {
	t      *testing.T
	server *httptest.Server
	topics []fakeTopic
	// The robot clock counts microseconds from boot, like the roboRIO's FPGA clock.
	boot time.Time

	mutex         sync.Mutex
	n             int
	connections   int
	subscriptions map[int]map[string]any // Current subscriptions on the current connection, by subuid.
	subscribeLog  []string
	sent          map[string][]fakeSample
	closeCurrent  func()
	announce      func() // Announces new topics on the current connection.
	open          bool
}

func newFakeRobot(t *testing.T, topics []fakeTopic) *fakeRobot {
	robot := &fakeRobot{t: t, topics: topics, boot: time.Now().Add(-1000 * time.Second), sent: map[string][]fakeSample{}}
	robot.server = httptest.NewServer(http.HandlerFunc(robot.serve))
	t.Cleanup(robot.server.Close)
	return robot
}

func (robot *fakeRobot) address() string { return strings.TrimPrefix(robot.server.URL, "http://") }

func (robot *fakeRobot) now() (int64, time.Time) {
	wall := time.Now()
	return wall.Sub(robot.boot).Microseconds(), wall
}

func (robot *fakeRobot) serve(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"v4.1.networktables.first.wpi.edu"}})
	if err != nil {
		return
	}
	if ws.Subprotocol() == "" || !strings.HasPrefix(r.URL.Path, "/nt/") {
		ws.Close(websocket.StatusPolicyViolation, "NT4 subprotocol required")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	robot.mutex.Lock()
	robot.connections++
	robot.subscriptions = map[int]map[string]any{}
	robot.closeCurrent = func() { cancel(); ws.CloseNow() }
	robot.open = true
	robot.mutex.Unlock()
	defer func() {
		robot.mutex.Lock()
		robot.open = false
		robot.mutex.Unlock()
		cancel()
		ws.CloseNow()
	}()

	var writeMutex sync.Mutex
	write := func(typ websocket.MessageType, data []byte) error {
		writeMutex.Lock()
		defer writeMutex.Unlock()
		return ws.Write(ctx, typ, data)
	}
	announced := map[int]bool{}
	// Announces topics a current subscription covers that this connection hasn't been told about yet.
	announce := func() {
		var announcements []map[string]any
		robot.mutex.Lock()
		for _, topic := range robot.topics {
			if !announced[topic.id] && robot.matches(topic.name, false) {
				announced[topic.id] = true
				announcements = append(
					announcements, map[string]any{
						"method": "announce",
						"params": map[string]any{"name": topic.name, "id": topic.id, "type": topic.typ, "properties": map[string]any{}},
					},
				)
			}
		}
		robot.mutex.Unlock()
		if len(announcements) > 0 {
			body, _ := json.Marshal(announcements)
			_ = write(websocket.MessageText, body)
		}
	}
	robot.mutex.Lock()
	robot.announce = announce
	robot.mutex.Unlock()
	go robot.publish(ctx, write)

	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageBinary {
			decoder := msgpack.NewDecoder(bytes.NewReader(data))
			var message []any
			for decoder.Decode(&message) == nil {
				if id, _ := message[0].(int8); id == -1 {
					robotTime, _ := robot.now()
					reply, _ := msgpack.Marshal([]any{-1, robotTime, 2, message[3]})
					_ = write(websocket.MessageBinary, reply)
				}
			}
			continue
		}
		var messages []struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		require.Nil(robot.t, json.Unmarshal(data, &messages))
		robot.mutex.Lock()
		for _, message := range messages {
			robot.subscribeLog = append(robot.subscribeLog, fmt.Sprintf("%s %v", message.Method, message.Params))
			subuid := int(message.Params["subuid"].(float64))
			switch message.Method {
			case "subscribe":
				robot.subscriptions[subuid] = message.Params
			case "unsubscribe":
				delete(robot.subscriptions, subuid)
			}
		}
		robot.mutex.Unlock()
		announce()
	}
}

// Starts publishing a topic partway through a connection, as robot code does the first time it logs something.
func (robot *fakeRobot) publishLater(topic fakeTopic) {
	robot.mutex.Lock()
	robot.topics = append(robot.topics, topic)
	announce := robot.announce
	robot.mutex.Unlock()
	announce()
}

// Reports whether any current subscription covers the topic; valuesOnly skips topics-only subscriptions. Caller holds
// the mutex.
func (robot *fakeRobot) matches(name string, valuesOnly bool) bool {
	for _, subscription := range robot.subscriptions {
		options, _ := subscription["options"].(map[string]any)
		if valuesOnly && options["topicsonly"] == true {
			continue
		}
		for _, pattern := range subscription["topics"].([]any) {
			if options["prefix"] == true && strings.HasPrefix(name, pattern.(string)) || pattern == name {
				return true
			}
		}
	}
	return false
}

// Every 20 ms, sends the next sample of every value-subscribed topic in a single binary frame.
func (robot *fakeRobot) publish(ctx context.Context, write func(websocket.MessageType, []byte) error) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	sentOnce := map[int]bool{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		robotTime, wall := robot.now()
		var frame bytes.Buffer
		robot.mutex.Lock()
		robot.n++
		n := robot.n
		for _, topic := range robot.topics {
			if !robot.matches(topic.name, true) || topic.once && sentOnce[topic.id] {
				continue
			}
			sentOnce[topic.id] = true
			typeIndex, value := topic.value(n)
			message, _ := msgpack.Marshal([]any{topic.id, robotTime, typeIndex, value})
			frame.Write(message)
			robot.sent[topic.name] = append(robot.sent[topic.name], fakeSample{robotTime: robotTime, wallTime: wall, n: n})
		}
		robot.mutex.Unlock()
		if frame.Len() > 0 && write(websocket.MessageBinary, frame.Bytes()) != nil {
			return
		}
	}
}

// The value-subscribed topics and their options, as the robot currently sees them: exact topic names, or with
// prefixes, prefix subscriptions.
func (robot *fakeRobot) valueSubscriptions(prefixes bool) (topics []string, options []map[string]any) {
	robot.mutex.Lock()
	defer robot.mutex.Unlock()
	for _, subscription := range robot.subscriptions {
		subscriptionOptions, _ := subscription["options"].(map[string]any)
		if subscriptionOptions["topicsonly"] == true || (subscriptionOptions["prefix"] == true) != prefixes {
			continue
		}
		for _, topic := range subscription["topics"].([]any) {
			topics = append(topics, topic.(string))
		}
		options = append(options, subscriptionOptions)
	}
	sort.Strings(topics)
	return topics, options
}

func (robot *fakeRobot) state() (connections int, open bool) {
	robot.mutex.Lock()
	defer robot.mutex.Unlock()
	return robot.connections, robot.open
}

func (robot *fakeRobot) dropConnection() {
	robot.mutex.Lock()
	closeCurrent := robot.closeCurrent
	robot.mutex.Unlock()
	closeCurrent()
}

func (robot *fakeRobot) samples(topic string) []fakeSample {
	robot.mutex.Lock()
	defer robot.mutex.Unlock()
	return append([]fakeSample(nil), robot.sent[topic]...)
}

func pose2dBytes(x, y, rotation float64) []byte {
	data := make([]byte, 24)
	binary.LittleEndian.PutUint64(data[0:], math.Float64bits(x))
	binary.LittleEndian.PutUint64(data[8:], math.Float64bits(y))
	binary.LittleEndian.PutUint64(data[16:], math.Float64bits(rotation))
	return data
}

// The pose the fake robots report for sample n.
func fakePose(n int) (x, y, rotation float64) { return 1.5 + 0.01*float64(n), -2.25, 0.5 }

// A value read back from a WPILOG.
type wpilogValue struct {
	name      string
	typeName  string
	timestamp int64 // Microseconds since the recording started.
	payload   []byte
}

// Reads a WPILOG's values, independently of the writer, following the spec in allwpilib's datalog/doc/datalog.adoc.
func readWpilog(t *testing.T, path string) []wpilogValue {
	data, err := os.ReadFile(path)
	require.Nil(t, err)
	require.Equal(t, "WPILOG\x00\x01", string(data[:8]))
	data = data[12+binary.LittleEndian.Uint32(data[8:]):]
	next := func(length int) []byte {
		require.GreaterOrEqual(t, len(data), length, "truncated record")
		field := data[:length]
		data = data[length:]
		return field
	}
	number := func(length int) uint64 {
		var value uint64
		for i, b := range next(length) {
			value |= uint64(b) << (8 * i)
		}
		return value
	}
	text := func() string { return string(next(int(number(4)))) }
	entries := map[uint64][2]string{}
	var values []wpilogValue
	for len(data) > 0 {
		lengths := next(1)[0]
		entryId := number(int(lengths&3) + 1)
		size := number(int(lengths>>2&3) + 1)
		timestamp := int64(number(int(lengths>>4&7) + 1))
		payload := next(int(size))
		if entryId != 0 {
			require.Contains(t, entries, entryId)
			values = append(values, wpilogValue{entries[entryId][0], entries[entryId][1], timestamp, payload})
			continue
		}
		rest := data
		data = payload
		if control := next(1)[0]; control == 0 {
			id := number(4)
			entries[id] = [2]string{text(), text()}
		} else {
			require.Equal(t, byte(1), control, "only start and finish are written")
			delete(entries, number(4))
		}
		data = rest
	}
	return values
}

// The values of one entry.
func wpilogEntry(values []wpilogValue, name string) []wpilogValue {
	var entry []wpilogValue
	for _, value := range values {
		if value.name == name {
			entry = append(entry, value)
		}
	}
	return entry
}

func float64At(payload []byte, i int) float64 {
	return math.Float64frombits(binary.LittleEndian.Uint64(payload[8*i:]))
}

// Reads the stations a recording session noted for its teams, by team number.
func readStationsFile(t *testing.T, sessionDir string) map[string]string {
	data, err := os.ReadFile(filepath.Join(sessionDir, field.RecordingStationsFile))
	require.Nil(t, err)
	var stations map[string]string
	require.Nil(t, json.Unmarshal(data, &stations), string(data))
	return stations
}

// Lists a recording session's files, relative to it.
func sessionFiles(t *testing.T, sessionDir string) []string {
	var files []string
	require.Nil(
		t, filepath.Walk(
			sessionDir, func(path string, info os.FileInfo, err error) error {
				if err == nil && !info.IsDir() {
					relative, _ := filepath.Rel(sessionDir, path)
					files = append(files, filepath.ToSlash(relative))
				}
				return err
			},
		),
	)
	return files
}

func TestTelemetryEndToEnd(t *testing.T) {
	var transcript strings.Builder
	note := func(format string, args ...any) { fmt.Fprintf(&transcript, format+"\n", args...) }
	outputDir := filepath.Join("..", "e2e-output")
	t.Cleanup(func() { _ = os.WriteFile(filepath.Join(outputDir, "telemetry.txt"), []byte(transcript.String()), 0644) })
	logTail := NewLogTail(200)
	log.SetOutput(io.MultiWriter(os.Stderr, logTail))
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	const (
		odometry   = "/AdvantageKit/RealOutputs/Odometry/Robot"
		state      = "/AdvantageKit/RealOutputs/Superstructure/State"
		field2d    = "/SmartDashboard/Field/Robot"
		modules    = "/AdvantageKit/RealOutputs/Drive/ModuleStates"
		shooterRpm = "/SmartDashboard/Shooter RPM"
	)
	poseTopic := func(id int, name string) fakeTopic {
		return fakeTopic{id: id, name: name, typ: "struct:Pose2d", value: func(n int) (int, any) {
			return 5, pose2dBytes(fakePose(n))
		}}
	}
	robot254 := newFakeRobot(
		t, slices.Concat(
			[]fakeTopic{
				poseTopic(1, odometry),
				{id: 2, name: state, typ: "string", value: func(n int) (int, any) { return 4, "INTAKING" }},
				{id: 3, name: field2d, typ: "double[]", value: func(n int) (int, any) {
					x, y, rotation := fakePose(n)
					return 17, []float64{x, y, rotation * 180 / math.Pi}
				}},
				{id: 4, name: modules, typ: "struct:SwerveModuleState[]", value: func(n int) (int, any) {
					return 5, make([]byte, 64)
				}},
				{id: 5, name: shooterRpm, typ: "double", value: func(n int) (int, any) { return 1, 4180.5 }},
			},
			// WPILib publishes the Pose2d schemas; AdvantageKit keeps the swerve module's in its own table.
			fakeSchemas(10, "", pose2dSchemas...),
			fakeSchemas(20, "/AdvantageKit", [2]string{"SwerveModuleState", "double speed;Rotation2d angle"}),
		),
	)
	robot1114 := newFakeRobot(t, append([]fakeTopic{poseTopic(7, "/Pose")}, fakeSchemas(10, "", pose2dSchemas...)...))
	addresses := map[int]string{254: robot254.address(), 1114: robot1114.address()}

	apServer := httptest.NewServer(&fakeAccessPoint{})
	defer apServer.Close()
	dbPath := filepath.Join(t.TempDir(), "event.db")
	database, err := model.OpenDatabase(dbPath)
	require.Nil(t, err)
	recordingsDir := filepath.Join(t.TempDir(), "recordings")
	fieldController, err := field.New(
		database, field.Options{
			DriverStationPorts: freeDriverStationPorts(t),
			// 971 has no robot at all; its connection is refused.
			NtAddress: func(teamId int) string {
				if address, ok := addresses[teamId]; ok {
					return address
				}
				return "127.0.0.1:1"
			},
			RecordingsDir: recordingsDir,
		},
	)
	require.Nil(t, err)
	server := httptest.NewServer(NewWeb(fieldController, logTail).newHandler())
	defer server.Close()

	call := func(method, path string, body any) (int, map[string]any) {
		var reader io.Reader
		requestJson := ""
		if body != nil {
			b, _ := json.Marshal(body)
			requestJson = string(b)
			reader = bytes.NewReader(b)
		}
		request, _ := http.NewRequest(method, server.URL+path, reader)
		response, err := http.DefaultClient.Do(request)
		require.Nil(t, err)
		defer response.Body.Close()
		responseJson, _ := io.ReadAll(response.Body)
		var decoded map[string]any
		_ = json.Unmarshal(responseJson, &decoded)
		summary := string(responseJson)
		if recording, ok := decoded["recording"]; ok {
			recordingJson, _ := json.Marshal(recording)
			summary = "recording=" + string(recordingJson)
		}
		note("== %s %s %s\n-> %d %s", method, path, requestJson, response.StatusCode, summary)
		return response.StatusCode, decoded
	}
	// Finds a team in GET /api/telemetry.
	telemetryTeam := func(teamId int) map[string]any {
		_, body := call("GET", "/api/telemetry", nil)
		for _, team := range body["teams"].([]any) {
			if team := team.(map[string]any); team["teamId"] == float64(teamId) {
				return team
			}
		}
		return nil
	}
	topicStatus := func(team map[string]any, name string) map[string]any {
		for _, topic := range team["topics"].([]any) {
			if topic := topic.(map[string]any); topic["name"] == name {
				return topic
			}
		}
		return nil
	}

	_, settings := call("GET", "/api/settings", nil)
	settings["ApAddress"] = strings.TrimPrefix(apServer.URL, "http://")
	status, _ := call("PUT", "/api/settings", settings)
	require.Equal(t, 200, status)
	empty := field.Assignment{}
	assign := func(r1, r2, b2 int) {
		assignment := func(teamId int) field.Assignment {
			if teamId == 0 {
				return empty
			}
			return field.Assignment{TeamId: teamId, WpaKey: fmt.Sprintf("key%dkey", teamId)}
		}
		status, _ := call(
			"PUT", "/api/stations",
			[6]field.Assignment{assignment(r1), assignment(r2), empty, empty, assignment(b2), empty},
		)
		require.Equal(t, 200, status)
	}
	assign(254, 971, 1114)

	// Whether each topic a team's robot publishes can be recorded usefully, by name.
	supportedTopics := func(team map[string]any) map[string]bool {
		supported := map[string]bool{}
		for _, topic := range team["available"].([]any) {
			topic := topic.(map[string]any)
			supported[topic["name"].(string)] = topic["supported"].(bool)
		}
		return supported
	}

	// (1, 12) Connected robots list their topics, schemas included, and every type is supported in a WPILOG, the
	// default format. 971's robot can't be reached.
	var team254 map[string]any
	require.Eventually(
		t, func() bool {
			team254 = telemetryTeam(254)
			return team254 != nil && team254["connection"] == "connected" && len(team254["available"].([]any)) == 9
		}, 5*time.Second, 100*time.Millisecond,
	)
	for name, supported := range supportedTopics(team254) {
		assert.True(t, supported, name)
	}
	assert.Equal(t, "R1", team254["station"])
	assert.Equal(t, "disconnected", telemetryTeam(971)["connection"])
	topics, _ := robot254.valueSubscriptions(false)
	assert.Empty(t, topics, "nothing configured, so no values are subscribed")
	prefixes, _ := robot254.valueSubscriptions(true)
	assert.Equal(t, []string{"/.schema/", "/AdvantageKit/.schema/"}, prefixes, "schemas are always subscribed")

	// Bad requests are rejected.
	status, _ = call("PUT", "/api/teams/0/topics", map[string]any{"topics": []string{odometry}})
	assert.Equal(t, 400, status)
	status, _ = call("PUT", "/api/teams/254/topics", map[string]any{"topics": "nope"})
	assert.Equal(t, 400, status)

	// (2, 3, 11) Configuring topics trims and dedupes them, drops blanks (an exclusion of nothing too), saves them to
	// the team, and subscribes to every value at the robot's full rate.
	status, body := call(
		"PUT", "/api/teams/254/topics",
		map[string]any{"topics": []string{odometry, " " + state + " ", odometry, "", " ! "}},
	)
	require.Equal(t, 200, status)
	require.NotNil(t, body["teams"])
	status, _ = call("PUT", "/api/teams/1114/topics", map[string]any{"topics": []string{"/Pose"}})
	require.Equal(t, 200, status)
	require.Eventually(
		t, func() bool {
			topics, _ := robot254.valueSubscriptions(false)
			return fmt.Sprint(topics) == fmt.Sprint([]string{odometry, state})
		}, 3*time.Second, 50*time.Millisecond,
	)
	_, options := robot254.valueSubscriptions(false)
	for _, option := range options {
		assert.Equal(t, 0.02, option["periodic"])
		assert.Equal(t, true, option["all"])
	}

	// (4, 6) Values come through at the robot's rate, decoded.
	require.Eventually(
		t, func() bool {
			team254 = telemetryTeam(254)
			topic := topicStatus(team254, odometry)
			return topic != nil && topic["hz"].(float64) >= 40
		}, 5*time.Second, 200*time.Millisecond,
	)
	odometryStatus := topicStatus(team254, odometry)
	assert.LessOrEqual(t, odometryStatus["hz"].(float64), 60.0)
	assert.Equal(t, "struct:Pose2d", odometryStatus["type"])
	assert.Regexp(t, `^\d+\.\d\d, -2\.25, 28\.6°$`, odometryStatus["last"])
	assert.Equal(t, `"INTAKING"`, topicStatus(team254, state)["last"])

	// (10) Record. Mid-recording (7) add a topic and (8) drop 254's connection; both should keep recording.
	status, body = call("PUT", "/api/recording", map[string]any{"active": true})
	require.Equal(t, 200, status)
	recording := body["recording"].(map[string]any)
	require.Equal(t, true, recording["active"])
	sessionDir := recording["folder"].(string)
	assert.True(t, strings.HasPrefix(sessionDir, recordingsDir), sessionDir)
	recordStart := time.Now()
	time.Sleep(800 * time.Millisecond)

	status, _ = call(
		"PUT", "/api/teams/254/topics", map[string]any{"topics": []string{odometry, state, field2d, modules}},
	)
	require.Equal(t, 200, status)
	require.Eventually(
		t, func() bool {
			topics, _ := robot254.valueSubscriptions(false)
			return fmt.Sprint(topics) == fmt.Sprint([]string{modules, odometry, state, field2d})
		}, 3*time.Second, 50*time.Millisecond,
	)
	time.Sleep(500 * time.Millisecond)

	robot254.dropConnection()
	require.Eventually(
		t, func() bool {
			connections, open := robot254.state()
			topics, _ := robot254.valueSubscriptions(false)
			return connections == 2 && open && len(topics) == 4
		}, 5*time.Second, 50*time.Millisecond,
	)
	time.Sleep(800 * time.Millisecond)
	status, body = call("PUT", "/api/recording", map[string]any{"active": false})
	require.Equal(t, 200, status)
	assert.Equal(t, false, body["recording"].(map[string]any)["active"])
	recordEnd := time.Now()

	// One WPILOG per team, and (17) the station each was on.
	assert.Equal(t, []string{"1114.wpilog", "254.wpilog", "stations.json"}, sessionFiles(t, sessionDir))
	assert.Equal(t, map[string]string{"1114": "B2", "254": "R1"}, readStationsFile(t, sessionDir))
	values := readWpilog(t, filepath.Join(sessionDir, "254.wpilog"))

	// (12) The schemas, sent before recording started, are in the file, so AdvantageScope can decode the structs.
	for _, name := range []string{
		"NT:/.schema/struct:Pose2d", "NT:/.schema/struct:Translation2d", "NT:/.schema/struct:Rotation2d",
		"NT:/AdvantageKit/.schema/struct:SwerveModuleState",
	} {
		schema := wpilogEntry(values, name)
		require.NotEmpty(t, schema, name)
		assert.Equal(t, "structschema", schema[0].typeName)
	}
	assert.Equal(t, "double value", string(wpilogEntry(values, "NT:/.schema/struct:Rotation2d")[0].payload))
	assert.Empty(t, wpilogEntry(values, "NT:"+shooterRpm), "not configured")

	// (5, 6, 10) Every recorded pose matches a sample the robot sent, at a time within a few ms of when the robot sent
	// it, and they cover the whole recording apart from the reconnect gap.
	systemTime := wpilogEntry(values, "systemTime")
	require.Len(t, systemTime, 1)
	recordingEpoch := int64(binary.LittleEndian.Uint64(systemTime[0].payload)) - systemTime[0].timestamp
	poses := wpilogEntry(values, "NT:"+odometry)
	require.NotEmpty(t, poses)
	note("== 254 odometry: %d values, the first at %d us", len(poses), poses[0].timestamp)
	sent := map[int]fakeSample{}
	for _, sample := range robot254.samples(odometry) {
		sent[sample.n] = sample
	}
	var worstSyncError time.Duration
	previousN, gaps := 0, 0
	for i, pose := range poses {
		require.Equal(t, "struct:Pose2d", pose.typeName)
		require.Len(t, pose.payload, 24)
		x, y, rotation := float64At(pose.payload, 0), float64At(pose.payload, 1), float64At(pose.payload, 2)
		n := int(math.Round((x - 1.5) / 0.01))
		sample, ok := sent[n]
		require.True(t, ok, "pose %v doesn't match a sample the robot sent", []float64{x, y, rotation})
		expectedX, expectedY, expectedRotation := fakePose(n)
		assert.Equal(t, []float64{expectedX, expectedY, expectedRotation}, []float64{x, y, rotation})
		worstSyncError = max(worstSyncError, time.UnixMicro(recordingEpoch+pose.timestamp).Sub(sample.wallTime).Abs())
		assert.Greater(t, n, previousN)
		if i > 0 && pose.timestamp-poses[i-1].timestamp > 60_000 {
			gaps++
		}
		previousN = n
	}
	note("== Worst clock conversion error %s, %d gap(s) over 60 ms", worstSyncError, gaps)
	assert.Less(t, worstSyncError, 10*time.Millisecond)
	assert.LessOrEqual(t, gaps, 1, "only the reconnect should leave a gap")
	expectedValues := int(recordEnd.Sub(recordStart) / (20 * time.Millisecond))
	assert.Greater(t, len(poses), expectedValues*3/4, "about 50 a second")

	fieldPoses := wpilogEntry(values, "NT:"+field2d)
	require.Greater(t, len(fieldPoses), 10)
	assert.Equal(t, "double[]", fieldPoses[0].typeName)
	require.Len(t, fieldPoses[0].payload, 24)
	assert.Equal(t, -2.25, float64At(fieldPoses[0].payload, 1))
	assert.InDelta(t, 28.648, float64At(fieldPoses[0].payload, 2), 0.001)
	states := wpilogEntry(values, "NT:"+state)
	require.NotEmpty(t, states)
	assert.Equal(t, "string", states[0].typeName)
	assert.Equal(t, "INTAKING", string(states[0].payload))
	moduleStates := wpilogEntry(values, "NT:"+modules)
	require.NotEmpty(t, moduleStates)
	assert.Equal(t, "struct:SwerveModuleState[]", moduleStates[0].typeName)
	assert.Len(t, moduleStates[0].payload, 64)
	poses = wpilogEntry(readWpilog(t, filepath.Join(sessionDir, "1114.wpilog")), "NT:/Pose")
	assert.Greater(t, len(poses), expectedValues*3/4)

	_ = os.RemoveAll(filepath.Join(outputDir, "recording"))
	require.Nil(t, os.CopyFS(filepath.Join(outputDir, "recording", "wpilog"), os.DirFS(sessionDir)))

	// (14, 15) A saved section records everything under it, including topics the robot starts publishing later. A "!"
	// excludes a topic or a smaller section, and a topic saved by name still records inside an excluded section.
	const (
		realOutputs = "/AdvantageKit/RealOutputs/"
		driveOutput = "/AdvantageKit/RealOutputs/Drive/"
		visionPose  = "/AdvantageKit/RealOutputs/Odometry/VisionPose"
		rotation    = "/AdvantageKit/RealOutputs/Drive/Rotation"
	)
	status, _ = call(
		"PUT", "/api/teams/254/topics",
		map[string]any{"topics": []string{realOutputs, "! " + driveOutput, modules, "!" + state, field2d}},
	)
	require.Equal(t, 200, status)
	assert.Equal(
		t, fmt.Sprint([]string{realOutputs, "!" + driveOutput, modules, "!" + state, field2d}),
		fmt.Sprint(telemetryTeam(254)["entries"]), "saved as entered, without the space after the !",
	)
	require.Eventually(
		t, func() bool {
			names, _ := robot254.valueSubscriptions(false)
			prefixes, _ := robot254.valueSubscriptions(true)
			return fmt.Sprint(names) == fmt.Sprint([]string{modules, field2d}) &&
				fmt.Sprint(prefixes) == fmt.Sprint([]string{"/.schema/", "/AdvantageKit/.schema/", realOutputs})
		}, 3*time.Second, 50*time.Millisecond, "names exactly, sections by prefix, exclusions not at all",
	)
	status, body = call("PUT", "/api/recording", map[string]any{"active": true})
	require.Equal(t, 200, status)
	sectionsDir := body["recording"].(map[string]any)["folder"].(string)
	time.Sleep(500 * time.Millisecond)
	robot254.publishLater(poseTopic(8, visionPose))
	robot254.publishLater(
		fakeTopic{id: 9, name: rotation, typ: "struct:Rotation2d", value: func(n int) (int, any) {
			return 5, make([]byte, 8)
		}},
	)
	require.Eventually(
		t, func() bool {
			topic := topicStatus(telemetryTeam(254), visionPose)
			return topic != nil && topic["hz"].(float64) >= 40
		}, 5*time.Second, 200*time.Millisecond, "a topic published later under a saved section",
	)
	var recordedNames []string
	for _, topic := range telemetryTeam(254)["topics"].([]any) {
		recordedNames = append(recordedNames, topic.(map[string]any)["name"].(string))
	}
	assert.Equal(
		t, []string{modules, field2d, odometry, visionPose}, recordedNames,
		"saved by name first, then what the section picks up",
	)
	time.Sleep(300 * time.Millisecond)
	status, _ = call("PUT", "/api/recording", map[string]any{"active": false})
	require.Equal(t, 200, status)
	values = readWpilog(t, filepath.Join(sectionsDir, "254.wpilog"))
	for _, name := range []string{odometry, visionPose, modules, field2d} {
		assert.NotEmpty(t, wpilogEntry(values, "NT:"+name), name)
	}
	for _, name := range []string{state, rotation, shooterRpm} {
		assert.Empty(t, wpilogEntry(values, "NT:"+name), name)
	}
	assert.NotEmpty(t, robot254.samples(rotation), "the robot sent the excluded topic; the recorder dropped it")
	note("== Sections recording: %d values of %s", len(wpilogEntry(values, "NT:"+visionPose)), visionPose)
	require.Nil(t, os.CopyFS(filepath.Join(outputDir, "recording", "sections"), os.DirFS(sectionsDir)))

	// (13) An unknown format is rejected. A CSV is in AdvantageScope's layout, and types it can't hold are unsupported.
	_, settings = call("GET", "/api/settings", nil)
	assert.Equal(t, "wpilog", settings["RecordingFormat"])
	settings["RecordingFormat"] = "parquet"
	status, _ = call("PUT", "/api/settings", settings)
	assert.Equal(t, 400, status)
	settings["RecordingFormat"] = "csv"
	status, _ = call("PUT", "/api/settings", settings)
	require.Equal(t, 200, status)
	supported := supportedTopics(telemetryTeam(254))
	assert.True(t, supported[odometry])
	assert.False(t, supported[modules])
	assert.False(t, supported["/.schema/struct:Pose2d"])

	// (16) Drive's section picks up Rotation, which a CSV can't hold, alongside ModuleStates, saved by name.
	status, _ = call(
		"PUT", "/api/teams/254/topics", map[string]any{"topics": []string{odometry, state, field2d, modules, driveOutput}},
	)
	require.Equal(t, 200, status)
	require.Eventually(
		t, func() bool {
			prefixes, _ := robot254.valueSubscriptions(true)
			return slices.Contains(prefixes, driveOutput)
		}, 3*time.Second, 50*time.Millisecond,
	)
	assert.Nil(t, topicStatus(telemetryTeam(254), rotation), "not listed as recorded in a CSV")

	// (17) 254 and 1114 swap alliances for this session, then swap back partway through it.
	assign(1114, 971, 254)
	status, body = call("PUT", "/api/recording", map[string]any{"active": true})
	require.Equal(t, 200, status)
	recording = body["recording"].(map[string]any)
	assert.Equal(t, "csv", recording["format"])
	csvDir := recording["folder"].(string)
	recordStart = time.Now()
	require.Eventually(
		t, func() bool { return len(sessionFiles(t, csvDir)) == 3 }, 3*time.Second, 50*time.Millisecond,
		"both teams' files and stations.json",
	)
	assign(254, 971, 1114)
	time.Sleep(time.Second - time.Since(recordStart))
	status, _ = call("PUT", "/api/recording", map[string]any{"active": false})
	require.Equal(t, 200, status)
	expectedValues = int(time.Since(recordStart) / (20 * time.Millisecond))
	assert.Equal(t, []string{"1114.csv", "254.csv", "stations.json"}, sessionFiles(t, csvDir))
	assert.Equal(t, map[string]string{"1114": "R1", "254": "B2"}, readStationsFile(t, csvDir))
	csvData, err := os.ReadFile(filepath.Join(csvDir, "254.csv"))
	require.Nil(t, err)
	lines := strings.Split(strings.TrimSuffix(string(csvData), "\n"), "\n")
	note("== 254.csv: %d lines, starting %q", len(lines), lines[:min(7, len(lines))])
	require.Equal(t, "Timestamp,Key,Value", lines[0])
	rowsByKey := map[string][]string{}
	for _, line := range lines[1:] {
		// AdvantageScope splits rows on every comma and needs exactly three columns.
		columns := strings.Split(line, ",")
		require.Len(t, columns, 3, line)
		_, err := strconv.ParseFloat(columns[0], 64)
		require.Nil(t, err, line)
		rowsByKey[columns[1]] = append(rowsByKey[columns[1]], columns[2])
	}
	assert.Len(t, rowsByKey["systemTime"], 1)
	assert.Greater(t, len(rowsByKey["NT:"+odometry+"/translation/x"]), expectedValues*3/4)
	for _, field := range []string{"/translation/y", "/rotation/value"} {
		assert.Len(t, rowsByKey["NT:"+odometry+field], len(rowsByKey["NT:"+odometry+"/translation/x"]))
	}
	assert.Equal(t, "-2.25", rowsByKey["NT:"+odometry+"/translation/y"][0])
	assert.Equal(t, `"INTAKING"`, rowsByKey["NT:"+state][0])
	assert.Regexp(t, `^[[][0-9.]+; -2[.]25; 28[.]6[0-9]*[]]$`, rowsByKey["NT:"+field2d][0])
	assert.Regexp(t, `^(00-){63}00$`, rowsByKey["NT:"+modules][0], "undecoded structs saved by name are hex bytes")
	assert.Empty(t, rowsByKey["NT:"+rotation], "a section skips types a CSV can't hold")
	for key := range rowsByKey {
		assert.NotContains(t, key, ".schema", "a CSV has no use for schemas")
	}
	require.Nil(t, os.CopyFS(filepath.Join(outputDir, "recording", "csv"), os.DirFS(csvDir)))

	// (9) Taking 254 off the field closes its connection for good; its topics stay configured.
	assign(0, 971, 1114)
	require.Eventually(
		t, func() bool {
			_, open := robot254.state()
			return !open
		}, 3*time.Second, 50*time.Millisecond,
	)
	time.Sleep(1500 * time.Millisecond)
	connections, open := robot254.state()
	assert.Equal(t, 2, connections)
	assert.False(t, open)
	team254 = telemetryTeam(254)
	assert.Equal(t, "offField", team254["connection"])
	assert.Equal(t, "", team254["station"])
	assert.Len(t, team254["topics"], 4)

	// (11) The topics are on the team record, next to its WPA key.
	require.Nil(t, database.Close())
	db, err := bbolt.Open(dbPath, 0644, nil)
	require.Nil(t, err)
	require.Nil(
		t, db.View(
			func(tx *bbolt.Tx) error {
				record := string(tx.Bucket([]byte("Team")).Get([]byte("254")))
				note("== Team/254 %s", record)
				assert.Contains(t, record, `"WpaKey":"key254key"`)
				assert.Contains(
					t, record,
					fmt.Sprintf(`"NtTopics":["%s","%s","%s","%s","%s"]`, odometry, state, field2d, modules, driveOutput),
				)
				return nil
			},
		),
	)
	db.Close()

	robot254.mutex.Lock()
	note("== 254 subscription messages\n%s", strings.Join(robot254.subscribeLog, "\n"))
	robot254.mutex.Unlock()
	note("== Log tail\n%s", strings.Join(logTail.Lines(), "\n"))
}
