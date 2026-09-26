// Copyright 2014 Team 254. All Rights Reserved.
// Author: pat@patfairbank.com (Patrick Fairbank)
//
// Optional FMS connection to the driver stations, for enabling and disabling every robot at once. While it's off
// nothing listens on the FMS ports, so driver stations stay in local mode and teams enable their own robots. A driver
// station only hands control to FMS while it holds the TCP connection, so turning it off closes every connection.

package field

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"
)

const (
	// Driver stations look for FMS at this address; the field switch config puts it on VLAN 100.
	fmsIpAddress = "10.0.100.5"
	// Where the old NI DS listens for control packets. The new DS says where in its first TCP packet.
	driverStationRoboRioUdpPort = 1121
	driverStationTcpLinkTimeout = 5 * time.Second
	driverStationUdpLinkTimeout = time.Second
	driverStationPacketPeriod   = 500 * time.Millisecond
	maxTcpPacketBytes           = 65537 // 2 for size, then 2^16-1 for data.
)

// DriverStationMode is whether FMS is talking to driver stations and, if so, whether robots are enabled.
type DriverStationMode string

const (
	// Nothing listens for driver stations; teams control their own robots. Always the mode at startup.
	DriverStationsOff DriverStationMode = "off"
	// Driver stations connect to FMS and every robot is disabled.
	DriverStationsDisabled DriverStationMode = "disabled"
	// Every robot connected when this mode was set is enabled, in teleop. Robots that connect later stay disabled until
	// it's set again.
	DriverStationsEnabled DriverStationMode = "enabled"
)

// Rejects anything but the three modes, so a bad API request fails to decode.
func (mode *DriverStationMode) UnmarshalText(text []byte) error {
	switch parsed := DriverStationMode(text); parsed {
	case DriverStationsOff, DriverStationsDisabled, DriverStationsEnabled:
		*mode = parsed
		return nil
	}
	return fmt.Errorf("unknown driver station mode %q", text)
}

// DriverStationPorts are the ports FMS listens on. main uses DefaultDriverStationPorts; tests use free ones.
type DriverStationPorts struct {
	Tcp int
	Udp int
}

var DefaultDriverStationPorts = DriverStationPorts{Tcp: 1750, Udp: 1160}

// DriverStationStatus is one station's driver station connection. Everything is false/zero when nothing is connected.
type DriverStationStatus struct {
	Connected      bool    `json:"connected"` // The DS holds a TCP connection to FMS.
	Enabled        bool    `json:"enabled"`   // What FMS is telling the DS.
	DsLinked       bool    `json:"dsLinked"`  // The rest come from the DS's UDP status packets.
	RadioLinked    bool    `json:"radioLinked"`
	RioLinked      bool    `json:"rioLinked"`
	RobotLinked    bool    `json:"robotLinked"`
	BatteryVoltage float64 `json:"batteryVoltage"`
	TripTimeMs     int     `json:"tripTimeMs"`
	MissedPackets  int     `json:"missedPackets"`
}

// One accepted driver station.
type dsConn struct {
	teamId         int
	station        int
	tcpConn        net.Conn
	udpAddrPort    netip.AddrPort
	packetCount    int
	lastPacketTime time.Time
	status         DriverStationStatus
}

// Listeners and connections for one stretch of FMS being on. Goroutines compare against driverStations.session to
// notice they belong to one that has since been closed.
type dsSession struct {
	tcpListener net.Listener
	udpConn     *net.UDPConn
	done        chan struct{}
	conns       [6]*dsConn
	// Driver stations told their team isn't assigned. Held open, as full Cheesy Arena does, and closed on the next
	// assignment change so they reconnect and pick up their station.
	waiting map[net.Conn]struct{}
}

type driverStations struct {
	ports DriverStationPorts

	// Guards everything below. Never held while calling into Field.
	mutex   sync.Mutex
	mode    DriverStationMode
	teamIds [6]int
	session *dsSession // nil while off
}

func newDriverStations(ports DriverStationPorts) *driverStations {
	return &driverStations{ports: ports, mode: DriverStationsOff}
}

// Switches mode, starting or stopping the listeners as needed, and sends the new enabled state right away. Setting
// Enabled again enables anything that connected since. Returns an error, leaving the mode alone, if the listeners can't
// start.
func (ds *driverStations) setMode(mode DriverStationMode) error {
	ds.mutex.Lock()
	defer ds.mutex.Unlock()
	switch {
	case mode == DriverStationsOff && ds.session != nil:
		ds.session.close()
		ds.session = nil
	case mode != DriverStationsOff && ds.session == nil:
		session, err := ds.listen()
		if err != nil {
			return err
		}
		ds.session = session
	}
	ds.mode = mode

	switch mode {
	case DriverStationsOff:
		log.Println("FMS off; teams control their own robots.")
	case DriverStationsDisabled:
		log.Println("FMS on; all robots disabled.")
	case DriverStationsEnabled:
		log.Printf("Robots enabled: %s", ds.describeConnected())
	}
	if ds.session != nil {
		for _, conn := range ds.session.conns {
			if conn != nil {
				conn.status.Enabled = mode == DriverStationsEnabled
			}
		}
		ds.sendControlPackets()
	}
	return nil
}

