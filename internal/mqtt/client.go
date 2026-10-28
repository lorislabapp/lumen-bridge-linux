// Package mqtt subscribes to a Frigate broker and decodes the JSON event
// payloads into typed Event structs that downstream packages (cloudkit,
// bridge) can consume without re-parsing.
//
// Frigate publishes three topic patterns we care about:
//   - frigate/events           — JSON events for "new" / "update" / "end"
//   - frigate/{camera}/{label}/snapshot — JPEG bytes (optional, retained)
//   - frigate/{camera}/{label}/clip.mp4  — finalised MP4 (Frigate 0.13+)
//
// The mac Bridge subscribes to all three and writes them as one CKRecord
// per event ID with snapshot + clip CKAssets attached. The Linux Bridge
// follows the same pattern, just over the CloudKit Web Services REST API
// rather than the native CKContainer SDK.
package mqtt

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"time"
	"unicode"

	paho "github.com/eclipse/paho.mqtt.golang"
)

// Event mirrors the fields produced by Frigate's events topic that we
// actually persist to CloudKit. Frigate emits more (e.g. bounding boxes,
// false-positive flags) — we ignore everything that doesn't end up on the
// CKRecord schema.
type Event struct {
	ID          string // Frigate event ID — also the CKRecord.ID.recordName
	Camera      string
	Label       string
	SubLabel    string // recognised face / plate name, "" when none
	Zones       []string
	TopScore    float64
	StartTime   time.Time
	EndTime     *time.Time // nil while the event is still active
	HasClip     bool
	HasSnapshot bool
	Phase       EventPhase // new | update | end
}

type EventPhase string

const (
	PhaseNew    EventPhase = "new"
	PhaseUpdate EventPhase = "update"
	PhaseEnd    EventPhase = "end"
)

// Handler is invoked for every decoded event. Implementations should be
// fast; long-running work (e.g. CloudKit writes) should be queued, not
// done inline, to avoid blocking the MQTT receive loop.
type Handler func(context.Context, Event)

type Client struct {
	host        string
	port        int
	username    string
	password    string
	topicPrefix string
	clientID    string
	tls         bool
	logger      *slog.Logger

	client    paho.Client
	onEvent   Handler
	snapshots *SnapshotCache
}

type Options struct {
	Host        string
	Port        int
	Username    string
	Password    string
	TopicPrefix string
	ClientID    string
	TLS         bool           // tcp:// (false) vs ssl:// (true)
	Snapshots   *SnapshotCache // nil = don't subscribe to retained snapshots
	Logger      *slog.Logger
}

func New(opts Options) *Client {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Client{
		host:        opts.Host,
		port:        opts.Port,
		username:    opts.Username,
		password:    opts.Password,
		topicPrefix: opts.TopicPrefix,
		clientID:    opts.ClientID,
		tls:         opts.TLS,
		snapshots:   opts.Snapshots,
		logger:      opts.Logger.With("component", "mqtt"),
	}
}

