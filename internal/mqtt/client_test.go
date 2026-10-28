package mqtt

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// Real Frigate event payloads observed in the wild (Frigate 0.13+). These
// are the canonical shapes the bridge must decode without losing data.
// New shapes seen in production should be added here as regression tests.

func TestDecode_NewEvent(t *testing.T) {
	raw := `{
		"type": "new",
		"before": null,
		"after": {
			"id": "1714155600.123456-abc",
			"camera": "front_door",
			"label": "person",
			"top_score": 0.87,
			"current_zones": ["entry"],
			"entered_zones": ["entry"],
			"start_time": 1714155600.123,
			"end_time": null,
			"has_clip": false,
			"has_snapshot": true
		}
	}`
	var env frigateEventEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ev, ok := env.toEvent()
	if !ok {
		t.Fatal("toEvent returned !ok for valid event")
	}
	if ev.ID != "1714155600.123456-abc" {
		t.Errorf("id: got %q", ev.ID)
	}
	if ev.Camera != "front_door" {
		t.Errorf("camera: got %q", ev.Camera)
	}
	if ev.Label != "person" {
		t.Errorf("label: got %q", ev.Label)
	}
	if ev.Phase != PhaseNew {
		t.Errorf("phase: got %q want %q", ev.Phase, PhaseNew)
	}
	if !ev.HasSnapshot {
		t.Error("hasSnapshot should be true")
	}
	if ev.HasClip {
		t.Error("hasClip should be false at new-event time")
	}
	if ev.EndTime != nil {
		t.Error("endTime should be nil for new events")
	}
	if len(ev.Zones) != 1 || ev.Zones[0] != "entry" {
		t.Errorf("zones: got %v", ev.Zones)
	}
	wantStart := time.UnixMilli(1714155600123).UTC()
	if !ev.StartTime.Equal(wantStart) {
		t.Errorf("startTime: got %v want %v", ev.StartTime, wantStart)
	}
}

func TestDecode_EndEvent(t *testing.T) {
	raw := `{
		"type": "end",
		"before": {
			"id": "1714155600.123456-abc",
			"camera": "front_door",
			"label": "person",
			"top_score": 0.87,
			"start_time": 1714155600.0,
			"end_time": null
		},
		"after": {
			"id": "1714155600.123456-abc",
			"camera": "front_door",
			"label": "person",
			"top_score": 0.93,
			"current_zones": [],
			"entered_zones": ["entry", "porch"],
			"start_time": 1714155600.0,
			"end_time": 1714155615.5,
			"has_clip": true,
			"has_snapshot": true
		}
	}`
	var env frigateEventEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ev, ok := env.toEvent()
	if !ok {
		t.Fatal("!ok")
	}
	if ev.Phase != PhaseEnd {
		t.Errorf("phase: got %q", ev.Phase)
	}
	if ev.EndTime == nil {
		t.Fatal("endTime should be non-nil for end events")
	}
	wantEnd := time.UnixMilli(1714155615500).UTC()
	if !ev.EndTime.Equal(wantEnd) {
		t.Errorf("endTime: got %v want %v", *ev.EndTime, wantEnd)
	}
	if ev.TopScore != 0.93 {
		t.Errorf("topScore: should reflect AFTER state, got %v", ev.TopScore)
	}
	if !ev.HasClip {
		t.Error("hasClip should be true at end-event time")
	}
	// Entered zones should be preferred over current_zones because Frigate's
	// current_zones is empty on event-end (target left the frame), but
	// entered_zones is the historical accumulation we want to surface.
	if len(ev.Zones) != 2 || ev.Zones[0] != "entry" || ev.Zones[1] != "porch" {
		t.Errorf("zones: got %v want [entry porch]", ev.Zones)
	}
}

func TestDecode_BeforeOnly(t *testing.T) {
	// Edge case: Frigate sometimes publishes an "end" event with
	// after == before semantically and only `before` populated. We should
	// fall back to before so we still emit the event.
	raw := `{
		"type": "end",
		"before": {
			"id": "fallback-id",
			"camera": "garage",
			"label": "car",
			"top_score": 0.5,
			"start_time": 1714155600.0
		},
		"after": null
	}`
	var env frigateEventEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ev, ok := env.toEvent()
	if !ok {
		t.Fatal("!ok")
	}
	if ev.ID != "fallback-id" {
		t.Errorf("id: got %q", ev.ID)
	}
}

func TestDecode_DropsEmpty(t *testing.T) {
	// Both before and after empty / missing — must drop, not panic.
	cases := []string{
		`{"type":"new","before":null,"after":null}`,
		`{"type":"new"}`,
		`{"type":"new","after":{"id":""}}`,
	}
	for i, raw := range cases {
		var env frigateEventEnvelope
		if err := json.Unmarshal([]byte(raw), &env); err != nil {
			t.Fatalf("case %d unmarshal: %v", i, err)
		}
		if _, ok := env.toEvent(); ok {
			t.Errorf("case %d: should have dropped empty event", i)
		}
	}
}

// Frigate 0.17: an object that entered "driveway" earlier and is now in
// "porch" — entered_zones holds the history, current_zones the live zone
// that may not yet be in entered_zones. Both must be forwarded, deduped,
// in first-seen order.
func TestDecode_ZonesMergeEnteredAndCurrent(t *testing.T) {
	raw := `{"type":"update","after":{"id":"1.2-a","camera":"front","label":"person",
		"top_score":0.8,"start_time":1,"entered_zones":["driveway","porch"],
		"current_zones":["porch","mailbox"]}}`
	var env frigateEventEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatal(err)
	}
	ev, ok := env.toEvent()
	if !ok {
		t.Fatal("!ok")
	}
	want := []string{"driveway", "porch", "mailbox"}
	if len(ev.Zones) != len(want) {
		t.Fatalf("zones: got %v want %v", ev.Zones, want)
	}
	for i := range want {
		if ev.Zones[i] != want[i] {
			t.Fatalf("zones: got %v want %v", ev.Zones, want)
		}
	}
}

