package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

// maxBody is the largest request body accepted.
const maxBody = 1 << 20

var errTrailingData = errors.New("unexpected data after the JSON value")

// Request bodies use pointer fields so a missing field is nil, not zero.
type startRequest struct {
	ChargerID    *string    `json:"charger_id"`
	StartedAt    *time.Time `json:"started_at"`
	MeterStartWh *int64     `json:"meter_start_wh"`
}

func (q startRequest) complete() bool {
	return q.ChargerID != nil && q.StartedAt != nil && q.MeterStartWh != nil
}

type meterValueRequest struct {
	Timestamp *time.Time `json:"timestamp"`
	MeterWh   *int64     `json:"meter_wh"`
}

func (q meterValueRequest) complete() bool { return q.Timestamp != nil && q.MeterWh != nil }

type stopRequest struct {
	StoppedAt   *time.Time `json:"stopped_at"`
	MeterStopWh *int64     `json:"meter_stop_wh"`
}

func (q stopRequest) complete() bool { return q.StoppedAt != nil && q.MeterStopWh != nil }

// utcMicro is the form timestamps are compared and stored in: the same instant
// in UTC, at Postgres's microsecond precision.
func utcMicro(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

// Known limitation: encoding/json matches keys case-insensitively and keeps the
// last of duplicate keys, so {"TIMESTAMP":...} is accepted although the contract
// is strict. It causes no 500 and has no security impact.
//
// decodeBody reads exactly one JSON value from the body into dst. It rejects
// unknown fields, bodies over maxBody, an empty body and trailing data.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errTrailingData
		}
		return err
	}
	return nil
}
