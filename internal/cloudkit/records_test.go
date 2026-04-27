package cloudkit

import (
	"encoding/json"
	"testing"
	"time"
)

func TestRecord_FrigateEventEncoding(t *testing.T) {
	when := time.Date(2026, 4, 27, 10, 30, 0, 0, time.UTC)
	ev := FrigateEvent{
		ID:        "abc-123",
		Camera:    "front_door",
		Label:     "person",
		Zones:     []string{"entry", "porch"},
		TopScore:  0.87,
		StartTime: when,
	}
	rec := ev.ToRecord()
	if rec.RecordType != "FrigateEvent" {
		t.Errorf("recordType: got %q", rec.RecordType)
	}
	if rec.RecordName != "abc-123" {
		t.Errorf("recordName: got %q", rec.RecordName)
	}

	// Round-trip through JSON to verify the wire shape matches what
	// CloudKit Web Services expects: every field wrapped in
	// {"value": ..., "type": "..."}.
	raw, err := json.Marshal(rec.toJSON())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	fields, ok := got["fields"].(map[string]any)
	if !ok {
		t.Fatalf("fields missing or wrong shape")
	}
	assertField(t, fields, "camera", "STRING", "front_door")
	assertField(t, fields, "label", "STRING", "person")
	// Zones should serialise as STRING_LIST.
	zones, ok := fields["zones"].(map[string]any)
	if !ok {
		t.Fatal("zones missing")
	}
	if zones["type"] != "STRING_LIST" {
		t.Errorf("zones type: got %v", zones["type"])
	}
	// topScore as DOUBLE.
	score, ok := fields["topScore"].(map[string]any)
	if !ok {
		t.Fatal("topScore missing")
	}
	if score["type"] != "DOUBLE" {
		t.Errorf("topScore type: got %v", score["type"])
	}
	// detectedAt as TIMESTAMP (Unix-millis).
	det, ok := fields["detectedAt"].(map[string]any)
	if !ok {
		t.Fatal("detectedAt missing")
	}
	if det["type"] != "TIMESTAMP" {
		t.Errorf("detectedAt type: got %v", det["type"])
	}
	wantMillis := float64(when.UnixMilli())
	if got, ok := det["value"].(float64); !ok || got != wantMillis {
		t.Errorf("detectedAt value: got %v want %v", det["value"], wantMillis)
	}
}

func TestEncodeField_Bool(t *testing.T) {
	out := encodeField(true)
	if out["type"] != "INT64" || out["value"] != 1 {
		t.Errorf("true: got %+v want INT64=1", out)
	}
	out = encodeField(false)
	if out["type"] != "INT64" || out["value"] != 0 {
		t.Errorf("false: got %+v want INT64=0", out)
	}
}

func TestEncodeField_UnknownTypeFallsBackToString(t *testing.T) {
	type weird struct{ X int }
	out := encodeField(weird{X: 7})
	if out["type"] != "STRING" {
		t.Errorf("unknown type fallback: got %v want STRING", out["type"])
	}
}

func assertField(t *testing.T, fields map[string]any, key, wantType string, wantValue any) {
	t.Helper()
	got, ok := fields[key].(map[string]any)
	if !ok {
		t.Errorf("%s missing or not an object: %+v", key, fields[key])
		return
	}
	if got["type"] != wantType {
		t.Errorf("%s type: got %v want %v", key, got["type"], wantType)
	}
	if got["value"] != wantValue {
		t.Errorf("%s value: got %v want %v", key, got["value"], wantValue)
	}
}
