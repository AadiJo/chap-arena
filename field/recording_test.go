package field

import (
	"bytes"
	"encoding/binary"
	"github.com/Team254/cheesy-arena/nt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A data record read back from a WPILOG, with its entry's name and type from the Start record.
type wpilogRecord struct {
	name      string
	typeName  string
	timestamp int64
	payload   []byte
}

// Reads a WPILOG as the spec describes it, independently of the writer. Returns the extra header, the data records, and
// a line per control record ("start 1 name type" or "finish 1").
func readWpilog(t *testing.T, data []byte) (string, []wpilogRecord, []string) {
	require.Equal(t, "WPILOG\x00\x01", string(data[:8]))
	headerLength := binary.LittleEndian.Uint32(data[8:])
	extraHeader := string(data[12 : 12+headerLength])
	data = data[12+headerLength:]
	next := func(length int) []byte {
		require.GreaterOrEqual(t, len(data), length, "truncated record")
		field := data[:length]
		data = data[length:]
		return field
	}
	number := func(length int) uint64 {
		var value uint64
		for i, b := range next(length) {
			value |= uint64(b) << (8 * i)
		}
		return value
	}
	text := func() string { return string(next(int(number(4)))) }

	entries := map[uint64][2]string{}
	var records []wpilogRecord
	var controls []string
	for len(data) > 0 {
		lengths := next(1)[0]
		require.Zero(t, lengths&0x80, "spare bit set")
		entryId := number(int(lengths&3) + 1)
		size := number(int(lengths>>2&3) + 1)
		timestamp := int64(number(int(lengths>>4&7) + 1))
		payload := next(int(size))
		if entryId != 0 {
			entry, ok := entries[entryId]
			require.True(t, ok, "record for entry %d, which isn't started", entryId)
			records = append(records, wpilogRecord{entry[0], entry[1], timestamp, payload})
			continue
		}
		saved := data
		data = payload
		switch next(1)[0] {
		case 0:
			id := number(4)
			name, typeName, metadata := text(), text(), text()
			assert.Empty(t, metadata)
			entries[id] = [2]string{name, typeName}
			controls = append(controls, strings.Join([]string{"start", strconv.FormatUint(id, 10), name, typeName}, " "))
		case 1:
			id := number(4)
			delete(entries, id)
			controls = append(controls, "finish "+strconv.FormatUint(id, 10))
		default:
			t.Fatalf("unknown control record %v", payload)
		}
		require.Empty(t, data, "control record has extra bytes")
		data = saved
	}
	return extraHeader, records, controls
}

func le64(value uint64) []byte   { return binary.LittleEndian.AppendUint64(nil, value) }
func leF64(value float64) []byte { return le64(math.Float64bits(value)) }

func TestWpilogRecord(t *testing.T) {
	for _, test := range []struct {
		name      string
		entryId   uint32
		timestamp int64
		payload   []byte
		expected  []byte
	}{
		{
			// The example in the spec: int64 3 on entry 1 at 1 s.
			"spec example", 1, 1_000_000, le64(3),
			[]byte{0x20, 0x01, 0x08, 0x40, 0x42, 0x0f, 0x03, 0, 0, 0, 0, 0, 0, 0},
		},
		{"zero timestamp", 1, 0, []byte{1}, []byte{0x00, 0x01, 0x01, 0x00, 0x01}},
		{"two byte id", 256, 5, []byte{}, []byte{0x01, 0x00, 0x01, 0x00, 0x05}},
		{"four byte id", 1 << 24, 5, []byte{}, []byte{0x03, 0, 0, 0, 1, 0x00, 0x05}},
		{
			"two byte size, eight byte timestamp", 1, math.MaxInt64, make([]byte, 300),
			append([]byte{0x74, 0x01, 0x2c, 0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}, make([]byte, 300)...),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var buffer bytes.Buffer
			log := &wpilog{writer: &buffer}
			require.Nil(t, log.record(test.entryId, test.timestamp, test.payload))
			assert.Equal(t, test.expected, buffer.Bytes())
		})
	}
}

