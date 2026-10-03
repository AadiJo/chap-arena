// End-to-end test of the FMS toggle: the real HTTP API and field controller against fake driver stations that do the
// real TCP handshake and read the real UDP control packets. Writes a transcript of every request, handshake and
// control packet state to e2e-output/driver-stations.txt for inspection.

package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/Team254/cheesy-arena/field"
	"github.com/Team254/cheesy-arena/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Plays a new-style driver station: connects over TCP, reads its station assignment, then receives control packets on
// its own UDP port and can send status packets back.
type fakeDriverStation struct {
	t          *testing.T
	teamId     int
	tcp        net.Conn
	udp        *net.UDPConn
	assignment []byte // [0, 6, 31, station, status, flags, team high, team low]
}

func connectFakeDriverStation(t *testing.T, ports field.DriverStationPorts, teamId int) *fakeDriverStation {
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.Nil(t, err)
	tcp, err := net.Dial("tcp4", fmt.Sprintf("127.0.0.1:%d", ports.Tcp))
	require.Nil(t, err)
	t.Cleanup(func() { tcp.Close(); udp.Close() })

	// Tag 30: the UDP port to send control packets to, flags, then the team number in ASCII.
	udpPort := udp.LocalAddr().(*net.UDPAddr).Port
	team := strconv.Itoa(teamId)
	body := append([]byte{30, byte(udpPort >> 8), byte(udpPort), 0, byte(len(team))}, team...)
	_, err = tcp.Write(append([]byte{0, byte(len(body))}, body...))
	require.Nil(t, err)
	assignment := make([]byte, 8)
	require.Nil(t, tcp.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, err = io.ReadFull(tcp, assignment)
	require.Nil(t, err)
	return &fakeDriverStation{t: t, teamId: teamId, tcp: tcp, udp: udp, assignment: assignment}
}

// Reads control packets until one has the wanted enabled bit, failing after a few seconds. Returns that packet.
func (ds *fakeDriverStation) waitForEnabled(want bool) []byte {
	deadline := time.Now().Add(3 * time.Second)
	packet := make([]byte, 1500)
	for {
		require.Nil(ds.t, ds.udp.SetReadDeadline(deadline))
		count, err := ds.udp.Read(packet)
		require.Nil(ds.t, err, "team %d never got enabled=%t", ds.teamId, want)
		if (packet[3]&0x04 != 0) == want {
			return packet[:count]
		}
	}
}

// Collects control packets for a while and returns how many arrived and how many of those had the enabled bit.
func (ds *fakeDriverStation) countEnabled(duration time.Duration) (total, enabled int) {
	deadline := time.Now().Add(duration)
	packet := make([]byte, 1500)
	for {
		require.Nil(ds.t, ds.udp.SetReadDeadline(deadline))
		if _, err := ds.udp.Read(packet); err != nil {
			return total, enabled
		}
		total++
		if packet[3]&0x04 != 0 {
			enabled++
		}
	}
}

// Sends a UDP status packet saying the radio, roboRIO and robot are linked with the given battery voltage.
func (ds *fakeDriverStation) sendStatus(ports field.DriverStationPorts, battery float64) {
	conn, err := net.Dial("udp4", fmt.Sprintf("127.0.0.1:%d", ports.Udp))
	require.Nil(ds.t, err)
	defer conn.Close()
	whole := int(battery)
	fraction := int((battery - float64(whole)) * 256)
	_, err = conn.Write([]byte{0, 1, 0, 0x38, byte(ds.teamId >> 8), byte(ds.teamId), byte(whole), byte(fraction)})
	require.Nil(ds.t, err)
}

// Waits for FMS to close the TCP connection.
func (ds *fakeDriverStation) waitForClose() error {
	require.Nil(ds.t, ds.tcp.SetReadDeadline(time.Now().Add(3*time.Second)))
	_, err := ds.tcp.Read(make([]byte, 64))
	return err
}

// Picks free ports by briefly binding to them.
func freeDriverStationPorts(t *testing.T) field.DriverStationPorts {
	tcp, err := net.Listen("tcp4", ":0")
	require.Nil(t, err)
	defer tcp.Close()
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{})
	require.Nil(t, err)
	defer udp.Close()
	return field.DriverStationPorts{Tcp: tcp.Addr().(*net.TCPAddr).Port, Udp: udp.LocalAddr().(*net.UDPAddr).Port}
}

