// Writes WPILib's data log format, WPILOG 1.0, as specified in allwpilib's datalog/doc/datalog.adoc.
//
// Each topic is an entry named "NT:" plus its path, typed as WPILib's DataLogManager types NetworkTables topics: the
// topic's own type, with int as int64. Floats are widened to doubles. Poses are written back to the struct:Pose2d bytes
// they were decoded from, and other structs and protobufs keep the bytes and type the robot sent, so with the robot's
// schemas (see schemaTopic) AdvantageScope decodes them all. A value that doesn't fit its entry's type, as when a robot
// is redeployed with a topic's type changed, finishes the entry and starts a new one under the same name.

package field

import (
	"encoding/binary"
	"github.com/Team254/cheesy-arena/nt"
	"io"
	"math"
)

const (
	wpilogExtraHeader = "chap-arena"
	wpilogControlId   = 0
	wpilogStart       = 0
	wpilogFinish      = 1
)

type wpilog struct {
	writer  io.Writer
	entries map[string]wpilogEntry // By name.
	nextId  uint32
}

type wpilogEntry struct {
	id       uint32
	typeName string
}

// Writes the file header and a systemTime value.
func newWpilog(writer io.Writer, timestamp, systemTime int64) (*wpilog, error) {
	header := append([]byte("WPILOG"), 0x00, 0x01) // Version 1.0, minor byte first.
	header = binary.LittleEndian.AppendUint32(header, uint32(len(wpilogExtraHeader)))
	header = append(header, wpilogExtraHeader...)
	if _, err := writer.Write(header); err != nil {
		return nil, err
	}
	log := &wpilog{writer: writer, entries: map[string]wpilogEntry{}, nextId: 1}
	return log, log.append(systemTimeKey, "int64", timestamp, binary.LittleEndian.AppendUint64(nil, uint64(systemTime)))
}

func (log *wpilog) write(timestamp int64, value nt.Value) error {
	typeName, payload := wpilogValue(value.Topic.Type, value.Data)
	return log.append(ntKeyPrefix+value.Topic.Name, typeName, timestamp, payload)
}

// Appends a value to the named entry, first starting the entry if it's new or has a different type.
func (log *wpilog) append(name, typeName string, timestamp int64, payload []byte) error {
	entry, ok := log.entries[name]
	if !ok || entry.typeName != typeName {
		if ok {
			finish := binary.LittleEndian.AppendUint32([]byte{wpilogFinish}, entry.id)
			if err := log.record(wpilogControlId, timestamp, finish); err != nil {
				return err
			}
		}
		entry = wpilogEntry{id: log.nextId, typeName: typeName}
		log.nextId++
		log.entries[name] = entry
		start := binary.LittleEndian.AppendUint32([]byte{wpilogStart}, entry.id)
		for _, field := range []string{name, typeName, ""} { // The last is the entry's metadata.
			start = binary.LittleEndian.AppendUint32(start, uint32(len(field)))
			start = append(start, field...)
		}
		if err := log.record(wpilogControlId, timestamp, start); err != nil {
			return err
		}
	}
	return log.record(entry.id, timestamp, payload)
}

// Writes one record, with the entry id, payload size and timestamp each in as few bytes as they fit in. The first byte
// gives those lengths, less one: two bits each for the id and size, then three for the timestamp.
func (log *wpilog) record(entryId uint32, timestamp int64, payload []byte) error {
	idLength := byteLength(uint64(entryId), 4)
	sizeLength := byteLength(uint64(len(payload)), 4)
	timestampLength := byteLength(uint64(timestamp), 8)
	header := []byte{byte(idLength-1) | byte(sizeLength-1)<<2 | byte(timestampLength-1)<<4}
	header = appendLittleEndian(header, uint64(entryId), idLength)
	header = appendLittleEndian(header, uint64(len(payload)), sizeLength)
	header = appendLittleEndian(header, uint64(timestamp), timestampLength)
	if _, err := log.writer.Write(header); err != nil {
		return err
	}
	_, err := log.writer.Write(payload)
	return err
}

// The fewest bytes, at least one and at most limit, that hold value.
func byteLength(value uint64, limit int) int {
	length := 1
	for length < limit && value>>(8*length) != 0 {
		length++
	}
	return length
}

func appendLittleEndian(data []byte, value uint64, length int) []byte {
	for i := 0; i < length; i++ {
		data = append(data, byte(value>>(8*i)))
	}
	return data
}

// The WPILOG type and payload for a value decoded from a topic of the given NT type.
func wpilogValue(ntType string, data any) (string, []byte) {
	le := binary.LittleEndian
	switch value := data.(type) {
	case bool:
		return "boolean", []byte{boolByte(value)}
	case float64:
		return "double", le.AppendUint64(nil, math.Float64bits(value))
	case int64:
		return "int64", le.AppendUint64(nil, uint64(value))
	case string:
		if ntType == "json" {
			return "json", []byte(value)
		}
		return "string", []byte(value)
	case []bool:
		payload := make([]byte, len(value))
		for i, item := range value {
			payload[i] = boolByte(item)
		}
		return "boolean[]", payload
	case []float64:
		payload := make([]byte, 0, 8*len(value))
		for _, item := range value {
			payload = le.AppendUint64(payload, math.Float64bits(item))
		}
		return "double[]", payload
	case []int64:
		payload := make([]byte, 0, 8*len(value))
		for _, item := range value {
			payload = le.AppendUint64(payload, uint64(item))
		}
		return "int64[]", payload
	case []string:
		payload := le.AppendUint32(nil, uint32(len(value)))
		for _, item := range value {
			payload = le.AppendUint32(payload, uint32(len(item)))
			payload = append(payload, item...)
		}
		return "string[]", payload
	case nt.Pose2d:
		return "struct:Pose2d", appendPose2d(nil, value)
	case []nt.Pose2d:
		payload := make([]byte, 0, 24*len(value))
		for _, pose := range value {
			payload = appendPose2d(payload, pose)
		}
		return "struct:Pose2d[]", payload
	case []byte:
		// Undecoded types keep their NT type. A value that should have decoded but didn't is just bytes.
		if ntType == "" || nt.Decodable(ntType) {
			return "raw", value
		}
		return ntType, value
	}
	// Anything else is a value that didn't fit its topic's type.
	return "json", []byte(jsonString(data))
}

func boolByte(value bool) byte {
	if value {
		return 1
	}
	return 0
}

// Packs a pose as WPILib's struct:Pose2d: x, y, then rotation, as little-endian doubles.
func appendPose2d(data []byte, pose nt.Pose2d) []byte {
	for _, field := range []float64{pose.X, pose.Y, pose.Rotation} {
		data = binary.LittleEndian.AppendUint64(data, math.Float64bits(field))
	}
	return data
}