func TestWpilogValue(t *testing.T) {
	for _, test := range []struct {
		ntType          string
		data            any
		expectedType    string
		expectedPayload []byte
	}{
		{"boolean", true, "boolean", []byte{1}},
		{"double", 1.5, "double", leF64(1.5)},
		{"float", 1.5, "double", leF64(1.5)},
		{"int", int64(-2), "int64", le64(math.MaxUint64 - 1)},
		{"string", "INTAKING", "string", []byte("INTAKING")},
		{"json", `{"a":1}`, "json", []byte(`{"a":1}`)},
		{"boolean[]", []bool{true, false}, "boolean[]", []byte{1, 0}},
		{"double[]", []float64{1.5, -2}, "double[]", append(leF64(1.5), leF64(-2)...)},
		{"int[]", []int64{7}, "int64[]", le64(7)},
		{"string[]", []string{}, "string[]", []byte{0, 0, 0, 0}},
		{"string[]", []string{"ab", ""}, "string[]", []byte{2, 0, 0, 0, 2, 0, 0, 0, 'a', 'b', 0, 0, 0, 0}},
		{
			"struct:Pose2d", nt.Pose2d{X: 1.5, Y: -2.25, Rotation: 0.5}, "struct:Pose2d",
			append(append(leF64(1.5), leF64(-2.25)...), leF64(0.5)...),
		},
		{
			"struct:Pose2d[]", []nt.Pose2d{{X: 1}, {Y: 2}}, "struct:Pose2d[]",
			bytes.Join([][]byte{leF64(1), leF64(0), leF64(0), leF64(0), leF64(2), leF64(0)}, nil),
		},
		{"struct:Pose2d[]", []nt.Pose2d{}, "struct:Pose2d[]", []byte{}},
		// Undecoded types keep their bytes and type.
		{"struct:SwerveModuleState[]", []byte{1, 2}, "struct:SwerveModuleState[]", []byte{1, 2}},
		{"proto:wpi.proto.ProtobufPose3d", []byte{3}, "proto:wpi.proto.ProtobufPose3d", []byte{3}},
		{"structschema", []byte("double x;double y"), "structschema", []byte("double x;double y")},
		{"raw", []byte{4}, "raw", []byte{4}},
		// A struct:Pose2d of the wrong length didn't decode, so it isn't a Pose2d.
		{"struct:Pose2d", []byte{5}, "raw", []byte{5}},
		// Values that don't fit their type.
		{"double", "oops", "string", []byte("oops")},
		{"double[]", []any{1.5, "x"}, "json", []byte(`[1.5,"x"]`)},
	} {
		typeName, payload := wpilogValue(test.ntType, test.data)
		assert.Equal(t, test.expectedType, typeName, "%s %v", test.ntType, test.data)
		assert.Equal(t, test.expectedPayload, payload, "%s %v", test.ntType, test.data)
	}
}

func TestCsvCells(t *testing.T) {
	cell := func(value string) []csvCell { return []csvCell{{"", value}} }
	for _, test := range []struct {
		data     any
		expected []csvCell
	}{
		{true, cell("true")},
		{4180.5, cell("4180.5")},
		{1e-7, cell("1e-07")},
		{math.NaN(), cell("null")},
		{math.Inf(-1), cell("null")},
		{int64(-3), cell("-3")},
		{"INTAKING", cell(`"INTAKING"`)},
		// Commas and semicolons would split the row or an array in AdvantageScope, so they're escaped.
		{`a, b; "c" <d>`, cell(`"a\u002c b\u003b \"c\" <d>"`)},
		{"line\nbreak", cell(`"line\nbreak"`)},
		{[]bool{true, false}, cell("[true; false]")},
		{[]float64{1.5, math.NaN()}, cell("[1.5; null]")},
		{[]float64{}, cell("[]")},
		{[]int64{1, 2}, cell("[1; 2]")},
		{[]string{"a,b", "c"}, cell(`["a\u002cb"; "c"]`)},
		{[]byte{0x0a, 0xff, 0x00}, cell("0a-ff-00")},
		{[]byte{}, cell("(empty)")},
		{
			nt.Pose2d{X: 1.5, Y: -2.25, Rotation: 0.5},
			[]csvCell{{"/translation/x", "1.5"}, {"/translation/y", "-2.25"}, {"/rotation/value", "0.5"}},
		},
		{
			[]nt.Pose2d{{X: 1}, {Y: 2}},
			[]csvCell{
				{"/length", "2"},
				{"/0/translation/x", "1"}, {"/0/translation/y", "0"}, {"/0/rotation/value", "0"},
				{"/1/translation/x", "0"}, {"/1/translation/y", "2"}, {"/1/rotation/value", "0"},
			},
		},
		{map[string]any{"a": 1}, cell(`"{\"a\":1}"`)},
	} {
		assert.Equal(t, test.expected, csvCells(test.data), "%v", test.data)
	}
}

// A value as the NT client would deliver it, at offset from the recording's start.
func recordedValue(recorder *recorder, offset time.Duration, name, typeName string, data any) nt.Value {
	return nt.Value{
		Topic: nt.Topic{Name: name, Type: typeName}, RobotTime: 1, Time: recorder.startedAt.Add(offset), Data: data,
	}
}

