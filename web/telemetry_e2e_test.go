// End-to-end test of NetworkTables recording: the real HTTP API, field controller and NT4 client against fake robots
// that speak NT4 over a real WebSocket. Writes a transcript to e2e-output/telemetry.txt and copies the recorded CSVs to
// e2e-output/recording/ for inspection.
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
// 10. Stopping a recording loses buffered rows, or file names and headers are wrong.
// 11. Topics are saved in a way that loses the team's other fields.

package web

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/csv"
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
}

// A sample the fake robot sent: its robot timestamp and the local wall time it corresponds to.
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
	connections   int
	subscriptions map[int]map[string]any // Current subscriptions on the current connection, by subuid.
	subscribeLog  []string
	sent          map[string][]fakeSample
	closeCurrent  func()
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
		var announcements []map[string]any
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
	n := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		n++
		robotTime, wall := robot.now()
		var frame bytes.Buffer
		robot.mutex.Lock()
		for _, topic := range robot.topics {
			if !robot.matches(topic.name, true) {
				continue
			}
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

// The value-subscribed topics and their options, as the robot currently sees them.
func (robot *fakeRobot) valueSubscriptions() (topics []string, options []map[string]any) {
	robot.mutex.Lock()
	defer robot.mutex.Unlock()
	for _, subscription := range robot.subscriptions {
		subscriptionOptions, _ := subscription["options"].(map[string]any)
		if subscriptionOptions["topicsonly"] == true {
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

func readCsv(t *testing.T, path string) [][]string {
	file, err := os.Open(path)
	require.Nil(t, err)
	defer file.Close()
	rows, err := csv.NewReader(file).ReadAll()
	require.Nil(t, err)
	return rows
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
	robot254 := newFakeRobot(
		t, []fakeTopic{
			{1, odometry, "struct:Pose2d", func(n int) (int, any) { return 5, pose2dBytes(fakePose(n)) }},
			{2, state, "string", func(n int) (int, any) { return 4, "INTAKING" }},
			{3, field2d, "double[]", func(n int) (int, any) {
				x, y, rotation := fakePose(n)
				return 17, []float64{x, y, rotation * 180 / math.Pi}
			}},
			{4, modules, "struct:SwerveModuleState[]", func(n int) (int, any) { return 5, make([]byte, 64) }},
			{5, shooterRpm, "double", func(n int) (int, any) { return 1, 4180.5 }},
		},
	)
	robot1114 := newFakeRobot(
		t, []fakeTopic{{7, "/Pose", "struct:Pose2d", func(n int) (int, any) { return 5, pose2dBytes(fakePose(n)) }}},
	)
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

	// (1) Connected robots list their topics; decodable types are marked. 971's robot can't be reached.
	var team254 map[string]any
	require.Eventually(
		t, func() bool {
			team254 = telemetryTeam(254)
			return team254 != nil && team254["connection"] == "connected" && len(team254["available"].([]any)) == 5
		}, 5*time.Second, 100*time.Millisecond,
	)
	supported := map[string]bool{}
	for _, topic := range team254["available"].([]any) {
		topic := topic.(map[string]any)
		supported[topic["name"].(string)] = topic["supported"].(bool)
	}
	assert.Equal(
		t, map[string]bool{odometry: true, state: true, field2d: true, modules: false, shooterRpm: true}, supported,
	)
	assert.Equal(t, "R1", team254["station"])
	assert.Equal(t, "disconnected", telemetryTeam(971)["connection"])
	topics, _ := robot254.valueSubscriptions()
	assert.Empty(t, topics, "nothing configured, so no values are subscribed")

	// Bad requests are rejected.
	status, _ = call("PUT", "/api/teams/0/topics", map[string]any{"topics": []string{odometry}})
	assert.Equal(t, 400, status)
	status, _ = call("PUT", "/api/teams/254/topics", map[string]any{"topics": "nope"})
	assert.Equal(t, 400, status)

	// (2, 3, 11) Configuring topics trims and dedupes them, saves them to the team, and subscribes to every value at
	// the robot's full rate.
	status, body := call(
		"PUT", "/api/teams/254/topics", map[string]any{"topics": []string{odometry, " " + state + " ", odometry, ""}},
	)
	require.Equal(t, 200, status)
	require.NotNil(t, body["teams"])
	status, _ = call("PUT", "/api/teams/1114/topics", map[string]any{"topics": []string{"/Pose"}})
	require.Equal(t, 200, status)
	require.Eventually(
		t, func() bool {
			topics, _ := robot254.valueSubscriptions()
			return fmt.Sprint(topics) == fmt.Sprint([]string{odometry, state})
		}, 3*time.Second, 50*time.Millisecond,
	)
	_, options := robot254.valueSubscriptions()
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

	status, _ = call("PUT", "/api/teams/254/topics", map[string]any{"topics": []string{odometry, state, field2d}})
	require.Equal(t, 200, status)
	require.Eventually(
		t, func() bool {
			topics, _ := robot254.valueSubscriptions()
			return fmt.Sprint(topics) == fmt.Sprint([]string{odometry, state, field2d})
		}, 3*time.Second, 50*time.Millisecond,
	)
	time.Sleep(500 * time.Millisecond)

	robot254.dropConnection()
	require.Eventually(
		t, func() bool {
			connections, open := robot254.state()
			topics, _ := robot254.valueSubscriptions()
			return connections == 2 && open && len(topics) == 3
		}, 5*time.Second, 50*time.Millisecond,
	)
	time.Sleep(800 * time.Millisecond)
	status, body = call("PUT", "/api/recording", map[string]any{"active": false})
	require.Equal(t, 200, status)
	assert.Equal(t, false, body["recording"].(map[string]any)["active"])
	recordEnd := time.Now()

	// Exactly the configured topics were written, one file per topic.
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
	assert.Equal(
		t, []string{
			"1114/Pose.csv", "254/AdvantageKit_RealOutputs_Odometry_Robot.csv",
			"254/AdvantageKit_RealOutputs_Superstructure_State.csv", "254/SmartDashboard_Field_Robot.csv",
		}, files,
	)

	// (5, 6, 10) Every recorded pose row matches a sample the robot sent, with its robot time converted to within a few
	// ms of when the robot sent it, and rows cover the whole recording apart from the reconnect gap.
	rows := readCsv(t, filepath.Join(sessionDir, "254", "AdvantageKit_RealOutputs_Odometry_Robot.csv"))
	note("== 254 odometry: %d rows, first %v", len(rows)-1, rows[:min(4, len(rows))])
	require.Equal(t, []string{"unix_time_us", "robot_time_us", "x", "y", "rotation"}, rows[0])
	sent := map[int64]fakeSample{}
	for _, sample := range robot254.samples(odometry) {
		sent[sample.robotTime] = sample
	}
	var worstSyncError time.Duration
	var previousRobotTime int64
	gaps := 0
	for _, row := range rows[1:] {
		unixTime, _ := strconv.ParseInt(row[0], 10, 64)
		robotTime, _ := strconv.ParseInt(row[1], 10, 64)
		sample, ok := sent[robotTime]
		require.True(t, ok, "row %v doesn't match a sample the robot sent", row)
		x, y, rotation := fakePose(sample.n)
		assert.Equal(t, []string{fmt.Sprint(x), fmt.Sprint(y), fmt.Sprint(rotation)}, row[2:])
		syncError := time.UnixMicro(unixTime).Sub(sample.wallTime).Abs()
		worstSyncError = max(worstSyncError, syncError)
		assert.Greater(t, robotTime, previousRobotTime)
		if previousRobotTime != 0 && robotTime-previousRobotTime > 60_000 {
			gaps++
		}
		previousRobotTime = robotTime
	}
	note("== Worst clock conversion error %s, %d gap(s) over 60 ms", worstSyncError, gaps)
	assert.Less(t, worstSyncError, 10*time.Millisecond)
	assert.LessOrEqual(t, gaps, 1, "only the reconnect should leave a gap")
	expectedRows := int(recordEnd.Sub(recordStart) / (20 * time.Millisecond))
	assert.Greater(t, len(rows)-1, expectedRows*3/4, "about 50 rows a second")

	rows = readCsv(t, filepath.Join(sessionDir, "254", "SmartDashboard_Field_Robot.csv"))
	require.Equal(t, []string{"unix_time_us", "robot_time_us", "value"}, rows[0])
	require.Greater(t, len(rows), 10)
	assert.Regexp(t, `^\[\d+\.\d+,-2\.25,28\.6`, rows[1][2])
	rows = readCsv(t, filepath.Join(sessionDir, "254", "AdvantageKit_RealOutputs_Superstructure_State.csv"))
	assert.Equal(t, "INTAKING", rows[1][2])
	rows = readCsv(t, filepath.Join(sessionDir, "1114", "Pose.csv"))
	assert.Greater(t, len(rows), expectedRows*3/4)

	_ = os.RemoveAll(filepath.Join(outputDir, "recording"))
	require.Nil(t, os.CopyFS(filepath.Join(outputDir, "recording"), os.DirFS(sessionDir)))

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
	assert.Len(t, team254["topics"], 3)

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
				assert.Contains(t, record, fmt.Sprintf(`"NtTopics":["%s","%s","%s"]`, odometry, state, field2d))
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