// Records the teams assigned to each station. Drops any driver station whose team is no longer in its station, and any
// waiting one, so they reconnect to their new station (or wait again).
func (ds *driverStations) setTeams(teamIds [6]int) {
	ds.mutex.Lock()
	defer ds.mutex.Unlock()
	ds.teamIds = teamIds
	if ds.session == nil {
		return
	}
	for i, conn := range ds.session.conns {
		if conn != nil && conn.teamId != teamIds[i] {
			log.Printf("Dropping team %d's driver station from %s; the station was reassigned.", conn.teamId, StationNames[i])
			closeQuietly(conn.tcpConn)
			ds.session.conns[i] = nil
		}
	}
	for tcpConn := range ds.session.waiting {
		closeQuietly(tcpConn)
	}
}

func (ds *driverStations) status() (DriverStationMode, [6]DriverStationStatus) {
	ds.mutex.Lock()
	defer ds.mutex.Unlock()
	var statuses [6]DriverStationStatus
	if ds.session != nil {
		for i, conn := range ds.session.conns {
			if conn != nil {
				statuses[i] = conn.status
			}
		}
	}
	return ds.mode, statuses
}

// Opens the FMS ports and starts the goroutines serving them. Caller must hold the mutex.
func (ds *driverStations) listen() (*dsSession, error) {
	tcpListener, err := net.Listen("tcp4", fmt.Sprintf(":%d", ds.ports.Tcp))
	if err != nil {
		return nil, fmt.Errorf("couldn't listen for driver stations: %w", err)
	}
	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: ds.ports.Udp})
	if err != nil {
		closeQuietly(tcpListener)
		return nil, fmt.Errorf("couldn't listen for driver stations: %w", err)
	}
	log.Printf("Listening for driver stations on TCP %d and UDP %d.", ds.ports.Tcp, ds.ports.Udp)
	if !hasLocalAddress(fmsIpAddress) {
		log.Printf("This computer doesn't have the address %s, so driver stations won't find it.", fmsIpAddress)
	}
	session := &dsSession{
		tcpListener: tcpListener, udpConn: udpConn, done: make(chan struct{}), waiting: map[net.Conn]struct{}{},
	}
	go ds.acceptConnections(session)
	go ds.receiveStatusPackets(session)
	go ds.sendPeriodically(session)
	return session, nil
}

// Closes the listeners and every driver station connection, which ends all of the session's goroutines.
func (session *dsSession) close() {
	close(session.done)
	closeQuietly(session.tcpListener)
	closeQuietly(session.udpConn)
	for _, conn := range session.conns {
		if conn != nil {
			closeQuietly(conn.tcpConn)
		}
	}
	for tcpConn := range session.waiting {
		closeQuietly(tcpConn)
	}
}

func (ds *driverStations) acceptConnections(session *dsSession) {
	for {
		tcpConn, err := session.tcpListener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("Error accepting driver station connection: %v", err)
			continue
		}
		go ds.handleConnection(session, tcpConn)
	}
}

