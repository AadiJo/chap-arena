// Records NetworkTables values to CSV: one folder per recording session, one folder per team inside it, and one file
// per topic, named after the topic path.
//
// Every file starts with unix_time_us (the robot timestamp converted to this computer's clock) and robot_time_us (the
// robot's own timestamp, microseconds since it booted). Poses then have x, y (meters) and rotation (radians) columns;
// anything else has one value column: numbers and strings as they are, arrays as JSON, and raw bytes (structs other
// than Pose2d) as base64.

package field

import (
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"github.com/Team254/cheesy-arena/nt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var unsafeFileNameCharacters = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

type recorder struct {
	dir       string
	startedAt time.Time
	rows      int
	files     map[recordingKey]*recordingFile
	fileNames map[string]bool // Paths already used, relative to dir, to keep topics with similar names apart.
}

type recordingKey struct {
	teamId int
	topic  string
}

type recordingFile struct {
	file   *os.File
	writer *csv.Writer
}

// Creates a new session folder under recordingsDir, named for the current time.
func startRecorder(recordingsDir string) (*recorder, error) {
	startedAt := time.Now()
	base := filepath.Join(recordingsDir, startedAt.Format("2006-01-02_150405"))
	dir := base
	for i := 2; ; i++ {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			break
		}
		dir = fmt.Sprintf("%s-%d", base, i)
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("couldn't create recording folder: %w", err)
	}
	absoluteDir, err := filepath.Abs(dir)
	if err != nil {
		absoluteDir = dir
	}
	return &recorder{
		dir: absoluteDir, startedAt: startedAt, files: map[recordingKey]*recordingFile{}, fileNames: map[string]bool{},
	}, nil
}

// Appends a value, creating the topic's file (with a header) on its first value.
func (recorder *recorder) write(teamId int, value nt.Value) error {
	key := recordingKey{teamId, value.Topic.Name}
	file := recorder.files[key]
	if file == nil {
		var err error
		if file, err = recorder.create(key, value.Data); err != nil {
			return err
		}
		recorder.files[key] = file
	}
	row := []string{strconv.FormatInt(value.Time.UnixMicro(), 10), strconv.FormatInt(value.RobotTime, 10)}
	row = append(row, csvValue(value.Data)...)
	recorder.rows++
	return file.writer.Write(row)
}

func (recorder *recorder) create(key recordingKey, data any) (*recordingFile, error) {
	name := strings.Trim(unsafeFileNameCharacters.ReplaceAllString(key.topic, "_"), "_")
	if name == "" {
		name = "topic"
	}
	path := filepath.Join(strconv.Itoa(key.teamId), name+".csv")
	for i := 2; recorder.fileNames[path]; i++ {
		path = filepath.Join(strconv.Itoa(key.teamId), fmt.Sprintf("%s_%d.csv", name, i))
	}
	recorder.fileNames[path] = true
	if err := os.MkdirAll(filepath.Join(recorder.dir, strconv.Itoa(key.teamId)), 0755); err != nil {
		return nil, err
	}
	osFile, err := os.Create(filepath.Join(recorder.dir, path))
	if err != nil {
		return nil, err
	}
	file := &recordingFile{file: osFile, writer: csv.NewWriter(osFile)}
	header := []string{"unix_time_us", "robot_time_us", "value"}
	if _, ok := data.(nt.Pose2d); ok {
		header = []string{"unix_time_us", "robot_time_us", "x", "y", "rotation"}
	}
	return file, file.writer.Write(header)
}

func (recorder *recorder) flush() error {
	for _, file := range recorder.files {
		file.writer.Flush()
		if err := file.writer.Error(); err != nil {
			return err
		}
	}
	return nil
}

// Flushes and closes every file. Returns the first error but closes them all regardless.
func (recorder *recorder) close() error {
	firstErr := recorder.flush()
	for _, file := range recorder.files {
		if err := file.file.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// The value columns for one row. Floats use the shortest form that reads back exactly.
func csvValue(data any) []string {
	number := func(value float64) string { return strconv.FormatFloat(value, 'g', -1, 64) }
	switch value := data.(type) {
	case nt.Pose2d:
		return []string{number(value.X), number(value.Y), number(value.Rotation)}
	case []nt.Pose2d:
		poses := make([][3]float64, len(value))
		for i, pose := range value {
			poses[i] = [3]float64{pose.X, pose.Y, pose.Rotation}
		}
		return []string{jsonString(poses)}
	case float64:
		return []string{number(value)}
	case int64:
		return []string{strconv.FormatInt(value, 10)}
	case bool:
		return []string{strconv.FormatBool(value)}
	case string:
		return []string{value}
	case []byte:
		return []string{base64.StdEncoding.EncodeToString(value)}
	}
	return []string{jsonString(data)}
}

func jsonString(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(data)
}
