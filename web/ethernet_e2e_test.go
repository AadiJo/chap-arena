// End-to-end test of reconfiguring the wired network: the real HTTP API and field controller against a fake switch that
// speaks Telnet and keeps a running config like a Catalyst 3560, and fake red and blue SCCs that speak SSH. Applying
// should only touch the stations whose VLAN on the switch doesn't match their team, so driver stations at the other
// stations stay connected. Writes every request and the commands each device received to e2e-output/ethernet.txt for
// inspection.
//
// What could go wrong, and the step that catches it:
//   - The real switch's output isn't parsed right, so matching stations are redone or stale ones are skipped (first
//     apply, against a capture from the field's 3560).
//   - A pool saved by an older Chap Arena, without the FMS route, counts as matching because its gateway does (first
//     apply).
//   - Changing one station rewrites other VLANs or turns off other ports, on either SCC (R3 changes team).
//   - SCC ports come back on before the switch is done, or go off after it starts (R3 changes team).
//   - A WPA key change, which is Wi-Fi only, touches the switch or SCCs (key only).
//   - Emptying a station leaves its old VLAN or adds an empty one (R3 emptied).
//   - SCC ports get bounced while the switch can't be read (switch unreachable).
//   - A switch that power-cycled and lost everything applied since it was saved is trusted to still have it (power
//     cycle).

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
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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

// Plays the field's Catalyst 3560 over Telnet. Lets anyone log in, keeps the team VLAN interfaces' addresses and DHCP
// pools, applies the config commands it's sent, and answers "show running-config" the way the real switch prints it.
// It starts as the field's real 3560 just after it powered on (testdata/c3560_running_config_after_power_on.txt, which
// has team 2687 saved on VLAN 20 by an older Chap Arena) and answers with that capture verbatim until it's sent a
// config command. Each config session's lines are recorded in hardware; reads aren't. stop makes it unreachable, start
// brings it back as it was, and powerCycle puts it back to its startup config, losing everything applied since.
type fakeSwitch struct {
	t        *testing.T
	hardware *hardwareLog
	port     int
	listener net.Listener
	startup  string

	mutex    sync.Mutex
	verbatim bool             // Still answering with startup as captured.
	gateways map[int]string   // Address on each VLAN interface that has one, e.g. "10.2.54.4 255.255.255.0".
	pools    map[int][]string // Lines of each DHCP pool named dhcp<vlan>, as entered.
}

func newFakeSwitch(t *testing.T, hardware *hardwareLog) *fakeSwitch {
	startup, err := os.ReadFile(filepath.Join("testdata", "c3560_running_config_after_power_on.txt"))
	require.Nil(t, err)
	sw := &fakeSwitch{t: t, hardware: hardware, startup: string(startup)}
	sw.powerCycle()
	sw.start()
	t.Cleanup(sw.stop)
	return sw
}

func (sw *fakeSwitch) powerCycle() {
	sw.mutex.Lock()
	defer sw.mutex.Unlock()
	sw.verbatim, sw.gateways, sw.pools = true, map[int]string{}, map[int][]string{}
	sw.apply(strings.Split(sw.startup, "\n"))
}

// Listens on the switch's port, or on a free one the first time.
func (sw *fakeSwitch) start() {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", sw.port))
	require.Nil(sw.t, err)
	sw.listener, sw.port = listener, listener.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go sw.serve(conn)
		}
	}()
}

func (sw *fakeSwitch) stop() { _ = sw.listener.Close() }

// One Telnet session. Everything Chap Arena sends arrives as lines, the config ones between "config terminal" and
// "end", and the session ends at the final "exit" like on the real switch.
func (sw *fakeSwitch) serve(conn net.Conn) {
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
		case line == "show running-config":
			_, _ = conn.Write([]byte(strings.ReplaceAll(sw.runningConfig(), "\n", "\r\n")))
		case line == "exit":
			if len(commands) > 0 {
				sw.mutex.Lock()
				sw.apply(commands)
				sw.verbatim = false
				sw.mutex.Unlock()
				sw.hardware.add("switch", commands)
			}
			return
		}
	}
}

