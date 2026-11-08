package web

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jlimty/ev-bill/internal/store"
)

// The list query's errors. Each one wraps the reason, which is for the log;
// the page shows only fixed copy, never the input (NAV-2, NAV-3, NAV-4).
var (
	// errBadQuery is a sort, dir or paging parameter the list does not take: 400.
	errBadQuery = errors.New("bad list query")
	// errBadCursor is a malformed cursor or direction: 400, linking to the
	// first page of the same filter and sort. It is also errBadQuery.
	errBadCursor = fmt.Errorf("%w: paging", errBadQuery)
	// errBadPostal is a site that is not six digits: 400.
	errBadPostal = errors.New("malformed postal code")
	// errUnknownSite is a well-formed site no chargers row has: 404.
	errUnknownSite = errors.New("unknown site")
)

// maxCursorLen is the longest cursor the list accepts (NAV-4). A valid one is
// at most about 105 characters.
const maxCursorLen = 128

// cursorTime is a cursor's started_at: UTC, to the microsecond, as Postgres
// stores it (NAV-4).
const cursorTime = "2006-01-02T15:04:05.000000Z07:00"

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// listQuery is a parsed, normalised /sessions query (NAV-2..5). Cursor is the
// page boundary, nil on a first page; Before says the page is the one before
// it, rather than after.
type listQuery struct {
	Site   string
	Sort   store.SessionSort
	Desc   bool
	Cursor *store.ListCursor
	Before bool
}

// parseListQuery reads site, sort, dir, cursor and before or after from v.
// Each takes its first value, and an empty one means the default: all sites,
// started, desc (NAV-3). before and after are bare flags, so their value must
// be empty, and exactly one of them comes with a cursor (NAV-4). Other
// parameters are ignored. On a cursor error the returned query still holds the
// valid site, sort and dir, for the link to the first page.
func parseListQuery(v url.Values) (listQuery, error) {
	q := listQuery{Site: v.Get("site"), Desc: true}
	if q.Site != "" && !isPostal(q.Site) {
		return listQuery{}, errBadPostal
	}
	switch v.Get("sort") {
	case "", "started":
	case "energy":
		q.Sort = store.SortEnergy
	default:
		return listQuery{}, fmt.Errorf("%w: unknown sort", errBadQuery)
	}
	switch v.Get("dir") {
	case "", "desc":
	case "asc":
		q.Desc = false
	default:
		return listQuery{}, fmt.Errorf("%w: unknown dir", errBadQuery)
	}

	before, after := v.Has("before"), v.Has("after")
	if v.Get("before") != "" || v.Get("after") != "" {
		return q, fmt.Errorf("%w: before and after take no value", errBadCursor)
	}
	raw := v.Get("cursor")
	switch {
	case raw == "" && !before && !after:
		return q, nil
	case raw == "":
		return q, fmt.Errorf("%w: a direction without a cursor", errBadCursor)
	case before == after:
		return q, fmt.Errorf("%w: a cursor needs exactly one of before and after", errBadCursor)
	}
	c, err := decodeCursor(raw, q.Sort, q.Desc)
	if err != nil {
		return q, err
	}
	q.Cursor, q.Before = &c, before
	return q, nil
}

