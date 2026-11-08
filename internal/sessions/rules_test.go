package sessions

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jlimty/ev-bill/internal/billing"
)

var sgt = billing.SingaporeTariff().Location

// at returns a clock time on 2026-11-06 in Singapore.
func at(h, m, s int) time.Time {
	return time.Date(2026, 11, 6, h, m, s, 0, sgt)
}

// fixture: session started 10:00 with meter 100, one stored reading 10:10/300, now 10:20.
func fixture() (Session, []billing.Reading, time.Time) {
	s := Session{ID: "s1", ChargerID: "CP-01", StartedAt: at(10, 0, 0), MeterStartWh: 100}
	readings := []billing.Reading{{At: at(10, 10, 0), MeterWh: 300}}
	return s, readings, at(10, 20, 0)
}

func stopped(s Session, when time.Time, meter int64) Session {
	s.StoppedAt = &when
	s.MeterStopWh = &meter
	return s
}

// wantRule asserts err is a *RuleError with the given kind and code.
func wantRule(t *testing.T, name string, err error, kind Kind, code string) {
	t.Helper()
	var re *RuleError
	if !errors.As(err, &re) {
		t.Errorf("%s: err = %v, want RuleError %s", name, err, code)
		return
	}
	if re.Kind != kind || re.Code != code {
		t.Errorf("%s: got kind %d code %q, want kind %d code %q", name, re.Kind, re.Code, kind, code)
	}
}

func TestCheckStart_SES1(t *testing.T) {
	now := at(10, 0, 0)
	cases := []struct {
		name    string
		charger string
		start   time.Time
		meter   int64
		kind    Kind
		code    string // empty means ok
	}{
		{"valid", "CP-01", now, 0, 0, ""},
		{"historic start allowed", "CP-01", now.Add(-72 * time.Hour), 0, 0, ""},
		{"32 chars ok", strings.Repeat("A", 32), now, 0, 0, ""},
		{"lower case", "cp-01", now, 0, KindInvalid, "invalid_charger_id"},
		{"33 chars", strings.Repeat("A", 33), now, 0, KindInvalid, "invalid_charger_id"},
		{"empty", "", now, 0, KindInvalid, "invalid_charger_id"},
		{"negative meter", "CP-01", now, -1, KindInvalid, "invalid_reading"},
		{"R3 meter at cap", "CP-01", now, MaxMeterWh, 0, ""},
		{"R3 meter over cap", "CP-01", now, 1_000_000_000_000_001, KindInvalid, "invalid_reading"},
		{"skew exactly 5m ok", "CP-01", now.Add(5 * time.Minute), 0, 0, ""},
		{"skew 6m", "CP-01", now.Add(6 * time.Minute), 0, KindInvalid, "invalid_reading"},
		{"earliest start 2000-01-01Z ok", "CP-01", time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), 0, 0, ""},
		{"one ns before earliest start", "CP-01", time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).Add(-time.Nanosecond), 0, KindInvalid, "invalid_reading"},
		{"year 0 with +08:00 offset", "CP-01", time.Date(0, 1, 1, 0, 0, 0, 0, time.FixedZone("", 8*3600)), 0, KindInvalid, "invalid_reading"},
	}
	for _, c := range cases {
		err := CheckStart(c.charger, c.start, c.meter, now)
		if c.code == "" {
			if err != nil {
				t.Errorf("%s: err = %v, want nil", c.name, err)
			}
			continue
		}
		wantRule(t, c.name, err, c.kind, c.code)
	}
}

// API-2: the error shape the API maps to HTTP codes (code and message, kind
// recoverable through wrapping).
func TestRuleError_Error(t *testing.T) {
	e := &RuleError{Kind: KindConflict, Code: "stop_conflict", Message: "a different stop exists"}
	if got, want := e.Error(), "stop_conflict: a different stop exists"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	wrapped := fmt.Errorf("get: %w", ErrNotFound)
	var re *RuleError
	if !errors.Is(wrapped, ErrNotFound) || !errors.As(wrapped, &re) || re.Kind != KindNotFound || re.Code != "not_found" {
		t.Errorf("wrapped ErrNotFound = %v (as %+v)", wrapped, re)
	}
}

