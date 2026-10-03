// Minimal NetworkTables 4 client: connects to a robot's NT server, subscribes to topics, and reports announcements and
// values with the robot's timestamps converted to local time. It never publishes. Protocol reference:
// https://github.com/wpilibsuite/allwpilib/blob/main/ntcore/doc/networktables4.adoc

package nt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/coder/websocket"
	"github.com/vmihailenco/msgpack/v5"
	"github.com/vmihailenco/msgpack/v5/msgpcode"
	"io"
	"net/url"
	"sync"
	"time"
)

const (
	Port = 5810
	// Offered in order of preference; 4.1 servers keep the connection alive with WebSocket pings.
	subprotocol41 = "v4.1.networktables.first.wpi.edu"
	subprotocol40 = "networktables.first.wpi.edu"
	// Time sync keeps the offset from the lowest round trip among this many recent samples.
	timeSyncSamples = 8
	// NT4 data type index for an int, used in time sync messages.
	intTypeIndex = 2
)

// RobotAddress is where a team's robot serves NetworkTables on a field network.
func RobotAddress(teamId int) string {
	return fmt.Sprintf("10.%d.%d.2:%d", teamId/100, teamId%100, Port)
}

type Topic struct {
	Id   int
	Name string
	Type string
}

// Event is one of Announce, Unannounce or Value.
type Event interface{ ntEvent() }

// The server made a topic known to this client.
type Announce struct{ Topic Topic }

// The topic no longer exists on the server.
type Unannounce struct{ Topic Topic }

// A new value for a subscribed topic.
type Value struct {
	Topic Topic
	// Microseconds on the robot's clock, which starts at robot boot.
	RobotTime int64
	// RobotTime on this computer's wall clock, worked out when the value arrived: the time then, less how long ago the
	// robot stamped it. Following the wall clock at arrival means a change to this computer's clock (an NTP step, say)
	// shows up in the very next value, the same as in anything else timestamped here, like camera frames.
	Time time.Time
	// See decode for the Go type each NT type decodes to.
	Data any
}

func (Announce) ntEvent()   {}
func (Unannounce) ntEvent() {}
func (Value) ntEvent()      {}

// SubscribeOptions are NT4's subscription options. Periodic is in seconds; the server's default is 0.1.
type SubscribeOptions struct {
	Periodic   float64 `json:"periodic,omitempty"`
	All        bool    `json:"all,omitempty"`
	TopicsOnly bool    `json:"topicsonly,omitempty"`
	Prefix     bool    `json:"prefix,omitempty"`
}

type syncSample struct {
	roundTrip int64 // µs
	offset    int64 // robot time minus local monotonic time, µs
}

// Conn is one connection to a robot's NT server. Events arrive on Events() until the connection ends.
type Conn struct {
	ws        *websocket.Conn
	events    chan Event
	err       error         // Why the connection ended; set before events is closed.
	done      chan struct{} // Closed by Close, so the reader never blocks on events nobody will read.
	closeOnce sync.Once
	// Clock sync runs on time elapsed since this, which is monotonic, so changes to the wall clock can't skew it.
	epoch time.Time

	// Guards everything below.
	mutex       sync.Mutex
	topics      map[int]Topic
	syncSamples []syncSample
	nextSubuid  int
}

// Connects to the NT server at address (host:port) and syncs clocks before returning, so every value can be
// converted to local time.
func Dial(ctx context.Context, address, clientName string) (*Conn, error) {
	ws, _, err := websocket.Dial(
		ctx, "ws://"+address+"/nt/"+url.PathEscape(clientName),
		&websocket.DialOptions{Subprotocols: []string{subprotocol41, subprotocol40}},
	)
	if err != nil {
		return nil, err
	}
	if ws.Subprotocol() == "" {
		ws.CloseNow()
		return nil, errors.New("server doesn't speak NetworkTables 4")
	}
	// Announcements for a robot with many topics can arrive in one large frame.
	ws.SetReadLimit(64 << 20)
	conn := &Conn{
		ws: ws, events: make(chan Event, 1024), done: make(chan struct{}), epoch: time.Now(), topics: map[int]Topic{},
		nextSubuid: 1,
	}

	if err := conn.SyncTime(ctx); err != nil {
		ws.CloseNow()
		return nil, err
	}
	for !conn.synced() {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			ws.CloseNow()
			return nil, fmt.Errorf("no time sync reply: %w", err)
		}
		if err := conn.handle(typ, data); err != nil {
			ws.CloseNow()
			return nil, err
		}
	}
	go conn.readLoop()
	return conn, nil
}