// isPostal reports whether s is a Singapore postal code: six ASCII digits.
func isPostal(s string) bool {
	if len(s) != 6 {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func sortName(s store.SessionSort) string {
	if s == store.SortEnergy {
		return "energy"
	}
	return "started"
}

func dirName(desc bool) string {
	if desc {
		return "desc"
	}
	return "asc"
}

// encodeCursor encodes a page boundary as base64url, without padding, of
// "sort|dir|value|id" (NAV-4). value is started_at in UTC to the microsecond
// for the started sort, or the energy in Wh.
func encodeCursor(sort store.SessionSort, desc bool, c store.ListCursor) string {
	value := c.StartedAt.UTC().Truncate(time.Microsecond).Format(cursorTime)
	if sort == store.SortEnergy {
		value = strconv.FormatInt(c.EnergyWh, 10)
	}
	payload := sortName(sort) + "|" + dirName(desc) + "|" + value + "|" + c.ID
	return base64.RawURLEncoding.EncodeToString([]byte(payload))
}

// decodeCursor decodes a cursor made by encodeCursor for this sort and dir.
// Anything else is errBadCursor: over maxCursorLen characters, not strict
// unpadded base64url, a field count other than 4, another sort or dir, a time
// or Wh not in encodeCursor's exact form (a negative Wh included), or an id
// that is not a UUID (NAV-4).
func decodeCursor(s string, sort store.SessionSort, desc bool) (store.ListCursor, error) {
	if len(s) > maxCursorLen {
		return store.ListCursor{}, fmt.Errorf("%w: cursor too long", errBadCursor)
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil {
		return store.ListCursor{}, fmt.Errorf("%w: cursor is not base64url", errBadCursor)
	}
	f := strings.Split(string(raw), "|")
	if len(f) != 4 {
		return store.ListCursor{}, fmt.Errorf("%w: cursor has %d fields", errBadCursor, len(f))
	}
	if f[0] != sortName(sort) || f[1] != dirName(desc) {
		return store.ListCursor{}, fmt.Errorf("%w: cursor is for another sort or dir", errBadCursor)
	}
	if !uuidPattern.MatchString(f[3]) {
		return store.ListCursor{}, fmt.Errorf("%w: cursor id is not a UUID", errBadCursor)
	}
	c := store.ListCursor{ID: strings.ToLower(f[3])}
	if sort == store.SortEnergy {
		wh, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil || wh < 0 || strconv.FormatInt(wh, 10) != f[2] {
			return store.ListCursor{}, fmt.Errorf("%w: cursor Wh is malformed", errBadCursor)
		}
		c.EnergyWh = wh
		return c, nil
	}
	at, err := time.Parse(cursorTime, f[2])
	if err != nil || at.UTC().Format(cursorTime) != f[2] {
		return store.ListCursor{}, fmt.Errorf("%w: cursor time is malformed", errBadCursor)
	}
	c.StartedAt = at.UTC()
	return c, nil
}

// params writes the non-default site, sort and dir, in that order, as
// "&name=value" pairs.
func (q listQuery) params(sort store.SessionSort, desc bool) string {
	s := ""
	if q.Site != "" {
		s += "&site=" + url.QueryEscape(q.Site)
	}
	if sort != store.SortStarted {
		s += "&sort=" + sortName(sort)
	}
	if !desc {
		s += "&dir=" + dirName(desc)
	}
	return s
}

// stream is the /events query for this filter and sort (NAV-5): "view=list"
// plus only the non-default site, sort and dir, in that order.
func (q listQuery) stream() string {
	return "view=list" + q.params(q.Sort, q.Desc)
}

// cacheKey is the render-cache key for this filter and sort (NAV-5), always
// explicit, so equivalent queries share one key: "list|<site>|started|desc".
func (q listQuery) cacheKey() string {
	return "list|" + q.Site + "|" + sortName(q.Sort) + "|" + dirName(q.Desc)
}

// url is the /sessions URL of this query's site with the given sort and dir:
// only the non-default site, sort and dir, then, when cursor is not empty,
// the cursor and "before" or "after" (NAV-3, NAV-4). Without a cursor it is
// that sort's first page.
func (q listQuery) url(sort store.SessionSort, desc bool, cursor string, before bool) string {
	query := q.params(sort, desc)
	if cursor != "" {
		dir := "after"
		if before {
			dir = "before"
		}
		query += "&cursor=" + url.QueryEscape(cursor) + "&" + dir
	}
	if query == "" {
		return "/sessions"
	}
	return "/sessions?" + query[1:]
}

// firstPage is the cursorless, live URL of this query (NAV-4, NAV-5).
func (q listQuery) firstPage() string {
	return q.url(q.Sort, q.Desc, "", false)
}