func TestDriverStationsEndToEnd(t *testing.T) {
	var transcript strings.Builder
	note := func(format string, args ...any) { fmt.Fprintf(&transcript, format+"\n", args...) }
	t.Cleanup(
		func() {
			outputDir := filepath.Join("..", "e2e-output")
			_ = os.MkdirAll(outputDir, 0755)
			_ = os.WriteFile(filepath.Join(outputDir, "driver-stations.txt"), []byte(transcript.String()), 0644)
		},
	)
	logTail := NewLogTail(200)
	log.SetOutput(io.MultiWriter(os.Stderr, logTail))
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	apServer := httptest.NewServer(&fakeAccessPoint{})
	defer apServer.Close()
	database, err := model.OpenDatabase(filepath.Join(t.TempDir(), "event.db"))
	require.Nil(t, err)
	defer database.Close()
	ports := freeDriverStationPorts(t)
	fieldController, err := field.New(database, field.Options{DriverStationPorts: ports, RecordingsDir: t.TempDir()})
	require.Nil(t, err)
	server := httptest.NewServer(NewWeb(fieldController, logTail).newHandler())
	defer server.Close()

	call := func(method, path string, body any) (int, map[string]any) {
		b, _ := json.Marshal(body)
		request, _ := http.NewRequest(method, server.URL+path, bytes.NewReader(b))
		response, err := http.DefaultClient.Do(request)
		require.Nil(t, err)
		defer response.Body.Close()
		responseJson, _ := io.ReadAll(response.Body)
		// Status responses are long; keep the transcript to the parts this test is about.
		var decoded map[string]any
		_ = json.Unmarshal(responseJson, &decoded)
		summary := string(responseJson)
		if stations, ok := decoded["stations"].([]any); ok {
			parts := []string{fmt.Sprintf("mode=%v", decoded["driverStationMode"])}
			for _, station := range stations {
				station := station.(map[string]any)
				ds, _ := json.Marshal(station["driverStation"])
				parts = append(parts, fmt.Sprintf("%v team=%v ds=%s", station["station"], station["assignment"].(map[string]any)["teamId"], ds))
			}
			summary = strings.Join(parts, "\n   ")
		}
		note("== %s %s %s\n-> %d %s", method, path, b, response.StatusCode, summary)
		return response.StatusCode, decoded
	}
	setMode := func(mode string) map[string]any {
		status, body := call("PUT", "/api/driver-stations", map[string]string{"mode": mode})
		require.Equal(t, 200, status)
		return body
	}
	stationStatus := func(body map[string]any, station int) map[string]any {
		return body["stations"].([]any)[station].(map[string]any)["driverStation"].(map[string]any)
	}
	dialFms := func() error {
		conn, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", ports.Tcp), time.Second)
		if err == nil {
			conn.Close()
		}
		return err
	}

	_, settings := call("GET", "/api/settings", nil)
	settings["ApAddress"] = strings.TrimPrefix(apServer.URL, "http://")
	status, _ := call("PUT", "/api/settings", settings)
	require.Equal(t, 200, status)
	empty := field.Assignment{}
	status, _ = call(
		"PUT", "/api/stations", [6]field.Assignment{
			{TeamId: 254, WpaKey: "cheesypoofs"}, empty, empty, empty, {TeamId: 1114, WpaKey: "simbotics"}, empty,
		},
	)
	require.Equal(t, 200, status)

	// Starts off: nothing listens, so driver stations stay in local mode.
	_, body := call("GET", "/api/status", nil)
	assert.Equal(t, "off", body["driverStationMode"])
	assert.NotNil(t, dialFms(), "FMS port should be closed while off")

	// An unknown mode is rejected.
	status, _ = call("PUT", "/api/driver-stations", map[string]string{"mode": "auto"})
	assert.Equal(t, 400, status)

	// Disabled: driver stations connect, get their station, and are told to stay disabled.
	setMode("disabled")
	red1 := connectFakeDriverStation(t, ports, 254)
	note("== Team 254 handshake reply %v", red1.assignment)
	assert.Equal(t, []byte{0, 6, 31, 0, 0, 0, 0, 254}, red1.assignment, "R1, status good")
	packet := red1.waitForEnabled(false)
	note("== Team 254 control packet %v", packet)
	assert.Equal(t, byte(0), packet[5], "station R1")

	// Status packets from the DS show up in the API.
	red1.sendStatus(ports, 12.5)
	assert.Eventually(
		t, func() bool {
			_, body = call("GET", "/api/status", nil)
			ds := stationStatus(body, 0)
			return ds["robotLinked"] == true && ds["batteryVoltage"] == 12.5
		}, 3*time.Second, 100*time.Millisecond,
	)

	// An unassigned team is told to wait.
	waiting := connectFakeDriverStation(t, ports, 9999)
	note("== Team 9999 handshake reply %v", waiting.assignment)
	assert.Equal(t, byte(2), waiting.assignment[4], "status waiting")

	// Enabled: the connected robot is enabled right away.
	body = setMode("enabled")
	assert.Equal(t, true, stationStatus(body, 0)["enabled"])
	packet = red1.waitForEnabled(true)
	note("== Team 254 control packet %v", packet)

	// A robot that connects while enabled stays disabled until Enable is pressed again.
	blue2 := connectFakeDriverStation(t, ports, 1114)
	note("== Team 1114 handshake reply %v", blue2.assignment)
	assert.Equal(t, byte(4), blue2.assignment[3], "station B2")
	total, enabled := blue2.countEnabled(1200 * time.Millisecond)
	note("== Team 1114 (late joiner) got %d control packets, %d enabled", total, enabled)
	assert.GreaterOrEqual(t, total, 2)
	assert.Equal(t, 0, enabled)
	setMode("enabled")
	blue2.waitForEnabled(true)

	// Disabled again: both robots get disabled immediately, not on the next tick.
	start := time.Now()
	setMode("disabled")
	red1.waitForEnabled(false)
	blue2.waitForEnabled(false)
	note("== Both disabled within %s", time.Since(start).Round(time.Millisecond))
	assert.Less(t, time.Since(start), 400*time.Millisecond)

	// Reassigning stations drops the moved team and the waiting one, and the waiting team can reconnect to its station.
	status, _ = call(
		"PUT", "/api/stations", [6]field.Assignment{
			{TeamId: 9999, WpaKey: "ninesnine"}, empty, empty, empty, {TeamId: 1114, WpaKey: "simbotics"}, empty,
		},
	)
	require.Equal(t, 200, status)
	assert.NotNil(t, red1.waitForClose(), "254 was moved out of R1")
	assert.NotNil(t, waiting.waitForClose(), "waiting 9999 should be dropped to reconnect")
	red1 = connectFakeDriverStation(t, ports, 9999)
	note("== Team 9999 reconnect handshake reply %v", red1.assignment)
	assert.Equal(t, []byte{0, 6, 31, 0, 0, 0, 0x27, 0x0f}, red1.assignment, "R1, status good")

	// Off: every connection is closed so the driver stations go back to local control, and the port is closed.
	setMode("off")
	assert.NotNil(t, red1.waitForClose())
	assert.NotNil(t, blue2.waitForClose())
	assert.NotNil(t, dialFms(), "FMS port should be closed while off")

	// And it can be turned back on.
	setMode("disabled")
	assert.Nil(t, dialFms())
	setMode("off")

	note("== Log tail\n%s", strings.Join(logTail.Lines(), "\n"))
}