// Reads the driver station's first packet, tells it its station (or that it isn't assigned one), then holds the
// connection until either side closes it.
func (ds *driverStations) handleConnection(session *dsSession, tcpConn net.Conn) {
	buffer := make([]byte, maxTcpPacketBytes)
	count, err := readTaggedTcpPacket(tcpConn, buffer)
	if err != nil {
		log.Printf("Error reading initial driver station packet: %v", err)
		closeQuietly(tcpConn)
		return
	}
	teamId, udpPort, newDs, err := parseInitialPacket(buffer[:count])
	if err != nil {
		log.Printf("Rejecting driver station from %s: %v", tcpConn.RemoteAddr(), err)
		closeQuietly(tcpConn)
		return
	}
	remoteAddr, err := netip.ParseAddrPort(tcpConn.RemoteAddr().String())
	if err != nil {
		log.Printf("Rejecting driver station from %s: %v", tcpConn.RemoteAddr(), err)
		closeQuietly(tcpConn)
		return
	}

	ds.mutex.Lock()
	if ds.session != session {
		ds.mutex.Unlock()
		closeQuietly(tcpConn)
		return
	}
	station := -1
	for i, assignedTeamId := range ds.teamIds {
		if teamId != 0 && assignedTeamId == teamId {
			station = i
		}
	}
	if station == -1 {
		log.Printf("Team %d's driver station connected, but the team isn't assigned to a station.", teamId)
		session.waiting[tcpConn] = struct{}{}
		ds.mutex.Unlock()
		// Status 2 shows the DS it's waiting for a match.
		if writeAssignmentPacket(tcpConn, newDs, 0, 2, 0) == nil {
			holdConnection(tcpConn, buffer)
		}
		ds.mutex.Lock()
		delete(session.waiting, tcpConn)
		ds.mutex.Unlock()
		closeQuietly(tcpConn)
		return
	}
	if err := writeAssignmentPacket(tcpConn, newDs, station, 0, teamId); err != nil {
		ds.mutex.Unlock()
		log.Printf("Error sending team %d its station: %v", teamId, err)
		closeQuietly(tcpConn)
		return
	}
	conn := &dsConn{
		teamId:      teamId,
		station:     station,
		tcpConn:     tcpConn,
		udpAddrPort: netip.AddrPortFrom(remoteAddr.Addr(), uint16(udpPort)),
		status:      DriverStationStatus{Connected: true},
	}
	if previous := session.conns[station]; previous != nil {
		closeQuietly(previous.tcpConn)
	}
	session.conns[station] = conn
	log.Printf("Team %d's driver station connected to %s from %s.", teamId, StationNames[station], remoteAddr.Addr())
	ds.mutex.Unlock()

	holdConnection(tcpConn, buffer)

	ds.mutex.Lock()
	if session.conns[station] == conn {
		session.conns[station] = nil
		log.Printf("Team %d's driver station disconnected from %s.", teamId, StationNames[station])
	}
	ds.mutex.Unlock()
	closeQuietly(tcpConn)
}

// Reads status packets from every driver station, updating the link state and battery voltage.
func (ds *driverStations) receiveStatusPackets(session *dsSession) {
	data := make([]byte, 1500)
	for {
		count, err := session.udpConn.Read(data)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("Error reading driver station UDP packet: %v", err)
			continue
		}
		if count < 8 {
			continue
		}
		teamId := int(data[4])<<8 + int(data[5])

		ds.mutex.Lock()
		for _, conn := range session.conns {
			if conn != nil && conn.teamId == teamId {
				conn.updateFromStatusPacket(data[:count])
			}
		}
		ds.mutex.Unlock()
	}
}

func (conn *dsConn) updateFromStatusPacket(packet []byte) {
	status := &conn.status
	conn.lastPacketTime = time.Now()
	status.DsLinked = true
	status.RioLinked = packet[3]&0x08 != 0
	status.RadioLinked = packet[3]&0x10 != 0
	status.RobotLinked = packet[3]&0x20 != 0
	if status.RobotLinked {
		// Stored as volts * 256.
		status.BatteryVoltage = float64(packet[6]) + float64(packet[7])/256
	}

	// Tag 1 carries lost packets and round trip time.
	for index := 8; index < len(packet); {
		length := int(packet[index])
		index++
		if length == 0 {
			continue
		}
		if index+length > len(packet) {
			break
		}
		if packet[index] == 1 && length == 6 {
			status.MissedPackets = int(packet[index+1])<<8 + int(packet[index+2])
			status.TripTimeMs = int(packet[index+5])
		}
		index += length
	}
}

// Sends control packets on a timer, so driver stations keep their FMS link, and times out links that went quiet.
func (ds *driverStations) sendPeriodically(session *dsSession) {
	ticker := time.NewTicker(driverStationPacketPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-session.done:
			return
		case <-ticker.C:
		}
		ds.mutex.Lock()
		if ds.session == session {
			for _, conn := range session.conns {
				if conn != nil && time.Since(conn.lastPacketTime) > driverStationUdpLinkTimeout {
					conn.status = DriverStationStatus{Connected: true, Enabled: conn.status.Enabled}
				}
			}
			ds.sendControlPackets()
		}
		ds.mutex.Unlock()
	}
}

// Caller must hold the mutex, with a session open.
func (ds *driverStations) sendControlPackets() {
	for _, conn := range ds.session.conns {
		if conn == nil {
			continue
		}
		if _, err := ds.session.udpConn.WriteToUDPAddrPort(conn.controlPacket(), conn.udpAddrPort); err != nil {
			log.Printf("Error sending control packet to team %d: %v", conn.teamId, err)
		}
	}
}