func TestCheckReading_SES2_SES5(t *testing.T) {
	s, readings, now := fixture()
	cases := []struct {
		name      string
		r         billing.Reading
		now       time.Time
		duplicate bool
		kind      Kind
		code      string // empty means no error
	}{
		{"new later reading", billing.Reading{At: at(10, 15, 0), MeterWh: 350}, now, false, 0, ""},
		{"before start", billing.Reading{At: at(9, 59, 0), MeterWh: 100}, now, false, KindInvalid, "invalid_reading"},
		{"skew 6m", billing.Reading{At: now.Add(6 * time.Minute), MeterWh: 400}, now, false, KindInvalid, "invalid_reading"},
		{"skew exactly 5m", billing.Reading{At: now.Add(5 * time.Minute), MeterWh: 400}, now, false, 0, ""},
		{"identical duplicate", billing.Reading{At: at(10, 10, 0), MeterWh: 300}, now, true, 0, ""},
		{"same time different meter", billing.Reading{At: at(10, 10, 0), MeterWh: 301}, now, false, KindConflict, "conflicting_reading"},
		{"duplicate of start point", billing.Reading{At: at(10, 0, 0), MeterWh: 100}, now, true, 0, ""},
		{"R5 start instant different meter", billing.Reading{At: at(10, 0, 0), MeterWh: 101}, now, false, KindConflict, "conflicting_reading"},
		{"out of order consistent", billing.Reading{At: at(10, 5, 0), MeterWh: 200}, now, false, 0, ""},
		{"out of order too high", billing.Reading{At: at(10, 5, 0), MeterWh: 400}, now, false, KindInvalid, "invalid_reading"},
		{"R5 below start meter", billing.Reading{At: at(10, 5, 0), MeterWh: 50}, now, false, KindInvalid, "invalid_reading"},
		{"later but lower than last", billing.Reading{At: at(10, 15, 0), MeterWh: 250}, now, false, KindInvalid, "invalid_reading"},
		{"negative meter", billing.Reading{At: at(10, 15, 0), MeterWh: -1}, now, false, KindInvalid, "invalid_reading"},
	}
	for _, c := range cases {
		dup, err := CheckReading(s, readings, c.r, c.now)
		if dup != c.duplicate {
			t.Errorf("%s: duplicate = %v, want %v", c.name, dup, c.duplicate)
		}
		if c.code == "" {
			if err != nil {
				t.Errorf("%s: err = %v, want nil", c.name, err)
			}
			continue
		}
		wantRule(t, c.name, err, c.kind, c.code)
	}
}

// SES-5: more than 48 h after start is rejected; exactly 48 h is allowed.
func TestCheckReading_SES5_MaxDuration(t *testing.T) {
	s, readings, _ := fixture()
	now := s.StartedAt.Add(49 * time.Hour)
	_, err := CheckReading(s, readings, billing.Reading{At: s.StartedAt.Add(48*time.Hour + time.Second), MeterWh: 400}, now)
	wantRule(t, "48h+1s", err, KindInvalid, "session_too_long")
	dup, err := CheckReading(s, readings, billing.Reading{At: s.StartedAt.Add(48 * time.Hour), MeterWh: 400}, now)
	if err != nil || dup {
		t.Errorf("exactly 48h: dup=%v err=%v, want false, nil", dup, err)
	}
}

// SES-3 / R5: readings after stop are a conflict, even an identical retry.
func TestCheckReading_SES3_R5_StoppedSession(t *testing.T) {
	s, readings, now := fixture()
	s = stopped(s, at(10, 30, 0), 500)
	_, err := CheckReading(s, readings, billing.Reading{At: at(10, 15, 0), MeterWh: 350}, now)
	wantRule(t, "new reading", err, KindConflict, "session_stopped")
	dup, err := CheckReading(s, readings, billing.Reading{At: at(10, 10, 0), MeterWh: 300}, now)
	wantRule(t, "identical retry", err, KindConflict, "session_stopped")
	if dup {
		t.Errorf("identical retry on stopped session reported duplicate")
	}
}

