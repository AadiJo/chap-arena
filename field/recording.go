// Records NetworkTables values in a format AdvantageScope opens: one folder per recording session, and one file per team
// inside it, named for the team number (254.wpilog or 254.csv).
//
// WPILOG (recording_wpilog.go) keeps every value with its type, as WPILib's DataLogManager does, so poses and other
// structs work in AdvantageScope's field and swerve views. CSV (recording_csv.go) is AdvantageScope's own CSV layout,
// for line graphs, tables and spreadsheets. Both name each topic "NT:" plus its path, as robot logs do, so AdvantageScope
// layouts made for a team's own logs work on these too.
//
// Timestamps are microseconds since the recording started, on this computer's clock (a robot's own clock restarts when
// it reboots), so the team files in a session share one timeline and line up when opened together in AdvantageScope.
// Each file starts with a systemTime value giving this computer's wall clock time in microseconds since 1970, which
// AdvantageScope also reads, so any timestamp t is at systemTime's value minus its timestamp plus t.

package field

import (
	"bufio"
	"fmt"
	"github.com/Team254/cheesy-arena/nt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// Recording formats, as stored in model.EventSettings.RecordingFormat.
const (
	RecordingWpilog        = "wpilog"
	RecordingCsv           = "csv"
	DefaultRecordingFormat = RecordingWpilog
)

const (
	ntKeyPrefix   = "NT:"        // Before each topic's path in a recording, as in DataLogManager's logs.
	systemTimeKey = "systemTime" // DataLogManager's name for the wall clock, in microseconds since 1970.
)

// Topics holding struct and protobuf schemas, which AdvantageScope needs to decode a robot's structs: WPILib publishes
// them under /.schema/, and AdvantageKit puts its own under its /AdvantageKit table.
var (
	schemaTopicPrefixes = []string{"/.schema/", "/AdvantageKit/.schema/"}
	schemaTopic         = regexp.MustCompile(`^(/AdvantageKit)?/\.schema/`)
)

type recorder struct {
	dir       string
	format    string
	startedAt time.Time
	values    int
	files     map[int]*teamFile           // By team id, created on the team's first value.
	schemas   map[int]map[string]nt.Value // Each team's latest schemas, by topic name. Kept only for WPILOG.
}

// One team's file in a recording.
type teamFile struct {
	file   *os.File
	buffer *bufio.Writer
	log    teamLog
}

// Writes values to a team's file in one format.
type teamLog interface {
	// Appends a value; timestamp is microseconds since the recording started.
	write(timestamp int64, value nt.Value) error
}

func validRecordingFormat(format string) bool {
	return format == RecordingWpilog || format == RecordingCsv
}

// Creates a new session folder under recordingsDir, named for the current time.
func startRecorder(recordingsDir, format string) (*recorder, error) {
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
		dir: absoluteDir, format: format, startedAt: startedAt, files: map[int]*teamFile{},
		schemas: map[int]map[string]nt.Value{},
	}, nil
}

// Appends a value, creating the team's file on its first value.
func (recorder *recorder) write(teamId int, value nt.Value) error {
	timestamp := recorder.timestamp(value.Time)
	file, err := recorder.file(teamId, timestamp)
	if err != nil {
		return err
	}
	recorder.values++
	return file.log.write(timestamp, value)
}

// Keeps a schema the robot published (see schemaTopic) for the team's WPILOG, writing it now if the team's file exists
// and otherwise once it's created. Schemas alone don't create a file. A CSV has no use for them.
func (recorder *recorder) writeSchema(teamId int, value nt.Value) error {
	if recorder.format != RecordingWpilog {
		return nil
	}
	if recorder.schemas[teamId] == nil {
		recorder.schemas[teamId] = map[string]nt.Value{}
	}
	recorder.schemas[teamId][value.Topic.Name] = value
	if file := recorder.files[teamId]; file != nil {
		return file.log.write(recorder.timestamp(value.Time), value)
	}
	return nil
}

// Microseconds from the start of the recording to t, or 0 for a value the robot stamped just before it started.
func (recorder *recorder) timestamp(t time.Time) int64 {
	return max(0, t.Sub(recorder.startedAt).Microseconds())
}

// Returns the team's file, creating it with its header, systemTime and any schemas already received if it's new.
func (recorder *recorder) file(teamId int, timestamp int64) (*teamFile, error) {
	if file := recorder.files[teamId]; file != nil {
		return file, nil
	}
	osFile, err := os.Create(filepath.Join(recorder.dir, fmt.Sprintf("%d.%s", teamId, recorder.format)))
	if err != nil {
		return nil, err
	}
	file := &teamFile{file: osFile, buffer: bufio.NewWriter(osFile)}
	// Added before anything can fail, so close still closes the file.
	recorder.files[teamId] = file
	systemTime := recorder.startedAt.UnixMicro() + timestamp
	if recorder.format == RecordingCsv {
		file.log, err = newCsvLog(file.buffer, timestamp, systemTime)
	} else {
		file.log, err = newWpilog(file.buffer, timestamp, systemTime)
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(recorder.schemas[teamId]))
	for name := range recorder.schemas[teamId] {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := file.log.write(timestamp, recorder.schemas[teamId][name]); err != nil {
			return nil, err
		}
	}
	return file, nil
}

func (recorder *recorder) flush() error {
	for _, file := range recorder.files {
		if err := file.buffer.Flush(); err != nil {
			return err
		}
	}
	return nil
}

// Flushes and closes every file. Returns the first error but closes them all regardless.
func (recorder *recorder) close() error {
	var firstErr error
	for _, file := range recorder.files {
		if err := file.buffer.Flush(); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := file.file.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
