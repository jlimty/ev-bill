package web

import (
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jlimty/ev-bill/internal/store"
)

// testCursorID is a well-formed session ID for cursor tests.
const testCursorID = "0b9a1f6e-6a39-4d3c-8f0e-1c2d3e4f5a6b"

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	v, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return v
}

// NAV-3: empty or absent parameters mean their defaults, a repeated one uses
// its first value, and any other value is a bad query. NAV-2: a site that is
// not six digits is a malformed postal code. NAV-4: exactly one of before or
// after comes with a cursor.
func TestParseListQuery_NAV3(t *testing.T) {
	started := encodeCursor(store.SortStarted, true, store.ListCursor{StartedAt: time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC), ID: testCursorID})
	for _, tc := range []struct {
		raw     string
		want    listQuery
		wantErr error
	}{
		{"", listQuery{Sort: store.SortStarted, Desc: true}, nil},
		{"sort=&dir=", listQuery{Sort: store.SortStarted, Desc: true}, nil},
		{"sort=started&dir=desc", listQuery{Sort: store.SortStarted, Desc: true}, nil},
		{"sort=energy&sort=started", listQuery{Sort: store.SortEnergy, Desc: true}, nil},
		{"dir=asc", listQuery{Sort: store.SortStarted}, nil},
		{"site=038983", listQuery{Site: "038983", Sort: store.SortStarted, Desc: true}, nil},
		{"site=", listQuery{Sort: store.SortStarted, Desc: true}, nil},
		{"view=list&other=1", listQuery{Sort: store.SortStarted, Desc: true}, nil},
		{"sort=cost", listQuery{}, errBadQuery},
		{"sort=Energy", listQuery{}, errBadQuery},
		{"dir=up", listQuery{}, errBadQuery},
		{"site=12345", listQuery{}, errBadPostal},
		{"site=1234567", listQuery{}, errBadPostal},
		{"site=abcdef", listQuery{}, errBadPostal},
		{"site=%EF%BC%91%EF%BC%92%EF%BC%93%EF%BC%94%EF%BC%95%EF%BC%96", listQuery{}, errBadPostal}, // full-width digits
		{"cursor=" + started, listQuery{}, errBadQuery},
		{"cursor=" + started + "&before&after", listQuery{}, errBadQuery},
		{"before", listQuery{}, errBadQuery},
		{"after", listQuery{}, errBadQuery},
		{"cursor=&after", listQuery{}, errBadQuery},
		{"cursor=" + started + "&after=1", listQuery{}, errBadQuery},
		{"cursor=" + started + "&after", listQuery{Sort: store.SortStarted, Desc: true,
			Cursor: &store.ListCursor{StartedAt: time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC), ID: testCursorID}}, nil},
		{"cursor=" + started + "&before", listQuery{Sort: store.SortStarted, Desc: true, Before: true,
			Cursor: &store.ListCursor{StartedAt: time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC), ID: testCursorID}}, nil},
		// The cursor was made for started/desc, so it does not fit energy.
		{"sort=energy&cursor=" + started + "&after", listQuery{}, errBadQuery},
	} {
		got, err := parseListQuery(mustQuery(t, tc.raw))
		if tc.wantErr != nil {
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("NAV-3: parseListQuery(%q) error = %v, want %v", tc.raw, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("NAV-3: parseListQuery(%q) = %v", tc.raw, err)
			continue
		}
		if !sameQuery(got, tc.want) {
			t.Errorf("NAV-3: parseListQuery(%q) = %+v, want %+v", tc.raw, got, tc.want)
		}
	}
}

func sameQuery(a, b listQuery) bool {
	if a.Site != b.Site || a.Sort != b.Sort || a.Desc != b.Desc || a.Before != b.Before || (a.Cursor == nil) != (b.Cursor == nil) {
		return false
	}
	return a.Cursor == nil || (a.Cursor.StartedAt.Equal(b.Cursor.StartedAt) && a.Cursor.EnergyWh == b.Cursor.EnergyWh && a.Cursor.ID == b.Cursor.ID)
}