// R3: meter cap boundary for readings.
func TestCheckReading_R3_MeterCap(t *testing.T) {
	s := Session{ID: "s1", ChargerID: "CP-01", StartedAt: at(10, 0, 0), MeterStartWh: 0}
	now := at(10, 20, 0)
	if _, err := CheckReading(s, nil, billing.Reading{At: at(10, 5, 0), MeterWh: 1_000_000_000_000_000}, now); err != nil {
		t.Errorf("10^15: err = %v, want nil", err)
	}
	_, err := CheckReading(s, nil, billing.Reading{At: at(10, 5, 0), MeterWh: 1_000_000_000_000_001}, now)
	wantRule(t, "10^15+1", err, KindInvalid, "invalid_reading")
}

func TestCheckStop_SES3(t *testing.T) {
	s, readings, _ := fixture()
	now := at(10, 40, 0)
	cases := []struct {
		name string
		stop billing.Reading
		now  time.Time
		kind Kind
		code string
	}{
		{"valid stop", billing.Reading{At: at(10, 30, 0), MeterWh: 500}, now, 0, ""},
		{"before last reading", billing.Reading{At: at(10, 5, 0), MeterWh: 350}, now, KindInvalid, "invalid_reading"},
		{"below last meter", billing.Reading{At: at(10, 30, 0), MeterWh: 250}, now, KindInvalid, "invalid_reading"},
		{"repeats last reading", billing.Reading{At: at(10, 10, 0), MeterWh: 300}, now, 0, ""},
		{"R5 same instant different meter", billing.Reading{At: at(10, 10, 0), MeterWh: 350}, now, KindConflict, "conflicting_reading"},
		{"before start", billing.Reading{At: at(9, 59, 0), MeterWh: 100}, now, KindInvalid, "invalid_reading"},
		{"skew 6m", billing.Reading{At: now.Add(6 * time.Minute), MeterWh: 500}, now, KindInvalid, "invalid_reading"},
		{"negative meter", billing.Reading{At: at(10, 30, 0), MeterWh: -1}, now, KindInvalid, "invalid_reading"},
	}
	for _, c := range cases {
		replay, err := CheckStop(s, readings, c.stop, c.now)
		if replay {
			t.Errorf("%s: replay = true on an active session", c.name)
		}
		if c.code == "" {
			if err != nil {
				t.Errorf("%s: err = %v, want nil", c.name, err)
			}
			continue
		}
		wantRule(t, c.name, err, c.kind, c.code)
	}
}

func TestCheckStop_SES3_Replay(t *testing.T) {
	s, readings, now := fixture()
	s = stopped(s, at(10, 30, 0), 500)
	replay, err := CheckStop(s, readings, billing.Reading{At: at(10, 30, 0), MeterWh: 500}, now)
	if err != nil || !replay {
		t.Errorf("identical stop: replay=%v err=%v, want true, nil", replay, err)
	}
	for name, stop := range map[string]billing.Reading{
		"different meter": {At: at(10, 30, 0), MeterWh: 501},
		"different time":  {At: at(10, 31, 0), MeterWh: 500},
	} {
		replay, err := CheckStop(s, readings, stop, now)
		if replay {
			t.Errorf("%s: replay = true", name)
		}
		wantRule(t, name, err, KindConflict, "stop_conflict")
	}
}

// R4: a stop is held to the 48 h limit; exactly 48 h is allowed.
func TestCheckStop_R4_MaxDuration(t *testing.T) {
	s, readings, _ := fixture()
	now := s.StartedAt.Add(49 * time.Hour)
	_, err := CheckStop(s, readings, billing.Reading{At: s.StartedAt.Add(48*time.Hour + time.Second), MeterWh: 500}, now)
	wantRule(t, "48h+1s", err, KindInvalid, "session_too_long")
	replay, err := CheckStop(s, readings, billing.Reading{At: s.StartedAt.Add(48 * time.Hour), MeterWh: 500}, now)
	if err != nil || replay {
		t.Errorf("exactly 48h: replay=%v err=%v, want false, nil", replay, err)
	}
}

