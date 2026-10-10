// Writes AdvantageScope's CSV layout: the "list" format its CSV export produces and its import reads. That's a
// Timestamp,Key,Value header, then a row per value with the timestamp in seconds, keys like a WPILOG's, and the value as
// JSON (arrays as [1; 2; 3]).
//
// AdvantageScope splits rows on every comma, with no quoting, and arrays on "; ", so commas and semicolons inside
// strings are written as JSON escapes (\u002c and \u003b), which read back as the original characters; keys can't be
// escaped, so a comma in a topic's path is written as a semicolon. Raw bytes are hex pairs joined by dashes, as
// AdvantageScope writes them. Numbers that aren't finite are null, as in its export, since JSON has no NaN. A CSV can't
// say a value is a Pose2d, so a pose is written as the fields AdvantageScope shows for a decoded one: /translation/x,
// /translation/y and /rotation/value under the topic's key, with /length and /0, /1, ... for an array of them.

package field

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/Team254/cheesy-arena/nt"
	"io"
	"math"
	"strconv"
	"strings"
)

var (
	csvKeyReplacer    = strings.NewReplacer(",", ";", "\r", " ", "\n", " ")
	csvStringReplacer = strings.NewReplacer(",", `\u002c`, ";", `\u003b`)
)

type csvLog struct {
	writer io.Writer
}

// A row's key suffix (after the topic's key) and value.
type csvCell struct {
	suffix string
	value  string
}

// Writes the header and a systemTime value.
func newCsvLog(writer io.Writer, timestamp, systemTime int64) (*csvLog, error) {
	if _, err := io.WriteString(writer, "Timestamp,Key,Value\n"); err != nil {
		return nil, err
	}
	log := &csvLog{writer: writer}
	return log, log.row(timestamp, systemTimeKey, strconv.FormatInt(systemTime, 10))
}

func (log *csvLog) write(timestamp int64, value nt.Value) error {
	key := ntKeyPrefix + value.Topic.Name
	for _, cell := range csvCells(value.Data) {
		if err := log.row(timestamp, key+cell.suffix, cell.value); err != nil {
			return err
		}
	}
	return nil
}

func (log *csvLog) row(timestamp int64, key, value string) error {
	_, err := fmt.Fprintf(log.writer, "%d.%06d,%s,%s\n", timestamp/1e6, timestamp%1e6, csvKeyReplacer.Replace(key), value)
	return err
}

// The rows for one value.
func csvCells(data any) []csvCell {
	switch value := data.(type) {
	case nt.Pose2d:
		return poseCells("", value)
	case []nt.Pose2d:
		cells := []csvCell{{"/length", strconv.Itoa(len(value))}}
		for i, pose := range value {
			cells = append(cells, poseCells("/"+strconv.Itoa(i), pose)...)
		}
		return cells
	case []byte:
		if len(value) == 0 {
			return []csvCell{{"", "(empty)"}}
		}
		pairs := make([]string, len(value))
		for i, item := range value {
			pairs[i] = hex.EncodeToString([]byte{item})
		}
		return []csvCell{{"", strings.Join(pairs, "-")}}
	case []bool:
		return []csvCell{{"", csvArray(value, strconv.FormatBool)}}
	case []float64:
		return []csvCell{{"", csvArray(value, csvNumber)}}
	case []int64:
		return []csvCell{{"", csvArray(value, func(item int64) string { return strconv.FormatInt(item, 10) })}}
	case []string:
		return []csvCell{{"", csvArray(value, csvString)}}
	case bool:
		return []csvCell{{"", strconv.FormatBool(value)}}
	case float64:
		return []csvCell{{"", csvNumber(value)}}
	case int64:
		return []csvCell{{"", strconv.FormatInt(value, 10)}}
	case string:
		return []csvCell{{"", csvString(value)}}
	}
	// Anything else is a value that didn't fit its topic's type.
	return []csvCell{{"", csvString(jsonString(data))}}
}

func poseCells(prefix string, pose nt.Pose2d) []csvCell {
	return []csvCell{
		{prefix + "/translation/x", csvNumber(pose.X)},
		{prefix + "/translation/y", csvNumber(pose.Y)},
		{prefix + "/rotation/value", csvNumber(pose.Rotation)},
	}
}

func csvArray[T any](values []T, format func(T) string) string {
	items := make([]string, len(values))
	for i, value := range values {
		items[i] = format(value)
	}
	return "[" + strings.Join(items, "; ") + "]"
}

// The shortest form that reads back exactly.
func csvNumber(value float64) string {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "null"
	}
	return strconv.FormatFloat(value, 'g', -1, 64)
}

func csvString(value string) string {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
	return csvStringReplacer.Replace(strings.TrimSuffix(buffer.String(), "\n"))
}

func jsonString(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(data)
}
