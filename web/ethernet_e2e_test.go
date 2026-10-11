// End-to-end test of reconfiguring the wired network: the real HTTP API and field controller against a fake switch that
// speaks Telnet and fake red and blue SCCs that speak SSH. Applying should only touch the stations whose team changed,
// so driver stations at the other stations stay connected. Writes every request and the commands each device received
// to e2e-output/ethernet.txt for inspection.
//
// What could go wrong, and the step that catches it:
//   - The first apply after startup trusts a switch it hasn't configured and skips VLANs or ports (first apply).
//   - Changing one station rewrites other VLANs or turns off other ports, on either SCC (R3 changes team).
//   - SCC ports come back on before the switch is done, or go off after it starts (R3 changes team).
//   - A WPA key change, which is Wi-Fi only, touches the switch or SCCs (key only).
//   - Emptying a station leaves its old VLAN or adds an empty one (R3 emptied).
//   - Saving unrelated settings forgets what the switch has, so the next apply rewrites everything (R3 emptied).
//   - A failed switch configuration counts as done, leaving its stations unconfigured (switch fails).
//   - Pointing at a different switch keeps what the old one had (switch password changes).

package web

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"github.com/Team254/cheesy-arena/field"
	"github.com/Team254/cheesy-arena/model"
	"github.com/Team254/cheesy-arena/network"
	"github.com/Team254/cheesy-arena/update"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// One connection to a fake device and the commands it received.
type hardwareSession struct {
	device   string
	commands []string
}

func (session hardwareSession) String() string {
	return session.device + ": " + strings.Join(session.commands, " / ")
}

// Sessions from the fake switch and SCCs, in the order they finished.
type hardwareLog struct {
	mutex    sync.Mutex
	sessions []hardwareSession
}

func (hardware *hardwareLog) add(device string, commands []string) {
	hardware.mutex.Lock()
	defer hardware.mutex.Unlock()
	hardware.sessions = append(hardware.sessions, hardwareSession{device, commands})
}

func (hardware *hardwareLog) count() int {
	hardware.mutex.Lock()
	defer hardware.mutex.Unlock()
	return len(hardware.sessions)
}

func (hardware *hardwareLog) from(start int) []hardwareSession {
	hardware.mutex.Lock()
	defer hardware.mutex.Unlock()
	return append([]hardwareSession(nil), hardware.sessions[start:]...)
}

// Plays the field's Catalyst switch on the given port (0 for any free one). Lets anyone log in, records the lines
// between "config terminal" and "end", and hangs up at the final "exit" like the real switch. Closing the returned
// listener makes the switch unreachable.
func startFakeSwitch(t *testing.T, hardware *hardwareLog, port int) net.Listener {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.Nil(t, err)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				var commands []string
				inConfig := false
				scanner := bufio.NewScanner(conn)
				for scanner.Scan() {
					switch line := scanner.Text(); {
					case line == "config terminal":
						inConfig = true
					case line == "end":
						inConfig = false
					case inConfig:
						commands = append(commands, line)
					case line == "exit":
						hardware.add("switch", commands)
						return
					}
				}
			}()
		}
	}()
	return listener
}

// Plays an SCC over SSH on a free port and returns the port. Lets anyone log in and follows the CLI's modes:
// "configure terminal" and "interface ..." each go a mode deeper, "exit" comes back up, and "exit" at the top ends the
// session.
func startFakeSCC(t *testing.T, hardware *hardwareLog, device string) int {
	_, privateKey, err := ed25519.GenerateKey(nil)
	require.Nil(t, err)
	signer, err := ssh.NewSignerFromKey(privateKey)
	require.Nil(t, err)
	config := &ssh.ServerConfig{
		PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) { return nil, nil },
	}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.Nil(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveFakeSCC(conn, config, hardware, device)
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port
}

func serveFakeSCC(conn net.Conn, config *ssh.ServerConfig, hardware *hardwareLog, device string) {
	defer conn.Close()
	_, channels, requests, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(requests)
	for newChannel := range channels {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "")
			continue
		}
		channel, channelRequests, err := newChannel.Accept()
		if err != nil {
			return
		}
		go func() {
			for request := range channelRequests {
				_ = request.Reply(request.Type == "pty-req" || request.Type == "shell", nil)
			}
		}()
		var commands []string
		depth := 0
		scanner := bufio.NewScanner(channel)
		for scanner.Scan() {
			line := strings.TrimRight(scanner.Text(), "\r")
			commands = append(commands, line)
			switch {
			case line == "configure terminal" && depth == 0, strings.HasPrefix(line, "interface ") && depth == 1:
				depth++
			case line == "exit" && depth > 0:
				depth--
			case line == "exit":
				hardware.add(device, commands)
				_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
				_ = channel.Close()
				return
			}
		}
	}
}