// Applies config lines the way IOS does as far as team VLANs go: "interface VlanN" and "ip dhcp pool dhcpN" enter a
// submode, the lines after them that belong to it configure it, and "!" or any other command leaves it. Indentation
// from a printed config is ignored. Caller must hold the mutex.
func (sw *fakeSwitch) apply(lines []string) {
	scan := func(line, format string, n *int) bool {
		count, err := fmt.Sscanf(line, format, n)
		return err == nil && count == 1
	}
	isPoolCommand := func(line string) bool {
		return slices.ContainsFunc(
			[]string{"network ", "default-router ", "option ", "lease ", "domain-name ", "dns-server "},
			func(prefix string) bool { return strings.HasPrefix(line, prefix) },
		)
	}
	mode, vlan := "", 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		var n int
		switch {
		case scan(line, "interface Vlan%d", &n):
			mode, vlan = "interface", n
		case scan(line, "ip dhcp pool dhcp%d", &n):
			mode, vlan = "pool", n
			if sw.pools[n] == nil {
				sw.pools[n] = []string{}
			}
		case scan(line, "no ip dhcp pool dhcp%d", &n):
			delete(sw.pools, n)
			mode = ""
		case mode == "interface" && strings.HasPrefix(line, "ip address "):
			sw.gateways[vlan] = strings.TrimPrefix(line, "ip address ")
		case mode == "interface" && line == "no ip address":
			delete(sw.gateways, vlan)
		case mode == "pool" && isPoolCommand(line):
			sw.pools[vlan] = append(sw.pools[vlan], line)
		case mode == "interface" && strings.HasPrefix(line, "ip access-group "):
			// Another setting on the interface.
		default:
			mode = ""
		}
	}
}