// NAV-4: both sorts' cursors survive encode then decode; a time keeps its
// microseconds and comes back in UTC.
func TestCursor_NAV4_RoundTrip(t *testing.T) {
	at := time.Date(2026, 11, 7, 0, 20, 1, 123456789, billingLoc(t))
	enc := encodeCursor(store.SortStarted, true, store.ListCursor{StartedAt: at, ID: testCursorID})
	got, err := decodeCursor(enc, store.SortStarted, true)
	if err != nil {
		t.Fatalf("NAV-4: decode started cursor: %v", err)
	}
	if want := at.Truncate(time.Microsecond); !got.StartedAt.Equal(want) || got.StartedAt.Location() != time.UTC || got.ID != testCursorID {
		t.Errorf("NAV-4: started cursor = %+v, want %v in UTC and %s", got, want, testCursorID)
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil || string(raw) != "started|desc|2026-11-06T16:20:01.123456Z|"+testCursorID {
		t.Errorf("NAV-4: started cursor payload = %q (%v)", raw, err)
	}

	enc = encodeCursor(store.SortEnergy, false, store.ListCursor{EnergyWh: 18000, ID: testCursorID})
	got, err = decodeCursor(enc, store.SortEnergy, false)
	if err != nil {
		t.Fatalf("NAV-4: decode energy cursor: %v", err)
	}
	if got.EnergyWh != 18000 || got.ID != testCursorID {
		t.Errorf("NAV-4: energy cursor = %+v", got)
	}
	if raw, err := base64.RawURLEncoding.DecodeString(enc); err != nil || string(raw) != "energy|asc|18000|"+testCursorID {
		t.Errorf("NAV-4: energy cursor payload = %q (%v)", raw, err)
	}
	for _, enc := range []string{
		encodeCursor(store.SortStarted, true, store.ListCursor{StartedAt: time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC), ID: testCursorID}),
		encodeCursor(store.SortEnergy, true, store.ListCursor{EnergyWh: 1<<63 - 1, ID: testCursorID}),
	} {
		if len(enc) > maxCursorLen {
			t.Errorf("NAV-4: a valid cursor is %d characters, over the %d limit", len(enc), maxCursorLen)
		}
	}
}

func billingLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Singapore")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// NAV-4: every malformed cursor is a bad query: over 128 characters, bad
// base64, the wrong field count, a sort or dir that differs from the query's,
// a bad time or Wh, or an id that is not a UUID.
func TestCursor_NAV4_Malformed(t *testing.T) {
	b64 := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	const at = "2026-11-06T16:20:01.123456Z"
	for _, tc := range []struct {
		name, cursor string
		sort         store.SessionSort
		desc         bool
	}{
		{"over 128 characters", strings.Repeat("A", maxCursorLen+1), store.SortStarted, true},
		{"bad base64", "!!!!", store.SortStarted, true},
		// 77 bytes, so padded base64 ends in "="; the payload itself is valid.
		{"padded base64", base64.URLEncoding.EncodeToString([]byte("started|desc|" + at + "|" + testCursorID)), store.SortStarted, true},
		{"three fields", b64("started|desc|" + at), store.SortStarted, true},
		{"five fields", b64("started|desc|" + at + "|" + testCursorID + "|x"), store.SortStarted, true},
		{"empty", "", store.SortStarted, true},
		{"wrong sort", b64("energy|desc|5|" + testCursorID), store.SortStarted, true},
		{"wrong dir", b64("started|asc|" + at + "|" + testCursorID), store.SortStarted, true},
		{"unknown sort", b64("cost|desc|5|" + testCursorID), store.SortStarted, true},
		{"bad time", b64("started|desc|yesterday|" + testCursorID), store.SortStarted, true},
		{"time with offset", b64("started|desc|2026-11-07T00:20:01.123456+08:00|" + testCursorID), store.SortStarted, true},
		{"time without microseconds", b64("started|desc|2026-11-06T16:20:01Z|" + testCursorID), store.SortStarted, true},
		{"bad Wh", b64("energy|desc|lots|" + testCursorID), store.SortEnergy, true},
		{"negative Wh", b64("energy|desc|-1|" + testCursorID), store.SortEnergy, true},
		{"Wh with a sign", b64("energy|desc|+5|" + testCursorID), store.SortEnergy, true},
		{"Wh with leading zero", b64("energy|desc|05|" + testCursorID), store.SortEnergy, true},
		{"Wh overflow", b64("energy|desc|9223372036854775808|" + testCursorID), store.SortEnergy, true},
		{"id not a UUID", b64("energy|desc|5|not-a-uuid"), store.SortEnergy, true},
		{"id with SQL", b64("energy|desc|5|' OR 1=1 --"), store.SortEnergy, true},
	} {
		if _, err := decodeCursor(tc.cursor, tc.sort, tc.desc); !errors.Is(err, errBadQuery) {
			t.Errorf("NAV-4: %s: decodeCursor error = %v, want errBadQuery", tc.name, err)
		}
	}
}

