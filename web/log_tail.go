// Copyright 2026 Team 254. All Rights Reserved.
//
// Keeps the most recent log lines in memory so the UI can show them.

package web

import (
	"strings"
	"sync"
)

// LogTail is an io.Writer for log.SetOutput (usually alongside stderr and the log file) that retains the last few
// lines.
type LogTail struct {
	mutex    sync.Mutex
	maxLines int
	lines    []string
}

func NewLogTail(maxLines int) *LogTail {
	return &LogTail{maxLines: maxLines}
}

func (logTail *LogTail) Write(p []byte) (int, error) {
	logTail.mutex.Lock()
	defer logTail.mutex.Unlock()
	logTail.lines = append(logTail.lines, strings.Split(strings.TrimRight(string(p), "\n"), "\n")...)
	if overflow := len(logTail.lines) - logTail.maxLines; overflow > 0 {
		logTail.lines = append([]string(nil), logTail.lines[overflow:]...)
	}
	return len(p), nil
}

// Returns a copy of the retained lines, oldest first.
func (logTail *LogTail) Lines() []string {
	logTail.mutex.Lock()
	defer logTail.mutex.Unlock()
	return append([]string{}, logTail.lines...)
}