// What "show running-config" prints: the power-on capture until something's been configured, then the team VLANs'
// pools and interfaces formatted like the capture (pool lines indented three spaces, a space after default-router's
// address).
func (sw *fakeSwitch) runningConfig() string {
	sw.mutex.Lock()
	defer sw.mutex.Unlock()
	if sw.verbatim {
		return sw.startup
	}
	var config strings.Builder
	config.WriteString("Building configuration...\n\nCurrent configuration : 8588 bytes\n!\n")
	for _, vlan := range slices.Sorted(maps.Keys(sw.pools)) {
		fmt.Fprintf(&config, "ip dhcp pool dhcp%d\n", vlan)
		for _, line := range sw.pools[vlan] {
			if strings.HasPrefix(line, "default-router ") {
				line += " "
			}
			fmt.Fprintf(&config, "   %s\n", line)
		}
		config.WriteString("!\n")
	}
	for _, vlan := range []int{1, 10, 20, 30, 40, 50, 60, 100} {
		fmt.Fprintf(&config, "interface Vlan%d\n", vlan)
		if gateway, ok := sw.gateways[vlan]; ok {
			fmt.Fprintf(&config, " ip address %s\n", gateway)
		} else {
			config.WriteString(" no ip address\n")
		}
		config.WriteString("!\n")
	}
	config.WriteString("end\n")
	return config.String()
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
	fakeSwitch := newFakeSwitch(t, hardware)
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
				Port: fakeSwitch.port, PauseDuration: time.Millisecond, BackoffDuration: time.Millisecond,
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
	team2687 := field.Assignment{TeamId: 2687, WpaKey: "key2687a"}
	sessions := apply(
		"First apply, against the field switch as it powered on: R2's team 2687 is saved there, but its pool has no FMS "+
			"route, so R2 is redone with R1 and R3; the empty blue VLANs already match and blue's SCC isn't contacted",
		[6]field.Assignment{team254, team2687, {TeamId: 1678, WpaKey: "key1678a"}, empty, empty, empty}, 4,
	)
	require.Len(t, sessions, 4)
	assert.Equal(t, sccPorts("red SCC", "shutdown", "1/2", "1/3", "1/4"), sessions[0])
	assert.Equal(t, removeVlans(10, 20, 30), sessions[1])
	assert.Equal(
		t,
		[]string{"interface Vlan10", "ip address 10.2.54.4 255.255.255.0", "interface Vlan20",
			"ip address 10.26.87.4 255.255.255.0", "interface Vlan30", "ip address 10.16.78.4 255.255.255.0"},
		addedVlans(sessions[2]),
	)
	assert.Contains(t, sessions[2].commands, "option 121 hex 180a.0064.0a1a.5704")
	assert.Equal(t, sccPorts("red SCC", "no shutdown", "1/2", "1/3", "1/4"), sessions[3])

	sessions = apply(
		"R3 changes team: only R3's VLAN and red SCC port are touched, with the port off around the switch change",
		[6]field.Assignment{team254, team2687, {TeamId: 2056, WpaKey: "key2056a"}, empty, empty, empty}, 4,
	)
	require.Len(t, sessions, 4)
	assert.Equal(t, sccPorts("red SCC", "shutdown", "1/4"), sessions[0])
	assert.Equal(t, removeVlans(30), sessions[1])
	assert.Equal(t, []string{"interface Vlan30", "ip address 10.20.56.4 255.255.255.0"}, addedVlans(sessions[2]))
	assert.Equal(t, sccPorts("red SCC", "no shutdown", "1/4"), sessions[3])

	note("\n== Key only: R1's WPA key changes, which is Wi-Fi only, so nothing wired is touched")
	team254 = field.Assignment{TeamId: 254, WpaKey: "newkey254"}
	start := hardware.count()
	status, _ = call(
		"PUT", "/api/stations",
		[6]field.Assignment{team254, team2687, {TeamId: 2056, WpaKey: "key2056a"}, empty, empty, empty},
	)
	require.Equal(t, 200, status)
	require.Eventually(
		t, func() bool { return logCount("Switch already configured for stations") == 1 }, 5*time.Second,
		5*time.Millisecond,
	)
	assert.Equal(t, start, hardware.count())

	sessions = apply(
		"R3 emptied: R3's VLAN is removed with none added",
		[6]field.Assignment{team254, team2687, empty, empty, empty, empty}, 3,
	)
	assert.Equal(
		t,
		[]hardwareSession{
			sccPorts("red SCC", "shutdown", "1/4"), removeVlans(30), sccPorts("red SCC", "no shutdown", "1/4"),
		},
		sessions,
	)

	note("\n== Switch unreachable: R2 changes team, but the switch can't be read, so no SCC port is touched")
	fakeSwitch.stop()
	team1114 := field.Assignment{TeamId: 1114, WpaKey: "simbotics"}
	start = hardware.count()
	status, _ = call("PUT", "/api/stations", [6]field.Assignment{team254, team1114, empty, empty, empty, empty})
	require.Equal(t, 200, status)
	require.Eventually(
		t, func() bool { return logCount("couldn't read the switch's configuration") == 1 }, 5*time.Second,
		5*time.Millisecond,
	)
	assert.Equal(t, start, hardware.count())

	fakeSwitch.start()
	sessions = apply(
		"Switch back: applying the same stations again redoes only R2, which the switch still has as 2687",
		[6]field.Assignment{team254, team1114, empty, empty, empty, empty}, 4,
	)
	require.Len(t, sessions, 4)
	assert.Equal(t, sccPorts("red SCC", "shutdown", "1/3"), sessions[0])
	assert.Equal(t, removeVlans(20), sessions[1])
	assert.Equal(t, []string{"interface Vlan20", "ip address 10.11.14.4 255.255.255.0"}, addedVlans(sessions[2]))
	assert.Equal(t, sccPorts("red SCC", "no shutdown", "1/3"), sessions[3])

	note("\n== Power cycle: the switch reboots into its startup config and loses R1 and R2")
	fakeSwitch.powerCycle()
	sessions = apply(
		"Same stations applied again: R1 and R2 are redone, R3 and blue already match the startup config",
		[6]field.Assignment{team254, team1114, empty, empty, empty, empty}, 4,
	)
	require.Len(t, sessions, 4)
	assert.Equal(t, sccPorts("red SCC", "shutdown", "1/2", "1/3"), sessions[0])
	assert.Equal(t, removeVlans(10, 20), sessions[1])
	assert.Equal(
		t,
		[]string{"interface Vlan10", "ip address 10.2.54.4 255.255.255.0", "interface Vlan20",
			"ip address 10.11.14.4 255.255.255.0"},
		addedVlans(sessions[2]),
	)
	assert.Equal(t, sccPorts("red SCC", "no shutdown", "1/2", "1/3"), sessions[3])

	note("\n== Log tail\n%s", strings.Join(logTail.Lines(), "\n"))
}