// R3: meter cap boundary for stops.
func TestCheckStop_R3_MeterCap(t *testing.T) {
	s := Session{ID: "s1", ChargerID: "CP-01", StartedAt: at(10, 0, 0), MeterStartWh: 0}
	now := at(10, 20, 0)
	if _, err := CheckStop(s, nil, billing.Reading{At: at(10, 5, 0), MeterWh: 1_000_000_000_000_000}, now); err != nil {
		t.Errorf("10^15: err = %v, want nil", err)
	}
	_, err := CheckStop(s, nil, billing.Reading{At: at(10, 5, 0), MeterWh: 1_000_000_000_000_001}, now)
	wantRule(t, "10^15+1", err, KindInvalid, "invalid_reading")
}

// R1: Timeline orders points and drops a point identical to the one before it.
func TestTimeline_R1(t *testing.T) {
	s := Session{StartedAt: at(10, 0, 0), MeterStartWh: 100}
	unsorted := []billing.Reading{{At: at(10, 10, 0), MeterWh: 300}, {At: at(10, 5, 0), MeterWh: 200}}
	got := Timeline(stopped(s, at(10, 10, 0), 300), unsorted)
	want := []billing.Reading{
		{At: at(10, 0, 0), MeterWh: 100},
		{At: at(10, 5, 0), MeterWh: 200},
		{At: at(10, 10, 0), MeterWh: 300},
	}
	if len(got) != len(want) {
		t.Fatalf("Timeline = %v, want %v", got, want)
	}
	for i := range want {
		if !got[i].At.Equal(want[i].At) || got[i].MeterWh != want[i].MeterWh {
			t.Errorf("point %d = %v, want %v", i, got[i], want[i])
		}
	}
	// The caller's slice must stay in its original order.
	if !unsorted[0].At.Equal(at(10, 10, 0)) {
		t.Errorf("Timeline reordered the caller's readings: %v", unsorted)
	}
	// Active session: no stop point.
	if n := len(Timeline(s, unsorted)); n != 3 {
		t.Errorf("active Timeline length = %d, want 3", n)
	}
}

// SES-2 / SES-3 / SES-5 / R5: rule ordering. Each case has inputs where only
// the first applicable check gives the expected code, so moving a check fails.
func TestRuleOrdering(t *testing.T) {
	s, readings, now := fixture()
	done := stopped(s, at(10, 30, 0), 500)
	big := MaxMeterWh + 1
	cases := []struct {
		name string
		run  func() error
		kind Kind
		code string
	}{
		{"R5 stopped beats meter range (reading)", func() error {
			_, err := CheckReading(done, readings, billing.Reading{At: at(10, 15, 0), MeterWh: -1}, now)
			return err
		}, KindConflict, "session_stopped"},
		{"R5 stopped beats future skew (reading)", func() error {
			_, err := CheckReading(done, readings, billing.Reading{At: now.Add(6 * time.Minute), MeterWh: 400}, now)
			return err
		}, KindConflict, "session_stopped"},
		{"SES-3 stopped beats meter range (stop)", func() error {
			_, err := CheckStop(done, readings, billing.Reading{At: at(10, 30, 0), MeterWh: big}, now)
			return err
		}, KindConflict, "stop_conflict"},
		{"R5 same instant beats meter below last (stop 10:10/250)", func() error {
			_, err := CheckStop(s, readings, billing.Reading{At: at(10, 10, 0), MeterWh: 250}, now)
			return err
		}, KindConflict, "conflicting_reading"},
		{"R5 same instant beats neighbours (reading 10:10/50)", func() error {
			_, err := CheckReading(s, readings, billing.Reading{At: at(10, 10, 0), MeterWh: 50}, now)
			return err
		}, KindConflict, "conflicting_reading"},
		{"R3 meter range beats too long (reading)", func() error {
			_, err := CheckReading(s, readings, billing.Reading{At: s.StartedAt.Add(48*time.Hour + time.Second), MeterWh: big}, s.StartedAt.Add(49*time.Hour))
			return err
		}, KindInvalid, "invalid_reading"},
		{"SES-2 future skew beats too long (reading)", func() error {
			_, err := CheckReading(s, readings, billing.Reading{At: s.StartedAt.Add(48*time.Hour + time.Second), MeterWh: 400}, s.StartedAt.Add(10*time.Minute))
			return err
		}, KindInvalid, "invalid_reading"},
	}
	for _, c := range cases {
		wantRule(t, c.name, c.run(), c.kind, c.code)
	}
}

