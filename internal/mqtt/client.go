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
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

// Event mirrors the fields produced by Frigate's events topic that we
// actually persist to CloudKit. Frigate emits more (e.g. bounding boxes,
// false-positive flags) — we ignore everything that doesn't end up on the
// CKRecord schema.
type Event struct {
	ID        string    // Frigate event ID — also the CKRecord.ID.recordName
	Camera    string
	Label     string
	Zones     []string
	TopScore  float64
	StartTime time.Time
	EndTime   *time.Time // nil while the event is still active
	HasClip   bool
	HasSnapshot bool
	Phase     EventPhase // new | update | end
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
// fields that map to our CKRecord schema.
type frigateEventEnvelope struct {
	Type   string         `json:"type"` // "new" | "update" | "end"
	Before *frigateState  `json:"before"`
	After  *frigateState  `json:"after"`
}

type frigateState struct {
	ID          string   `json:"id"`
	Camera      string   `json:"camera"`
	Label       string   `json:"label"`
	TopScore    float64  `json:"top_score"`
	CurrentZones []string `json:"current_zones"`
	EnteredZones []string `json:"entered_zones"`
	StartTimeUnix float64 `json:"start_time"`
	EndTimeUnix   *float64 `json:"end_time"`
	HasClip       bool    `json:"has_clip"`
	HasSnapshot   bool    `json:"has_snapshot"`
}

func (e frigateEventEnvelope) toEvent() (Event, bool) {
	state := e.After
	if state == nil {
		state = e.Before
	}
	if state == nil || state.ID == "" {
		return Event{}, false
	}
	zones := state.EnteredZones
	if len(zones) == 0 {
		zones = state.CurrentZones
	}
	out := Event{
		ID:        state.ID,
		Camera:    state.Camera,
		Label:     state.Label,
		Zones:     zones,
		TopScore:  state.TopScore,
		StartTime: time.Unix(int64(state.StartTimeUnix), 0).UTC(),
		HasClip:   state.HasClip,
		HasSnapshot: state.HasSnapshot,
		Phase:     EventPhase(e.Type),
	}
	if state.EndTimeUnix != nil {
		t := time.Unix(int64(*state.EndTimeUnix), 0).UTC()
		out.EndTime = &t
	}
	return out, true
}