// Builds the next UDP control packet. It's always teleop in a test match, with no match number or time remaining.
func (conn *dsConn) controlPacket() []byte {
	var packet [22]byte
	packet[0] = byte(conn.packetCount >> 8)
	packet[1] = byte(conn.packetCount)
	packet[2] = 0 // Protocol version.

	// Robot status byte. Auto (0x02), A-stop (0x40) and E-stop (0x80) stay clear.
	if conn.status.Enabled {
		packet[3] |= 0x04
	}

	packet[5] = byte(conn.station)
	// Bytes 6-8 are the match type (0 is test) and number.
	packet[9] = 1 // Match repeat number.

	now := time.Now()
	microseconds := now.Nanosecond() / 1000
	packet[10] = byte(microseconds >> 24)
	packet[11] = byte(microseconds >> 16)
	packet[12] = byte(microseconds >> 8)
	packet[13] = byte(microseconds)
	packet[14] = byte(now.Second())
	packet[15] = byte(now.Minute())
	packet[16] = byte(now.Hour())
	packet[17] = byte(now.Day())
	packet[18] = byte(now.Month())
	packet[19] = byte(now.Year() - 1900)
	// Bytes 20-21 are the seconds left in the match.

	conn.packetCount++
	return packet[:]
}

// Parses a driver station's first TCP packet. The old NI DS sends tag 24 with the team number; the new DS sends tag 30
// with the UDP port to send control packets to and the team number in ASCII.
func parseInitialPacket(packet []byte) (teamId int, udpPort int, newDs bool, err error) {
	if len(packet) >= 5 && packet[0] == 0 && packet[1] == 3 && packet[2] == 24 {
		return int(packet[3])<<8 + int(packet[4]), driverStationRoboRioUdpPort, false, nil
	}
	if len(packet) >= 7 && packet[0] == 0 && packet[1] >= 5 && packet[2] == 30 {
		// Byte 5 is flags.
		teamNumberLength := int(packet[6])
		if len(packet) < 7+teamNumberLength {
			return 0, 0, false, fmt.Errorf("initial packet too short: %v", packet)
		}
		teamId, err = strconv.Atoi(string(packet[7 : 7+teamNumberLength]))
		if err != nil || teamId < 0 || teamId > 65535 {
			return 0, 0, false, fmt.Errorf("invalid team number in initial packet: %v", packet)
		}
		return teamId, int(packet[3])<<8 + int(packet[4]), true, nil
	}
	return 0, 0, false, fmt.Errorf("invalid initial packet: %v", packet)
}

// Tells a driver station its station (0-5 for R1-B3) and status (0 good, 2 waiting).
func writeAssignmentPacket(tcpConn net.Conn, newDs bool, station int, status byte, teamId int) error {
	packet := []byte{0, 6, 31, byte(station), status, 0, byte(teamId >> 8), byte(teamId)}
	if !newDs {
		packet = []byte{0, 3, 25, byte(station), status}
	}
	if err := tcpConn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	_, err := tcpConn.Write(packet)
	return err
}

// Reads and discards packets (keepalives and DS logs) until the connection closes or goes quiet.
func holdConnection(tcpConn net.Conn, buffer []byte) {
	for {
		if _, err := readTaggedTcpPacket(tcpConn, buffer); err != nil {
			if !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
				log.Printf("Driver station connection from %s ended: %v", tcpConn.RemoteAddr(), err)
			}
			return
		}
	}
}

// Reads one packet: a two-byte big-endian length, then that many bytes. Returns the total bytes read.
func readTaggedTcpPacket(tcpConn net.Conn, buffer []byte) (int, error) {
	if err := tcpConn.SetReadDeadline(time.Now().Add(driverStationTcpLinkTimeout)); err != nil {
		return 0, err
	}
	if _, err := io.ReadFull(tcpConn, buffer[:2]); err != nil {
		return 0, err
	}
	packetLength := int(buffer[0])<<8 + int(buffer[1])
	if _, err := io.ReadFull(tcpConn, buffer[2:2+packetLength]); err != nil {
		return 0, err
	}
	return 2 + packetLength, nil
}

func closeQuietly(closer io.Closer) {
	if err := closer.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Printf("Error closing driver station connection: %v", err)
	}
}

// Formats connected teams as "R1=254 B2=1114" for the log, or "none connected".
func (ds *driverStations) describeConnected() string {
	description := ""
	if ds.session != nil {
		for i, conn := range ds.session.conns {
			if conn != nil {
				description += fmt.Sprintf(" %s=%d", StationNames[i], conn.teamId)
			}
		}
	}
	if description == "" {
		return "none connected"
	}
	return description[1:]
}

func hasLocalAddress(address string) bool {
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, addr := range addresses {
		if ipNet, ok := addr.(*net.IPNet); ok && ipNet.IP.String() == address {
			return true
		}
	}
	return false
}