// Delivers events until the connection ends, then is closed. Err says why it ended.
func (conn *Conn) Events() <-chan Event {
	return conn.events
}

// Why the connection ended. Only valid once Events() is closed.
func (conn *Conn) Err() error {
	return conn.err
}

// Closes the connection. Events() is closed shortly after, without delivering anything still queued.
func (conn *Conn) Close() {
	conn.closeOnce.Do(func() { close(conn.done) })
	conn.ws.CloseNow()
}

// Queues an event unless the connection has been closed.
func (conn *Conn) emit(event Event) {
	select {
	case conn.events <- event:
	case <-conn.done:
	}
}

// Subscribes to the given topic names (or prefixes, with Prefix) and returns the subscription id for Unsubscribe.
func (conn *Conn) Subscribe(ctx context.Context, topics []string, options SubscribeOptions) (int, error) {
	conn.mutex.Lock()
	subuid := conn.nextSubuid
	conn.nextSubuid++
	conn.mutex.Unlock()
	params := map[string]any{"topics": topics, "subuid": subuid, "options": options}
	return subuid, conn.sendText(ctx, "subscribe", params)
}

func (conn *Conn) Unsubscribe(ctx context.Context, subuid int) error {
	return conn.sendText(ctx, "unsubscribe", map[string]any{"subuid": subuid})
}

// Sends a time sync request. The reply updates the clock offset when it arrives. Call it every few seconds: it also
// keeps a 4.0 connection alive and tracks drift between the two clocks.
func (conn *Conn) SyncTime(ctx context.Context) error {
	var message bytes.Buffer
	encoder := msgpack.NewEncoder(&message)
	_ = encoder.EncodeArrayLen(4)
	_ = encoder.EncodeInt(-1)
	_ = encoder.EncodeInt(0)
	_ = encoder.EncodeInt(intTypeIndex)
	_ = encoder.EncodeInt(conn.monotonicMicros(time.Now()))
	return conn.ws.Write(ctx, websocket.MessageBinary, message.Bytes())
}

func (conn *Conn) sendText(ctx context.Context, method string, params any) error {
	message, err := json.Marshal([]map[string]any{{"method": method, "params": params}})
	if err != nil {
		return err
	}
	return conn.ws.Write(ctx, websocket.MessageText, message)
}

func (conn *Conn) readLoop() {
	defer close(conn.events)
	for {
		typ, data, err := conn.ws.Read(context.Background())
		if err == nil {
			err = conn.handle(typ, data)
		}
		if err != nil {
			conn.err = err
			conn.ws.CloseNow()
			return
		}
	}
}

func (conn *Conn) handle(typ websocket.MessageType, data []byte) error {
	if typ == websocket.MessageText {
		return conn.handleText(data)
	}
	return conn.handleBinary(data)
}

// Text frames are a JSON array of control messages. Only announce and unannounce matter to a subscriber.
func (conn *Conn) handleText(data []byte) error {
	var messages []struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(data, &messages); err != nil {
		return fmt.Errorf("bad control message: %w", err)
	}
	for _, message := range messages {
		var topic struct {
			Id   int    `json:"id"`
			Name string `json:"name"`
			Type string `json:"type"`
		}
		switch message.Method {
		case "announce", "unannounce":
			if err := json.Unmarshal(message.Params, &topic); err != nil {
				return fmt.Errorf("bad %s message: %w", message.Method, err)
			}
		default:
			continue
		}
		conn.mutex.Lock()
		if message.Method == "announce" {
			conn.topics[topic.Id] = Topic(topic)
		} else {
			delete(conn.topics, topic.Id)
		}
		conn.mutex.Unlock()
		if message.Method == "announce" {
			conn.emit(Announce{Topic: Topic(topic)})
		} else {
			conn.emit(Unannounce{Topic: Topic(topic)})
		}
	}
	return nil
}

