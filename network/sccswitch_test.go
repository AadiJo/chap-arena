// Copyright 2025 Team 254. All Rights Reserved.
// Author: pat@patfairbank.com (Patrick Fairbank)

package network

import (
	"bytes"
	"fmt"
	"github.com/stretchr/testify/assert"
	"golang.org/x/crypto/ed25519"
	"golang.org/x/crypto/ssh"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestConfigureSCC(t *testing.T) {
	username := "username"
	password := "password"
	scc := NewSCCSwitch("127.0.0.1", 9150, username, password, [3]string{"gi 1/2", "gi 1/3", "gi 1/4"})
	scc.connectTimeoutDuration = 10 * time.Millisecond
	scc.configTimeoutDuration = 15 * time.Millisecond

	var receivedUpCommands, receivedDownCommands []string

	// Turn off stations 1 and 3's ports
	mockSSHSwitch(t, scc.port, username, password, &receivedDownCommands)
	assert.Nil(t, scc.SetStationPortsEnabled([3]bool{true, false, true}, false))
	assert.Equal(
		t,
		[]string{"configure terminal", "interface gi 1/2", "shutdown", "exit", "interface gi 1/4", "shutdown", "exit",
			"exit", "exit"},
		receivedDownCommands,
	)
	assert.Equal(t, "DISABLED", scc.Status)

	// Turn station 3's port back on
	scc.port += 1
	mockSSHSwitch(t, scc.port, username, password, &receivedUpCommands)
	assert.Nil(t, scc.SetStationPortsEnabled([3]bool{false, false, true}, true))
	assert.Equal(
		t, []string{"configure terminal", "interface gi 1/4", "no shutdown", "exit", "exit", "exit"}, receivedUpCommands,
	)
	assert.Equal(t, "ACTIVE", scc.Status)

	// No stations means no connection at all; nothing is listening on the next port.
	scc.port += 1
	assert.Nil(t, scc.SetStationPortsEnabled([3]bool{}, false))
	assert.Equal(t, "ACTIVE", scc.Status)
}

func mockSSHSwitch(t *testing.T, port int, username, password string, commands *[]string) {
	go func() {
		// Create a simple SSH server that accepts a connection with password authentication
		_, privateKey, err := ed25519.GenerateKey(nil)
		assert.Nil(t, err)
		signer, err := ssh.NewSignerFromKey(privateKey)
		assert.Nil(t, err)
		config := &ssh.ServerConfig{
			PasswordCallback: func(conn ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
				assert.Equal(t, username, conn.User())
				assert.Equal(t, password, string(pass))
				return nil, nil
			},
		}
		config.AddHostKey(signer)
		listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		assert.Nil(t, err)
		nConn, err := listener.Accept()
		assert.Nil(t, err)
		defer nConn.Close()
		conn, chans, reqs, err := ssh.NewServerConn(nConn, config)
		assert.Nil(t, err)
		defer conn.Close()

		// Ignore all client requests
		go ssh.DiscardRequests(reqs)

		// Wait for the client to connect and request a session
		rawChannel := <-chans
		assert.Equal(t, "session", rawChannel.ChannelType())
		channel, requests, err := rawChannel.Accept()
		assert.Nil(t, err)
		defer channel.Close()

		// Wait for the client to request a PTY
		req := <-requests
		assert.Equal(t, "pty-req", req.Type)
		req.Reply(true, nil)

		// Wait for the client to request a shell
		req = <-requests
		assert.Equal(t, "shell", req.Type)
		req.Reply(true, nil)

		// Read all data sent by the client
		var receivedData bytes.Buffer
		done := make(chan struct{})
		go func() {
			defer close(done)
			buffer := make([]byte, 1024)
			for {
				n, err := channel.Read(buffer)
				if err != nil {
					assert.Equal(t, io.EOF, err)
					break
				}
				receivedData.Write(buffer[:n])
			}
		}()

		select {
		case <-done:
			// Client closed the channel
		case <-time.After(5 * time.Millisecond):
			// All data should be read by now. Close the connection
		}

		*commands = strings.Split(receivedData.String(), "\n")
		if len(*commands) > 0 && (*commands)[len(*commands)-1] == "" {
			*commands = (*commands)[:len(*commands)-1] // Remove trailing newline
		}

		// Send an exit command to cleanly close the session
		channel.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
	}()
	time.Sleep(100 * time.Millisecond) // Give it some time to open the socket.
}