// current_zones only (object still in its first zone, entered_zones not
// yet populated) must still produce the zone.
func TestDecode_ZonesCurrentOnly(t *testing.T) {
	raw := `{"type":"new","after":{"id":"1.2-a","camera":"front","label":"person",
		"top_score":0.8,"start_time":1,"entered_zones":[],"current_zones":["mailbox"]}}`
	var env frigateEventEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatal(err)
	}
	ev, ok := env.toEvent()
	if !ok || len(ev.Zones) != 1 || ev.Zones[0] != "mailbox" {
		t.Fatalf("got ok=%v zones=%v", ok, ev.Zones)
	}
}

// No zones at all → empty (non-nil) slice so the relay JSON is [] not null.
func TestDecode_NoZones(t *testing.T) {
	raw := `{"type":"new","after":{"id":"1.2-a","camera":"front","label":"car","top_score":0.8,"start_time":1}}`
	var env frigateEventEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatal(err)
	}
	ev, ok := env.toEvent()
	if !ok || ev.Zones == nil || len(ev.Zones) != 0 {
		t.Fatalf("got ok=%v zones=%#v", ok, ev.Zones)
	}
}

// sub_label arrives as a string, a [name, score] pair, null, or an
// unexpected shape. None of these may drop the event.
func TestDecode_SubLabelShapes(t *testing.T) {
	cases := []struct {
		name, subLabel, want string
	}{
		{"string", `"Kevin"`, "Kevin"},
		{"pair", `["Kevin", 0.92]`, "Kevin"},
		{"plate pair", `["AB-123-CD", 0.88]`, "AB-123-CD"},
		{"null", `null`, ""},
		{"empty array", `[]`, ""},
		{"number", `42`, ""},
		{"object", `{"name":"x"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"type":"new","after":{"id":"1.2-a","camera":"front","label":"person",
				"top_score":0.8,"start_time":1,"sub_label":` + tc.subLabel + `}}`
			var env frigateEventEnvelope
			if err := json.Unmarshal([]byte(raw), &env); err != nil {
				t.Fatalf("unmarshal must not fail: %v", err)
			}
			ev, ok := env.toEvent()
			if !ok {
				t.Fatal("event dropped")
			}
			if ev.SubLabel != tc.want {
				t.Errorf("subLabel: got %q want %q", ev.SubLabel, tc.want)
			}
		})
	}
}

// Malformed identity fields or scores are dropped, not forwarded.
func TestDecode_RejectsMalformed(t *testing.T) {
	cases := []string{
		`{"type":"new","after":{"id":"1.2-a","camera":"front door","label":"person","top_score":0.5,"start_time":1}}`,
		`{"type":"new","after":{"id":"1.2-a","camera":"front","label":"","top_score":0.5,"start_time":1}}`,
		`{"type":"new","after":{"id":"1.2/../a","camera":"front","label":"person","top_score":0.5,"start_time":1}}`,
		`{"type":"new","after":{"id":"1.2-a","camera":"front","label":"person","top_score":1.5,"start_time":1}}`,
		`{"type":"new","after":{"id":"1.2-a","camera":"front","label":"person","top_score":0.5,"start_time":1,"current_zones":["bad zone"]}}`,
	}
	for i, raw := range cases {
		var env frigateEventEnvelope
		if err := json.Unmarshal([]byte(raw), &env); err != nil {
			t.Fatalf("case %d unmarshal: %v", i, err)
		}
		if _, ok := env.toEvent(); ok {
			t.Errorf("case %d: malformed event should be dropped", i)
		}
	}
}

type fakeMsg struct {
	topic   string
	payload []byte
}

func (m fakeMsg) Duplicate() bool   { return false }
func (m fakeMsg) Qos() byte         { return 0 }
func (m fakeMsg) Retained() bool    { return false }
func (m fakeMsg) Topic() string     { return m.topic }
func (m fakeMsg) MessageID() uint16 { return 0 }
func (m fakeMsg) Payload() []byte   { return m.payload }
func (m fakeMsg) Ack()              {}

// End-to-end through the paho callback: raw Frigate 0.17 bytes in,
// decoded Event out; garbage is dropped without calling the handler.
func TestHandleMessage_DeliversDecodedEvent(t *testing.T) {
	var got []Event
	c := New(Options{TopicPrefix: "frigate"})
	c.onEvent = func(_ context.Context, ev Event) { got = append(got, ev) }
	c.handleMessage(nil, fakeMsg{"frigate/events", []byte(`{"type":"new","before":null,"after":{
		"id":"1730000000.5-xyz","camera":"front_door","label":"person","sub_label":["Kevin",0.91],
		"top_score":0.84,"start_time":1730000000.5,"entered_zones":["porch"],"current_zones":["porch","mailbox"],
		"has_clip":false,"has_snapshot":true}}`)})
	c.handleMessage(nil, fakeMsg{"frigate/events", []byte(`not json`)})
	if len(got) != 1 {
		t.Fatalf("got %d events", len(got))
	}
	ev := got[0]
	if ev.SubLabel != "Kevin" || len(ev.Zones) != 2 || ev.Zones[1] != "mailbox" || ev.Phase != PhaseNew {
		t.Fatalf("decoded: %+v", ev)
	}
}
