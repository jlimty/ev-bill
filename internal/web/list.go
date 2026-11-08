package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jlimty/ev-bill/internal/mirror"
	"github.com/jlimty/ev-bill/internal/reqid"
	"github.com/jlimty/ev-bill/internal/sessions"
	"github.com/jlimty/ev-bill/internal/store"
)

// listRow is one session on the list page. SourceClass is "mirror" or
// "simulated", for the CSS; SourceLabel is the pill's text (VIEW-2).
// StatusClass is the SES-4 status, for the badge's CSS, and Status its label.
// Energy and Cost carry " est." for mirror sessions (VIEW-1). Where is
// "location · operator · kW" ("Test Mall · Testco · 7.4 kW AC"), empty
// without a chargers row (VIEW-3).
type listRow struct {
	ID, Charger, SourceClass, SourceLabel, Where, Started, Ago string
	StatusClass, Status, Energy, Cost                          string
}

// sortHeader is a sortable column's header link (NAV-3). Sort is
// "descending" or "ascending" on the active column, for aria-sort and the
// arrow, and empty on the other.
type sortHeader struct {
	URL, Sort string
}

// listSite is the site a filtered list shows (NAV-2): its name, and the link
// to every site's sessions in the same order.
type listSite struct {
	Name, AllURL string
}

// listPage is the list page and its live fragment (NAV-2..5, VIEW-1..3).
// Heading is the h1: "Sessions at {site name}" on a site filter (NAV-2),
// else "Charging sessions". FirstPage is set on a cursor page, which is not
// live; Site is nil without a filter. Count is empty when the filter has no
// sessions at all. HasPrev says whether rows lie before this page; PrevURL
// and NextURL are empty when no rows lie that way.
type listPage struct {
	layoutData
	Heading                string
	Notice                 string
	Feed                   feedLine
	FirstPage              string
	Site                   *listSite
	Caption, Count, Empty  string
	CostHeader             string
	Started, Energy        sortHeader
	Rows                   []listRow
	TotalLabel             string
	TotalEnergy, TotalCost string
	HasPrev                bool
	PrevURL, NextURL       string
}

const (
	// estSuffix marks an estimated figure (VIEW-1).
	estSuffix = " est."
	// showAllSessions is the error pages' link back to the list (NAV-2, NAV-3).
	showAllSessions = "Show all sessions"
)

// listNotice is the list page's notice (VIEW-1, LIVE-4): the rows' own "est."
// labels mark which figures are estimates, so it says only how.
var listNotice = "Real chargers: energy is estimated at rated power (DC capped at " +
	FormatKW(mirror.DCMaxW) + `); "occupied" includes reserved connectors.`

// estimated appends " est." to a mirror session's figure (VIEW-1).
func estimated(figure string, est bool) string {
	if est {
		return figure + estSuffix
	}
	return figure
}

// list serves GET /sessions (NAV-2..5). A bad query is a 400 and an unknown
// site a 404, each on an error page that does not echo the input. A Before
// page that reaches the start of the order redirects to the live first page.
func (p *Pages) list(w http.ResponseWriter, r *http.Request) {
	q, err := parseListQuery(r.URL.Query())
	if err != nil {
		p.badList(w, r, q, err)
		return
	}
	data, err := p.listData(r.Context(), q)
	if errors.Is(err, errUnknownSite) {
		p.listError(w, r, http.StatusNotFound, "Not found", "Site not found",
			"No watched site has postal code "+q.Site+".", "/sessions", showAllSessions)
		return
	}
	if err != nil {
		p.serverError(w, r, navSessions, "list page", err)
		return
	}
	if q.Before && !data.HasPrev {
		setSecurityHeaders(w)
		// The target is always a path starting "/sessions", whose only input
		// is a site parseListQuery has checked is six digits, so it cannot
		// point at another host.
		http.Redirect(w, r, q.firstPage(), http.StatusFound) //nolint:gosec // G710: same-origin path, see above
		return
	}
	p.render(w, r, http.StatusOK, "list", data)
}

// badList answers a query parseListQuery refused with a 400 error page. The
// reason goes to the log only. A paging error links to the first page of the
// query's own filter and sort, which parseListQuery left valid in q.
func (p *Pages) badList(w http.ResponseWriter, r *http.Request, q listQuery, err error) {
	reqid.Logger(r.Context(), p.Logger).Info("bad list query", "error", err)
	switch {
	case errors.Is(err, errBadPostal):
		p.listError(w, r, http.StatusBadRequest, "Bad request", "Not a postal code",
			"Postal codes have 6 digits.", "/sessions", showAllSessions)
	case errors.Is(err, errBadCursor):
		p.listError(w, r, http.StatusBadRequest, "Bad request", "Page link not valid",
			"This page link is malformed or out of date.", q.firstPage(), "Back to the first page")
	default:
		p.listError(w, r, http.StatusBadRequest, "Bad request", "Sort not available",
			"Sessions sort by start time or by energy.", "/sessions", showAllSessions)
	}
}

// listError renders an error page under /sessions, which marks Sessions (NAV-1).
func (p *Pages) listError(w http.ResponseWriter, r *http.Request, status int, title, heading, message, link, linkText string) {
	data := p.layout(title, "")
	data.Nav = navSessions
	p.render(w, r, status, "error", errorPage{layoutData: data, Heading: heading, Message: message, LinkURL: link, LinkText: linkText})
}