// SES-3: the last point includes the start point, so a stop below the start
// meter is rejected even with no readings.
func TestCheckStop_SES3_BelowStartMeter(t *testing.T) {
	s, _, now := fixture()
	_, err := CheckStop(s, nil, billing.Reading{At: at(10, 5, 0), MeterWh: 50}, now)
	wantRule(t, "stop below start meter", err, KindInvalid, "invalid_reading")
}

// SES-3: a session with only one stop field set is still active.
func TestHalfSetStop_SES3(t *testing.T) {
	s, readings, now := fixture()
	when := at(10, 30, 0)
	s.StoppedAt = &when
	if _, err := CheckReading(s, readings, billing.Reading{At: at(10, 15, 0), MeterWh: 350}, now); err != nil {
		t.Errorf("half-set stop treated as stopped: %v", err)
	}
	v, err := BuildView(s, readings, billing.SingaporeTariff(), now)
	if err != nil || v.Status == StatusCompleted {
		t.Errorf("half-set stop: status=%q err=%v, want not completed", v.Status, err)
	}
}

// manyReadings returns n valid readings, one per second from 10:00:01, meter 1 Wh each step.
func manyReadings(n int) []billing.Reading {
	rs := make([]billing.Reading, n)
	for i := range rs {
		rs[i] = billing.Reading{At: at(10, 0, 0).Add(time.Duration(i+1) * time.Second), MeterWh: int64(i + 1)}
	}
	return rs
}

// SES-2: a session holds at most 10000 readings; an identical retry at the cap
// is still a duplicate.
func TestCheckReading_SES2_ReadingCap(t *testing.T) {
	s := Session{ID: "s1", ChargerID: "CP-01", StartedAt: at(10, 0, 0), MeterStartWh: 0}
	now := at(13, 0, 0)
	next := billing.Reading{At: at(10, 0, 0).Add(10_001 * time.Second), MeterWh: 20_000}

	if _, err := CheckReading(s, manyReadings(9_999), next, now); err != nil {
		t.Errorf("9999 readings stored: err = %v, want accepted", err)
	}
	full := manyReadings(10_000)
	_, err := CheckReading(s, full, next, now)
	wantRule(t, "10000 readings stored", err, KindInvalid, "invalid_reading")
	if err == nil || !strings.Contains(err.Error(), "10000-reading limit") {
		t.Errorf("err = %v, want message containing %q", err, "10000-reading limit")
	}
	dup, err := CheckReading(s, full, full[42], now)
	if err != nil || !dup {
		t.Errorf("identical retry at cap: dup = %v, err = %v, want true, nil", dup, err)
	}
	_, err = CheckReading(s, full, billing.Reading{At: full[42].At, MeterWh: 999}, now)
	wantRule(t, "conflict at cap", err, KindConflict, "conflicting_reading")
}

// MIR-7: the API and the mirror never write to each other's sessions.
func TestCheckWriter_MIR7(t *testing.T) {
	cases := []struct {
		name    string
		session Source
		as      Source
		code    string // empty means allowed
	}{
		{"api on api", SourceAPI, SourceAPI, ""},
		{"mirror on mirror", SourceMirror, SourceMirror, ""},
		{"api on mirror", SourceMirror, SourceAPI, "mirror_session"},
		{"mirror on api", SourceAPI, SourceMirror, "api_session"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckWriter(Session{Source: c.session}, c.as)
			if c.code == "" {
				if err != nil {
					t.Errorf("MIR-7: err = %v, want nil", err)
				}
				return
			}
			wantRule(t, c.name, err, KindConflict, c.code)
		})
	}
}
