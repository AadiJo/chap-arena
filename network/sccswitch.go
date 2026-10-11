// Copyright 2025 Team 254. All Rights Reserved.
// Author: pat@patfairbank.com (Patrick Fairbank)
//
// Methods for turning the driver station ports on an SCC switch on and off via SSH.

package network

import (
	"bytes"
	"fmt"
	"golang.org/x/crypto/ssh"
	"net"
	"strconv"
	"sync"
	"time"
)

const (
	sccSwitchConnectTimeoutSec = 5
	sccSwitchConfigTimeoutSec  = 5
	sccSwitchSSHPort           = 22
)

// Interfaces that driver stations 1 to 3 plug into on each alliance's SCC, for any left blank in settings.
var DefaultSCCStationPorts = [3]string{"gigabitEthernet 1/2", "gigabitEthernet 1/3", "gigabitEthernet 1/4"}

type SCCSwitch struct {
	address                string
	port                   int
	username               string
	password               string
	mutex                  sync.Mutex
	connectTimeoutDuration time.Duration
	configTimeoutDuration  time.Duration
	stationPorts           [3]string
	Status                 string
}

// Port 0 means the standard SSH port; tests point it at a fake. stationPorts are the interfaces stations 1 to 3 plug
// into.
func NewSCCSwitch(address string, port int, username, password string, stationPorts [3]string) *SCCSwitch {
	if port == 0 {
		port = sccSwitchSSHPort
	}
	return &SCCSwitch{
		address:                address,
		port:                   port,
		username:               username,
		password:               password,
		connectTimeoutDuration: sccSwitchConnectTimeoutSec * time.Second,
		configTimeoutDuration:  sccSwitchConfigTimeoutSec * time.Second,
		stationPorts:           stationPorts,
		Status:                 "UNKNOWN",
	}
}

// Turns the ports of the given stations (1 to 3, in order) on or off, leaving the others alone. Turning a port off and
// back on makes the driver station plugged into it get a new DHCP lease, which it needs once its station's team, and so
// its subnet, changes. Does nothing if no station is given.
func (scc *SCCSwitch) SetStationPortsEnabled(stations [3]bool, enabled bool) error {
	if stations == [3]bool{} {
		return nil
	}
	scc.mutex.Lock()
	defer scc.mutex.Unlock()

	scc.Status = "CONFIGURING"

	portCommand := "shutdown"
	if enabled {
		portCommand = "no shutdown"
	}
	commandSequence := []string{"configure terminal"}
	for i, selected := range stations {
		if selected {
			commandSequence = append(commandSequence, "interface "+scc.stationPorts[i], portCommand, "exit")
		}
	}
	commandSequence = append(commandSequence, "exit", "exit")

	_, err := scc.runCommandSequence(commandSequence)
	if err != nil {
		scc.Status = "ERROR"
		return fmt.Errorf("failed to set station port state: %w", err)
	}

	if enabled {
		scc.Status = "ACTIVE"
	} else {
		scc.Status = "DISABLED"
	}

	return nil
}

// Logs into the switch via SSH and runs the given commands in sequence.
// Returns the output of the commands or an error if the operation fails.
func (scc *SCCSwitch) runCommandSequence(commands []string) (string, error) {
	// Open an SSH connection to the switch.
	sshConfig := &ssh.ClientConfig{
		User: scc.username,
		Auth: []ssh.AuthMethod{
			ssh.Password(scc.password),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // Allow any host key for simplicity
		Timeout:         scc.connectTimeoutDuration,
	}
	client, err := ssh.Dial("tcp", net.JoinHostPort(scc.address, strconv.Itoa(scc.port)), sshConfig)
	if err != nil {
		return "", fmt.Errorf("failed to connect to SSH: %w", err)
	}
	defer client.Close()

	// Create an interactive session to run commands
	session, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("failed to create SSH session: %w", err)
	}
	defer session.Close()

	// Capture the session output
	var outputBuffer bytes.Buffer
	session.Stdout = &outputBuffer
	session.Stderr = &outputBuffer

	inputPipe, err := session.StdinPipe()
	if err != nil {
		return "", fmt.Errorf("failed to create input pipe: %w", err)
	}

	modes := ssh.TerminalModes{ssh.ECHO: 0}
	if err := session.RequestPty("vt100", 80, 40, modes); err != nil {
		return "", fmt.Errorf("failed to configure shell: %w", err)
	}

	// Launch the switch's interactive shell
	err = session.Shell()
	if err != nil {
		return "", fmt.Errorf("failed to start shell: %w", err)
	}

	// Submit the commands to the switch
	for _, command := range commands {
		if _, err := fmt.Fprintln(inputPipe, command); err != nil {
			return "", fmt.Errorf("failed to write command to switch: %w", err)
		}
	}

	// Wait for the remote to process the commands and exit the shell
	done := make(chan error, 1)
	go func() {
		done <- session.Wait()
	}()
	select {
	case err := <-done:
		if err != nil {
			return "", fmt.Errorf("failed to run command sequence: %w", err)
		}
	case <-time.After(scc.configTimeoutDuration):
		return "", fmt.Errorf("timed out waiting for command sequence to complete")
	}

	return outputBuffer.String(), nil
}