// listData builds the list page's data for q, which the list_body fragment
// also uses (NAV-2..5, VIEW-1..3). An unknown site is errUnknownSite. Only a
// first page streams. The totals add up the rows' int64 values and are
// formatted once.
func (p *Pages) listData(ctx context.Context, q listQuery) (listPage, error) {
	now := p.Now()
	data := listPage{
		layoutData: p.layout("Charging sessions", ""),
		Heading:    "Charging sessions",
		Notice:     listNotice,
		Caption:    "Sessions",
		Empty:      "No sessions yet. Sessions appear here as soon as a charger starts one.",
		CostHeader: "Cost (" + tariffName(p.Tariff) + " tariff)",
		Rows:       []listRow{},
	}
	data.Nav = navSessions
	if q.Site != "" {
		name, found, err := p.Store.SiteName(ctx, q.Site)
		if err != nil {
			return listPage{}, err
		}
		if !found {
			return listPage{}, errUnknownSite
		}
		all := q
		all.Site = ""
		data.Site = &listSite{Name: name, AllURL: all.firstPage()}
		heading := "Sessions at " + name
		data.Title, data.Heading, data.Caption, data.Empty = heading, heading, heading, "No sessions at this site yet."
	}
	if q.Cursor == nil {
		data.Stream = q.stream()
		feed, err := p.feedLine(ctx, now, p.feedState())
		if err != nil {
			return listPage{}, err
		}
		data.Feed = feed
	} else {
		data.FirstPage = q.firstPage()
		data.Caption += ", continued"
	}
	data.Started, data.Energy = q.sortHeader(store.SortStarted), q.sortHeader(store.SortEnergy)

	sq := store.ListQuery{Site: q.Site, Sort: q.Sort, Desc: q.Desc, Limit: sessions.ListLimit}
	if q.Before {
		sq.Before = q.Cursor
	} else {
		sq.After = q.Cursor
	}
	page, err := p.Store.ListSessions(ctx, sq)
	if err != nil {
		return listPage{}, err
	}
	var energyWh, costCents int64
	var anyEst bool
	var first, last store.ListCursor
	for i, it := range page.Items {
		v, err := sessions.BuildView(it.Session, it.Readings, p.Tariff, now)
		if err != nil {
			return listPage{}, fmt.Errorf("build view: %w", err)
		}
		est := it.Session.Source == sessions.SourceMirror
		row := listRow{
			ID:          v.Session.ID,
			Charger:     v.Session.ChargerID,
			SourceClass: sourceClass(v.Session.Source),
			SourceLabel: sourceLabel(v.Session.Source),
			Started:     p.clock(v.Session.StartedAt, true),
			Ago:         FormatAgo(now.Sub(v.Session.StartedAt)),
			StatusClass: v.Status,
			Status:      statusLabel(v.Session.Source, v.Status),
			Energy:      estimated(FormatWh(v.Bill.EnergyWh), est),
			Cost:        estimated(FormatCents(v.Bill.TotalCents), est),
		}
		if c := it.Charger; c != nil && showsCharger(it.Session) {
			row.Where = joinNonEmpty(" · ", c.LocationName, c.Operator, chargerPower(c))
		}
		data.Rows = append(data.Rows, row)
		energyWh += v.Bill.EnergyWh
		costCents += v.Bill.TotalCents
		anyEst = anyEst || est
		// The energy cursor is the billed energy, which equals the SQL's (NAV-3).
		c := store.ListCursor{StartedAt: v.Session.StartedAt, EnergyWh: v.Bill.EnergyWh, ID: v.Session.ID}
		if i == 0 {
			first = c
		}
		last = c
	}
	n := len(data.Rows)
	if page.Total > 0 {
		data.Count = groupThousands(int64(n)) + " on this page · " + groupThousands(page.Total) + " in all"
		data.Empty = "No sessions on this page."
	}
	data.HasPrev = page.HasPrev
	switch {
	case page.HasPrev && n == 0:
		// Past the end, so there is no row to page back from: start again.
		data.PrevURL = q.firstPage()
	case page.HasPrev:
		data.PrevURL = q.url(q.Sort, q.Desc, encodeCursor(q.Sort, q.Desc, first), true)
	}
	if page.HasNext && n > 0 {
		data.NextURL = q.url(q.Sort, q.Desc, encodeCursor(q.Sort, q.Desc, last), false)
	}
	data.TotalLabel = "Total, these " + groupThousands(int64(n)) + " sessions"
	if n == 1 {
		data.TotalLabel = "Total, this 1 session"
	}
	data.TotalEnergy, data.TotalCost = estimated(FormatWh(energyWh), anyEst), estimated(FormatCents(costCents), anyEst)
	return data, nil
}

// sortHeader is the header link of a sortable column (NAV-3). On the active
// column it reverses dir; on the other it starts that column's default order,
// descending. Both drop the cursor.
func (q listQuery) sortHeader(col store.SessionSort) sortHeader {
	if col != q.Sort {
		return sortHeader{URL: q.url(col, true, "", false)}
	}
	h := sortHeader{URL: q.url(col, !q.Desc, "", false), Sort: "ascending"}
	if q.Desc {
		h.Sort = "descending"
	}
	return h
}