// Binary frames hold one or more MessagePack arrays: [topic id, timestamp, type index, value]. Topic id -1 is a time
// sync reply, whose value is the local time the request was sent.
func (conn *Conn) handleBinary(data []byte) error {
	decoder := msgpack.NewDecoder(bytes.NewReader(data))
	for {
		length, err := decoder.DecodeArrayLen()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil || length != 4 {
			return fmt.Errorf("bad value message (length %d): %v", length, err)
		}
		id, err := decoder.DecodeInt64()
		if err != nil {
			return fmt.Errorf("bad topic id: %w", err)
		}
		timestamp, err := decoder.DecodeInt64()
		if err != nil {
			return fmt.Errorf("bad timestamp: %w", err)
		}
		if _, err := decoder.DecodeInt64(); err != nil {
			return fmt.Errorf("bad type index: %w", err)
		}
		raw, err := decodeValue(decoder)
		if err != nil {
			return fmt.Errorf("bad value: %w", err)
		}

		if id == -1 {
			sentAt, ok := asInt(raw)
			if ok {
				conn.addSyncSample(timestamp, sentAt, conn.monotonicMicros(time.Now()))
			}
			continue
		}
		conn.mutex.Lock()
		topic, ok := conn.topics[int(id)]
		offset := conn.offset()
		conn.mutex.Unlock()
		if ok {
			now := time.Now()
			age := time.Duration(conn.monotonicMicros(now)+offset-timestamp) * time.Microsecond
			conn.emit(
				Value{
					Topic: topic, RobotTime: timestamp, Time: now.Add(-age).Round(0),
					Data: decode(topic.Type, raw),
				},
			)
		}
	}
}

// Decodes one value loosely (see decode), except that binary stays []byte; the library's loose decoding would turn it
// into a string.
func decodeValue(decoder *msgpack.Decoder) (any, error) {
	code, err := decoder.PeekCode()
	if err != nil {
		return nil, err
	}
	if code == msgpcode.Bin8 || code == msgpcode.Bin16 || code == msgpcode.Bin32 {
		return decoder.DecodeBytes()
	}
	return decoder.DecodeInterfaceLoose()
}

// Microseconds from the connection's epoch to t, on the monotonic clock. t must come from time.Now().
func (conn *Conn) monotonicMicros(t time.Time) int64 {
	return t.Sub(conn.epoch).Microseconds()
}

// Takes the server's reply time as half way through the round trip. All three times are microseconds: serverTime on
// the robot's clock, the other two from monotonicMicros.
func (conn *Conn) addSyncSample(serverTime, sentAt, receivedAt int64) {
	roundTrip := receivedAt - sentAt
	conn.mutex.Lock()
	defer conn.mutex.Unlock()
	conn.syncSamples = append(conn.syncSamples, syncSample{roundTrip, serverTime + roundTrip/2 - receivedAt})
	if len(conn.syncSamples) > timeSyncSamples {
		conn.syncSamples = conn.syncSamples[1:]
	}
}

func (conn *Conn) synced() bool {
	conn.mutex.Lock()
	defer conn.mutex.Unlock()
	return len(conn.syncSamples) > 0
}

// Offset from the recent sample with the shortest round trip, the least affected by queueing. Caller holds the mutex.
func (conn *Conn) offset() int64 {
	best := conn.syncSamples[0]
	for _, sample := range conn.syncSamples[1:] {
		if sample.roundTrip < best.roundTrip {
			best = sample
		}
	}
	return best.offset
}