func TestRecorderWpilog(t *testing.T) {
	recorder, err := startRecorder(t.TempDir(), RecordingWpilog)
	require.Nil(t, err)
	poseSchema := recordedValue(
		recorder, -time.Minute, "/.schema/struct:Pose2d", "structschema",
		[]byte("Translation2d translation;Rotation2d rotation"),
	)
	akitSchema := recordedValue(
		recorder, 2*time.Millisecond, "/AdvantageKit/.schema/struct:Rotation2d", "structschema",
		[]byte("double value"),
	)
	pose := nt.Pose2d{X: 1.5, Y: -2.25, Rotation: 0.5}

	// A schema received before the team's file exists waits for it; one received after is written straight away.
	require.Nil(t, recorder.writeSchema(254, poseSchema))
	require.Nil(t, recorder.write(254, recordedValue(recorder, -5*time.Millisecond, "/Pose", "struct:Pose2d", pose)))
	require.Nil(t, recorder.writeSchema(254, akitSchema))
	require.Nil(t, recorder.write(254, recordedValue(recorder, 3*time.Millisecond, "/Mode", "int", int64(2))))
	// The robot was redeployed with /Mode as a string.
	require.Nil(t, recorder.write(254, recordedValue(recorder, 4*time.Millisecond, "/Mode", "string", "AUTO")))
	require.Nil(t, recorder.write(1114, recordedValue(recorder, 5*time.Millisecond, "/Pose", "struct:Pose2d", pose)))
	require.Nil(t, recorder.close())
	assert.Equal(t, 4, recorder.values)

	data, err := os.ReadFile(filepath.Join(recorder.dir, "254.wpilog"))
	require.Nil(t, err)
	extraHeader, records, controls := readWpilog(t, data)
	assert.Equal(t, "chap-arena", extraHeader)
	assert.Equal(
		t, []string{
			"start 1 systemTime int64",
			"start 2 NT:/.schema/struct:Pose2d structschema",
			"start 3 NT:/Pose struct:Pose2d",
			"start 4 NT:/AdvantageKit/.schema/struct:Rotation2d structschema",
			"start 5 NT:/Mode int64",
			"finish 5",
			"start 6 NT:/Mode string",
		}, controls,
	)
	poseBytes := append(append(leF64(1.5), leF64(-2.25)...), leF64(0.5)...)
	assert.Equal(
		t, []wpilogRecord{
			// The pose was stamped before the recording started, so it and everything written with the file are at 0.
			{"systemTime", "int64", 0, le64(uint64(recorder.startedAt.UnixMicro()))},
			{"NT:/.schema/struct:Pose2d", "structschema", 0, []byte("Translation2d translation;Rotation2d rotation")},
			{"NT:/Pose", "struct:Pose2d", 0, poseBytes},
			{"NT:/AdvantageKit/.schema/struct:Rotation2d", "structschema", 2000, []byte("double value")},
			{"NT:/Mode", "int64", 3000, le64(2)},
			{"NT:/Mode", "string", 4000, []byte("AUTO")},
		}, records,
	)

	// Each team has its own file on the same timeline, with its own systemTime.
	data, err = os.ReadFile(filepath.Join(recorder.dir, "1114.wpilog"))
	require.Nil(t, err)
	_, records, _ = readWpilog(t, data)
	assert.Equal(
		t, []wpilogRecord{
			{"systemTime", "int64", 5000, le64(uint64(recorder.startedAt.UnixMicro() + 5000))},
			{"NT:/Pose", "struct:Pose2d", 5000, poseBytes},
		}, records,
	)
}

func TestRecorderCsv(t *testing.T) {
	recorder, err := startRecorder(t.TempDir(), RecordingCsv)
	require.Nil(t, err)
	require.Nil(t, recorder.writeSchema(254, recordedValue(recorder, 0, "/.schema/struct:Pose2d", "structschema", []byte("x"))))
	require.Nil(
		t, recorder.write(
			254, recordedValue(recorder, 1500*time.Millisecond, "/Pose", "struct:Pose2d", nt.Pose2d{X: 1.5, Y: -2, Rotation: 0.5}),
		),
	)
	require.Nil(t, recorder.write(254, recordedValue(recorder, 2_000_001*time.Microsecond, "/Auto, chosen", "string", "Left, far")))
	require.Nil(t, recorder.write(254, recordedValue(recorder, 12*time.Second, "/Speeds", "double[]", []float64{1, 2.5})))
	require.Nil(t, recorder.close())

	data, err := os.ReadFile(filepath.Join(recorder.dir, "254.csv"))
	require.Nil(t, err)
	assert.Equal(
		t, strings.Join(
			[]string{
				"Timestamp,Key,Value",
				"1.500000,systemTime," + strconv.FormatInt(recorder.startedAt.UnixMicro()+1_500_000, 10),
				"1.500000,NT:/Pose/translation/x,1.5",
				"1.500000,NT:/Pose/translation/y,-2",
				"1.500000,NT:/Pose/rotation/value,0.5",
				`2.000001,NT:/Auto; chosen,"Left\u002c far"`,
				"12.000000,NT:/Speeds,[1; 2.5]",
				"",
			}, "\n",
		), string(data),
	)
}
