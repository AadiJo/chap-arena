// Decodes NT4 values into Go types, including the WPILib structs this app understands.

package nt

import (
	"encoding/binary"
	"math"
)

// Pose2d is WPILib's struct:Pose2d: meters, meters, and radians counterclockwise.
type Pose2d struct {
	X        float64
	Y        float64
	Rotation float64
}

const pose2dBytes = 24

// Decodable reports whether values of an NT type name decode to something other than raw bytes. Anything else is
// passed through as it came (raw bytes for other structs and protobufs).
func Decodable(typeName string) bool {
	switch typeName {
	case "boolean", "double", "float", "int", "string", "json", "boolean[]", "double[]", "float[]", "int[]", "string[]",
		"struct:Pose2d", "struct:Pose2d[]":
		return true
	}
	return false
}

// Converts a loosely decoded MessagePack value to the Go type for its NT type: bool, float64 (double and float),
// int64, string (string and json), []bool, []float64, []int64, []string, Pose2d or []Pose2d. A value that doesn't fit
// its declared type, or has a type Decodable rejects, is returned as it came.
func decode(typeName string, raw any) any {
	switch typeName {
	case "boolean":
		if value, ok := raw.(bool); ok {
			return value
		}
	case "double", "float":
		if value, ok := asFloat(raw); ok {
			return value
		}
	case "int":
		if value, ok := asInt(raw); ok {
			return value
		}
	case "string", "json":
		if value, ok := raw.(string); ok {
			return value
		}
	case "boolean[]":
		if values, ok := decodeArray(raw, func(item any) (bool, bool) { value, ok := item.(bool); return value, ok }); ok {
			return values
		}
	case "double[]", "float[]":
		if values, ok := decodeArray(raw, asFloat); ok {
			return values
		}
	case "int[]":
		if values, ok := decodeArray(raw, asInt); ok {
			return values
		}
	case "string[]":
		if values, ok := decodeArray(raw, func(item any) (string, bool) { value, ok := item.(string); return value, ok }); ok {
			return values
		}
	case "struct:Pose2d":
		if data, ok := raw.([]byte); ok && len(data) == pose2dBytes {
			return decodePose2d(data)
		}
	case "struct:Pose2d[]":
		if data, ok := raw.([]byte); ok && len(data)%pose2dBytes == 0 {
			poses := make([]Pose2d, len(data)/pose2dBytes)
			for i := range poses {
				poses[i] = decodePose2d(data[i*pose2dBytes:])
			}
			return poses
		}
	}
	return raw
}

// Structs are packed little-endian in field order: Translation2d (x, y), then Rotation2d (radians).
func decodePose2d(data []byte) Pose2d {
	field := func(i int) float64 { return math.Float64frombits(binary.LittleEndian.Uint64(data[i*8:])) }
	return Pose2d{X: field(0), Y: field(1), Rotation: field(2)}
}

func decodeArray[T any](raw any, convert func(any) (T, bool)) ([]T, bool) {
	items, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	values := make([]T, len(items))
	for i, item := range items {
		if values[i], ok = convert(item); !ok {
			return nil, false
		}
	}
	return values, true
}

// MessagePack integers decode loosely as int64 or uint64; floats as float64.
func asInt(raw any) (int64, bool) {
	switch value := raw.(type) {
	case int64:
		return value, true
	case uint64:
		return int64(value), value <= math.MaxInt64
	}
	return 0, false
}

// A double topic can carry an integer-encoded value (e.g. 0), so both are accepted.
func asFloat(raw any) (float64, bool) {
	if value, ok := raw.(float64); ok {
		return value, true
	}
	value, ok := asInt(raw)
	return float64(value), ok
}