// NAV-5: values are normalised, so explicit defaults and no parameters give
// the same stream query and the same render-cache key; the stream query names
// only non-default values, in the order site, sort, dir.
func TestListQuery_NAV5_Normalised(t *testing.T) {
	explicit, err := parseListQuery(mustQuery(t, "sort=started&dir=desc"))
	if err != nil {
		t.Fatal(err)
	}
	empty, err := parseListQuery(url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	if explicit.cacheKey() != empty.cacheKey() || empty.cacheKey() != "list||started|desc" {
		t.Errorf("NAV-5: cache keys %q and %q, want both list||started|desc", explicit.cacheKey(), empty.cacheKey())
	}
	if explicit.stream() != "view=list" || empty.stream() != "view=list" {
		t.Errorf("NAV-5: streams %q and %q, want view=list", explicit.stream(), empty.stream())
	}

	full, err := parseListQuery(mustQuery(t, "dir=asc&sort=energy&site=038983"))
	if err != nil {
		t.Fatal(err)
	}
	if got := full.cacheKey(); got != "list|038983|energy|asc" {
		t.Errorf("NAV-5: cacheKey = %q", got)
	}
	if got := full.stream(); got != "view=list&site=038983&sort=energy&dir=asc" {
		t.Errorf("NAV-5: stream = %q", got)
	}
	if got := full.url(store.SortEnergy, false, "", false); got != "/sessions?site=038983&sort=energy&dir=asc" {
		t.Errorf("NAV-5: url = %q", got)
	}
	if got := full.url(store.SortStarted, true, "", false); got != "/sessions?site=038983" {
		t.Errorf("NAV-5: url = %q", got)
	}
	if got := empty.url(store.SortStarted, true, "", false); got != "/sessions" {
		t.Errorf("NAV-4: first page url = %q, want /sessions", got)
	}
	if got := empty.url(store.SortStarted, true, "abc", true); got != "/sessions?cursor=abc&before" {
		t.Errorf("NAV-4: before url = %q", got)
	}
	if got := empty.url(store.SortEnergy, true, "abc", false); got != "/sessions?sort=energy&cursor=abc&after" {
		t.Errorf("NAV-4: after url = %q", got)
	}
}

// NAV-4: the URLs the page builds parse back to the query they describe.
func TestListQuery_NAV4_URLRoundTrip(t *testing.T) {
	q := listQuery{Site: "038983", Sort: store.SortEnergy, Desc: false}
	c := store.ListCursor{EnergyWh: 4200, ID: testCursorID}
	link := q.url(q.Sort, q.Desc, encodeCursor(q.Sort, q.Desc, c), true)
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseListQuery(u.Query())
	if err != nil {
		t.Fatalf("NAV-4: parse %q: %v", link, err)
	}
	want := q
	want.Cursor, want.Before = &c, true
	if !sameQuery(got, want) {
		t.Errorf("NAV-4: %q parsed to %+v, want %+v", link, got, want)
	}
}
