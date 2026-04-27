package cloudkit

import (
	"time"
)

// Record is a typed CKRecord-shaped object. We only model what Linux
// Bridge writes — extending this for other record types means adding new
// fields to Fields plus updating toJSON().
type Record struct {
	RecordType string         // e.g. "FrigateEvent"
	RecordName string         // CKRecord.ID.recordName — must be unique within the type
	Fields     map[string]any // typed values — strings, numbers, dates, [string], *AssetReceipt, etc.
}

// toJSON renders a Record into the wire shape CloudKit Web Services expects.
// Every field is wrapped in a `{"value": ..., "type": ...}` envelope
// because CloudKit needs to know the type to honour query/index semantics.
func (r *Record) toJSON() map[string]any {
	fields := make(map[string]any, len(r.Fields))
	for k, v := range r.Fields {
		fields[k] = encodeField(v)
	}
	return map[string]any{
		"recordType": r.RecordType,
		"recordName": r.RecordName,
		"fields":     fields,
	}
}

func encodeField(v any) map[string]any {
	switch x := v.(type) {
	case string:
		return map[string]any{"value": x, "type": "STRING"}
	case []string:
		return map[string]any{"value": x, "type": "STRING_LIST"}
	case int:
		return map[string]any{"value": x, "type": "INT64"}
	case int64:
		return map[string]any{"value": x, "type": "INT64"}
	case float64:
		return map[string]any{"value": x, "type": "DOUBLE"}
	case bool:
		return map[string]any{"value": boolToInt(x), "type": "INT64"}
	case time.Time:
		// CloudKit stores dates as Unix-millis since 1970-01-01.
		return map[string]any{"value": x.UnixMilli(), "type": "TIMESTAMP"}
	case *AssetReceipt:
		// Asset fields are populated AFTER UploadAsset returns. The wire
		// representation uses the type "ASSETID" with the receipt bundle
		// inline as the value.
		if x == nil {
			return nil
		}
		return map[string]any{
			"type": "ASSETID",
			"value": map[string]any{
				"fileChecksum":      x.FileChecksum,
				"size":              x.Size,
				"wrappingKey":       x.WrappingKey,
				"referenceChecksum": x.ReferenceChecksum,
				"receipt":           x.Receipt,
			},
		}
	default:
		// Unknown types fall back to string-ised representation; safer than
		// silent drop because the missing field would only show up at iOS
		// fetch time as a `nil` record value.
		return map[string]any{"value": v, "type": "STRING"}
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// FrigateEvent maps Frigate's MQTT event payload to the CloudKit record
// schema the macOS Bridge already writes. iOS clients subscribe by
// recordType, so the field names must match exactly.
type FrigateEvent struct {
	ID        string
	Camera    string
	Label     string
	Zones     []string
	TopScore  float64
	StartTime time.Time
	// Snapshot / Clip are optional; populate after UploadAsset succeeds.
	Snapshot *AssetReceipt
	Clip     *AssetReceipt
}

func (e FrigateEvent) ToRecord() *Record {
	fields := map[string]any{
		"camera":     e.Camera,
		"label":      e.Label,
		"zones":      e.Zones,
		"topScore":   e.TopScore,
		"detectedAt": e.StartTime,
	}
	if e.Snapshot != nil {
		fields["snapshot"] = e.Snapshot
	}
	if e.Clip != nil {
		fields["clip"] = e.Clip
	}
	return &Record{
		RecordType: "FrigateEvent",
		RecordName: e.ID,
		Fields:     fields,
	}
}