// Connect dials the broker and subscribes to the events topic. Returns when
// the broker has CONNACK'd OR the dial timed out / was refused. The connection
// auto-reconnects with paho's built-in exponential backoff.
func (c *Client) Connect(ctx context.Context, onEvent Handler) error {
	if onEvent == nil {
		return fmt.Errorf("onEvent handler is required")
	}
	c.onEvent = onEvent

	scheme := "tcp"
	if c.tls {
		scheme = "ssl"
	}
	opts := paho.NewClientOptions().
		AddBroker(fmt.Sprintf("%s://%s:%d", scheme, c.host, c.port)).
		SetClientID(c.clientID).
		SetUsername(c.username).
		SetPassword(c.password).
		SetAutoReconnect(true).
		SetMaxReconnectInterval(60 * time.Second).
		SetCleanSession(false).
		SetOnConnectHandler(c.onConnected)

	c.client = paho.NewClient(opts)

	tok := c.client.Connect()
	select {
	case <-tok.Done():
		if err := tok.Error(); err != nil {
			return fmt.Errorf("mqtt connect: %w", err)
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (c *Client) Disconnect() {
	if c.client != nil && c.client.IsConnected() {
		c.client.Disconnect(500)
	}
}

// IsConnected reports whether the broker connection is currently up. False
// during the initial dial, after a network drop, and during reconnect
// attempts. Used by /readyz to gate the daemon's readiness signal.
func (c *Client) IsConnected() bool {
	return c.client != nil && c.client.IsConnected()
}

func (c *Client) onConnected(client paho.Client) {
	topic := fmt.Sprintf("%s/events", c.topicPrefix)
	c.logger.Info("connected; subscribing", "topic", topic, "tls", c.tls)
	if tok := client.Subscribe(topic, 0, c.handleMessage); tok.Wait() && tok.Error() != nil {
		c.logger.Error("subscribe failed", "err", tok.Error())
	}
	if c.snapshots != nil {
		if err := c.snapshots.Subscribe(client, c.topicPrefix); err != nil {
			c.logger.Error("snapshot subscribe failed", "err", err)
		} else {
			c.logger.Info("snapshot cache subscribed", "topic", c.topicPrefix+"/+/+/snapshot")
		}
	}
}

func (c *Client) handleMessage(_ paho.Client, msg paho.Message) {
	var raw frigateEventEnvelope
	if err := json.Unmarshal(msg.Payload(), &raw); err != nil {
		c.logger.Warn("decode event failed", "err", err, "topic", msg.Topic())
		return
	}
	ev, ok := raw.toEvent()
	if !ok {
		return
	}
	c.onEvent(context.Background(), ev)
}

// frigateEventEnvelope is the wire shape of the JSON Frigate publishes
// to `frigate/events`. The schema is documented at
// https://docs.frigate.video/integrations/mqtt — we only deserialise the
// fields the bridge forwards.
type frigateEventEnvelope struct {
	Type   string        `json:"type"` // "new" | "update" | "end"
	Before *frigateState `json:"before"`
	After  *frigateState `json:"after"`
}

type frigateState struct {
	ID       string  `json:"id"`
	Camera   string  `json:"camera"`
	Label    string  `json:"label"`
	TopScore float64 `json:"top_score"`
	// Frigate's MQTT payload carries current_zones (live) and
	// entered_zones (history). `zones` is the REST/legacy name; accept it
	// too so a replayed REST payload still decodes.
	Zones         []string         `json:"zones"`
	CurrentZones  []string         `json:"current_zones"`
	EnteredZones  []string         `json:"entered_zones"`
	SubLabel      flexibleSubLabel `json:"sub_label"`
	StartTimeUnix float64          `json:"start_time"`
	EndTimeUnix   *float64         `json:"end_time"`
	HasClip       bool             `json:"has_clip"`
	HasSnapshot   bool             `json:"has_snapshot"`
}

// flexibleSubLabel accepts the two shapes Frigate uses for sub_label:
// a plain string ("Kevin") or a [name, score] pair (["Kevin", 0.92]).
// null or any other shape decodes to "" instead of failing the whole
// event.
type flexibleSubLabel struct {
	Name string
}

func (f *flexibleSubLabel) UnmarshalJSON(b []byte) error {
	f.Name = ""
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		f.Name = s
		return nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(b, &arr); err == nil && len(arr) > 0 {
		if err := json.Unmarshal(arr[0], &s); err == nil {
			f.Name = s
		}
	}
	return nil
}

// isValidName mirrors the macOS Bridge guard: non-empty, at most 128
// bytes, letters/digits plus "-_.:". Frigate event ids, camera, label
// and zone names fit inside that set; anything else is treated as a
// malformed payload and dropped rather than forwarded.
func isValidName(v string) bool {
	if v == "" || len(v) > 128 {
		return false
	}
	for _, r := range v {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == '-', r == '_', r == '.', r == ':':
		default:
			return false
		}
	}
	return true
}

// mergeZones returns zones ∪ entered_zones ∪ current_zones, de-duplicated
// in first-seen order. entered_zones keeps the history (current_zones is
// empty once the object has left); current_zones adds any zone entered
// since the last entered_zones update.
func mergeZones(lists ...[]string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, l := range lists {
		for _, z := range l {
			if !seen[z] {
				seen[z] = true
				out = append(out, z)
			}
		}
	}
	return out
}

func unixFloat(sec float64) time.Time {
	return time.UnixMilli(int64(math.Round(sec * 1000))).UTC()
}

func (e frigateEventEnvelope) toEvent() (Event, bool) {
	state := e.After
	if state == nil {
		state = e.Before
	}
	if state == nil || state.ID == "" {
		return Event{}, false
	}
	zones := mergeZones(state.Zones, state.EnteredZones, state.CurrentZones)
	if !isValidName(state.ID) || !isValidName(state.Camera) || !isValidName(state.Label) || len(zones) > 64 {
		return Event{}, false
	}
	for _, z := range zones {
		if !isValidName(z) {
			return Event{}, false
		}
	}
	if math.IsNaN(state.TopScore) || state.TopScore < 0 || state.TopScore > 1 ||
		math.IsNaN(state.StartTimeUnix) || math.IsInf(state.StartTimeUnix, 0) {
		return Event{}, false
	}
	out := Event{
		ID:          state.ID,
		Camera:      state.Camera,
		Label:       state.Label,
		SubLabel:    state.SubLabel.Name,
		Zones:       zones,
		TopScore:    state.TopScore,
		StartTime:   unixFloat(state.StartTimeUnix),
		HasClip:     state.HasClip,
		HasSnapshot: state.HasSnapshot,
		Phase:       EventPhase(e.Type),
	}
	if state.EndTimeUnix != nil {
		t := unixFloat(*state.EndTimeUnix)
		out.EndTime = &t
	}
	return out, true
}