func TestEthernetEndToEnd(t *testing.T) {
	var transcript strings.Builder
	note := func(format string, args ...any) { fmt.Fprintf(&transcript, format+"\n", args...) }
	t.Cleanup(
		func() {
			outputDir := filepath.Join("..", "e2e-output")
			_ = os.MkdirAll(outputDir, 0755)
			_ = os.WriteFile(filepath.Join(outputDir, "ethernet.txt"), []byte(transcript.String()), 0644)
		},
	)
	logTail := NewLogTail(200)
	log.SetOutput(io.MultiWriter(os.Stderr, logTail))
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	hardware := &hardwareLog{}
	switchListener := startFakeSwitch(t, hardware, 0)
	t.Cleanup(func() { _ = switchListener.Close() })
	switchPort := switchListener.Addr().(*net.TCPAddr).Port
	redSCCPort := startFakeSCC(t, hardware, "red SCC")
	blueSCCPort := startFakeSCC(t, hardware, "blue SCC")
	apServer := httptest.NewServer(&fakeAccessPoint{})
	defer apServer.Close()

	database, err := model.OpenDatabase(filepath.Join(t.TempDir(), "event.db"))
	require.Nil(t, err)
	defer database.Close()
	fieldController, err := field.New(
		database, field.Options{
			DriverStationPorts: field.DefaultDriverStationPorts,
			RecordingsDir:      t.TempDir(),
			Switch: network.SwitchOptions{
				Port: switchPort, PauseDuration: time.Millisecond, BackoffDuration: time.Millisecond,
			},
			RedSCCPort:  redSCCPort,
			BlueSCCPort: blueSCCPort,
		},
	)
	require.Nil(t, err)
	server := httptest.NewServer(NewWeb(fieldController, logTail, update.New(update.Options{Current: "dev"})).newHandler())
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
		note("%s %s %s\n-> %d", method, path, requestJson, response.StatusCode)
		var decoded map[string]any
		_ = json.Unmarshal(responseJson, &decoded)
		return response.StatusCode, decoded
	}
	logCount := func(text string) int { return strings.Count(strings.Join(logTail.Lines(), "\n"), text) }

	// Applies the assignment, waits until the hardware has seen the expected number of sessions since, and returns them.
	apply := func(step string, assignments [6]field.Assignment, expectedSessions int) []hardwareSession {
		note("\n== %s", step)
		start := hardware.count()
		status, _ := call("PUT", "/api/stations", assignments)
		require.Equal(t, 200, status)
		require.Eventually(
			t, func() bool { return hardware.count() >= start+expectedSessions }, 5*time.Second, 5*time.Millisecond,
		)
		sessions := hardware.from(start)
		for _, session := range sessions {
			note("%s", session)
		}
		return sessions
	}

	// Expected sessions. Switch sessions that add VLANs are boiled down to the VLAN interfaces and gateway addresses;
	// network/switch_test.go checks the rest of each team's commands.
	sccPorts := func(device, portCommand string, ports ...string) hardwareSession {
		commands := []string{"configure terminal"}
		for _, port := range ports {
			commands = append(commands, "interface gigabitEthernet "+port, portCommand, "exit")
		}
		return hardwareSession{device, append(commands, "exit", "exit")}
	}
	removeVlans := func(vlans ...int) hardwareSession {
		var commands []string
		for _, vlan := range vlans {
			commands = append(
				commands, fmt.Sprintf("interface Vlan%d", vlan), "no ip address", fmt.Sprintf("no ip dhcp pool dhcp%d", vlan),
			)
		}
		return hardwareSession{"switch", commands}
	}
	addedVlans := func(session hardwareSession) []string {
		var lines []string
		for _, line := range session.commands {
			if strings.HasPrefix(line, "interface Vlan") || strings.HasPrefix(line, "ip address ") {
				lines = append(lines, line)
			}
		}
		return lines
	}
	allPortsOff := []hardwareSession{
		sccPorts("red SCC", "shutdown", "1/2", "1/3", "1/4"), sccPorts("blue SCC", "shutdown", "1/2", "1/3", "1/4"),
	}
	allPortsOn := []hardwareSession{
		sccPorts("red SCC", "no shutdown", "1/2", "1/3", "1/4"),
		sccPorts("blue SCC", "no shutdown", "1/2", "1/3", "1/4"),
	}
	// Checks a full reconfiguration: every port off on both SCCs (in either order), every VLAN removed, the given VLANs
	// added, then every port back on.
	assertFullReconfiguration := func(sessions []hardwareSession, added []string) {
		require.Len(t, sessions, 6)
		assert.ElementsMatch(t, allPortsOff, sessions[0:2])
		assert.Equal(t, removeVlans(10, 20, 30, 40, 50, 60), sessions[2])
		assert.Equal(t, added, addedVlans(sessions[3]))
		assert.ElementsMatch(t, allPortsOn, sessions[4:6])
	}

	// The SCC station ports start at their defaults; then everything is pointed at the fakes.
	_, settings := call("GET", "/api/settings", nil)
	assert.Equal(t, "gigabitEthernet 1/2", settings["SCCStation1Port"])
	assert.Equal(t, "gigabitEthernet 1/3", settings["SCCStation2Port"])
	assert.Equal(t, "gigabitEthernet 1/4", settings["SCCStation3Port"])
	settings["ApAddress"] = strings.TrimPrefix(apServer.URL, "http://")
	settings["SwitchAddress"] = "127.0.0.1"
	settings["SwitchPassword"] = "sw"
	settings["SCCManagementEnabled"] = true
	settings["RedSCCAddress"] = "127.0.0.1"
	settings["BlueSCCAddress"] = "127.0.0.1"
	settings["SCCUsername"] = "admin"
	settings["SCCPassword"] = "scc"
	status, _ := call("PUT", "/api/settings", settings)
	require.Equal(t, 200, status)

	empty := field.Assignment{}
	team254 := field.Assignment{TeamId: 254, WpaKey: "key254ab"}
	sessions := apply(
		"First apply: nothing is known about the switch yet, so every VLAN and port is redone",
		[6]field.Assignment{team254, empty, {TeamId: 1678, WpaKey: "key1678a"}, empty, empty, empty}, 6,
	)
	assertFullReconfiguration(
		sessions,
		[]string{"interface Vlan10", "ip address 10.2.54.4 255.255.255.0", "interface Vlan30",
			"ip address 10.16.78.4 255.255.255.0"},
	)

	sessions = apply(
		"R3 changes team: only R3's VLAN and red SCC port are touched, with the port off around the switch change",
		[6]field.Assignment{team254, empty, {TeamId: 2056, WpaKey: "key2056a"}, empty, empty, empty}, 4,
	)
	require.Len(t, sessions, 4)
	assert.Equal(t, sccPorts("red SCC", "shutdown", "1/4"), sessions[0])
	assert.Equal(t, removeVlans(30), sessions[1])
	assert.Equal(t, []string{"interface Vlan30", "ip address 10.20.56.4 255.255.255.0"}, addedVlans(sessions[2]))
	assert.Equal(t, sccPorts("red SCC", "no shutdown", "1/4"), sessions[3])

	note("\n== Key only: R1's WPA key changes, which is Wi-Fi only, so nothing wired is touched")
	start := hardware.count()
	status, _ = call(
		"PUT", "/api/stations",
		[6]field.Assignment{{TeamId: 254, WpaKey: "newkey254"}, empty, {TeamId: 2056, WpaKey: "key2056a"}, empty, empty, empty},
	)
	require.Equal(t, 200, status)
	require.Eventually(
		t, func() bool { return logCount("Switch already configured for stations") == 1 }, 5*time.Second,
		5*time.Millisecond,
	)
	assert.Equal(t, start, hardware.count())

	settings["RecordingFormat"] = "csv"
	note("\n== Unrelated settings saved")
	status, _ = call("PUT", "/api/settings", settings)
	require.Equal(t, 200, status)
	team254 = field.Assignment{TeamId: 254, WpaKey: "newkey254"}
	sessions = apply(
		"R3 emptied: the switch still knows the other stations after the save, and R3's VLAN is removed with none added",
		[6]field.Assignment{team254, empty, empty, empty, empty, empty}, 3,
	)
	assert.Equal(
		t,
		[]hardwareSession{
			sccPorts("red SCC", "shutdown", "1/4"), removeVlans(30), sccPorts("red SCC", "no shutdown", "1/4"),
		},
		sessions,
	)

	require.Nil(t, switchListener.Close())
	team1114 := field.Assignment{TeamId: 1114, WpaKey: "simbotics"}
	sessions = apply(
		"Switch fails: it's unreachable while R2 is assigned, so R2's port goes off and back on with nothing between",
		[6]field.Assignment{team254, team1114, empty, empty, empty, empty}, 2,
	)
	require.Eventually(
		t, func() bool { return logCount("Failed to configure team Ethernet") == 1 }, 5*time.Second, 5*time.Millisecond,
	)
	assert.Equal(
		t,
		[]hardwareSession{sccPorts("red SCC", "shutdown", "1/3"), sccPorts("red SCC", "no shutdown", "1/3")},
		sessions,
	)

	switchListener = startFakeSwitch(t, hardware, switchPort)
	sessions = apply(
		"Switch back: after a failure the switch's state is unknown, so the same assignment redoes everything",
		[6]field.Assignment{team254, team1114, empty, empty, empty, empty}, 6,
	)
	assertFullReconfiguration(
		sessions,
		[]string{"interface Vlan10", "ip address 10.2.54.4 255.255.255.0", "interface Vlan20",
			"ip address 10.11.14.4 255.255.255.0"},
	)

	settings["SwitchPassword"] = "sw2"
	note("\n== Switch password changes")
	status, _ = call("PUT", "/api/settings", settings)
	require.Equal(t, 200, status)
	sessions = apply(
		"Same assignment on a newly configured switch: nothing is known about it, so everything is redone",
		[6]field.Assignment{team254, team1114, empty, empty, empty, empty}, 6,
	)
	assertFullReconfiguration(
		sessions,
		[]string{"interface Vlan10", "ip address 10.2.54.4 255.255.255.0", "interface Vlan20",
			"ip address 10.11.14.4 255.255.255.0"},
	)

	note("\n== Log tail\n%s", strings.Join(logTail.Lines(), "\n"))
}
