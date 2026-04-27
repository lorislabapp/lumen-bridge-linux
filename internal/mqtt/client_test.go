package mqtt

import (
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
	wantStart := time.Unix(1714155600, 0).UTC()
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
	wantEnd := time.Unix(1714155615, 0).UTC()
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
