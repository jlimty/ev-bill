package web

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jlimty/ev-bill/internal/billing"
	"github.com/jlimty/ev-bill/internal/live"
	"github.com/jlimty/ev-bill/internal/mirror"
	"github.com/jlimty/ev-bill/internal/reqid"
	"github.com/jlimty/ev-bill/internal/sessions"
	"github.com/jlimty/ev-bill/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

// staticFS holds the one script the live pages load (LIVE-3).
//
//go:embed static/live.js
var staticFS embed.FS

// pageTemplates holds one template set per page, each parsed with the layout
// and the fragments the page includes. The event stream executes the same
// fragments from these sets, so the page and the stream cannot disagree.
// The files are embedded, so a parse failure is a programming error.
var pageTemplates = map[string]*template.Template{
	"list":   parsePage("list", "list_body"),
	"detail": parsePage("detail", "detail_live"),
	"error":  parsePage("error"),
}

func parsePage(name string, fragments ...string) *template.Template {
	files := []string{"templates/layout.html", "templates/" + name + ".html"}
	for _, f := range fragments {
		files = append(files, "templates/"+f+".html")
	}
	return template.Must(template.ParseFS(templateFS, files...))
}

// Pages serves the status pages and their event stream. Logger must be
// non-nil. Hub may be nil, which turns GET /events into a 503. Feed is the
// mirror's health, shown on the list page; nil means the mirror is off.
type Pages struct {
	Store  *store.Store
	Tariff billing.Tariff
	Now    func() time.Time
	Logger *slog.Logger
	Hub    *live.Hub
	Feed   *live.Feed
}

// Register mounts the pages on mux. The event stream is mounted separately,
// by RegisterEvents, so it can sit outside the per-request deadline.
func (p *Pages) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", p.list)
	mux.HandleFunc("GET /sessions/{id}", p.detail)
	mux.HandleFunc("GET /static/live.js", p.liveJS)
}

// layoutData is what the layout needs. Stream is the query live.js passes to
// GET /events, such as "view=list"; it is empty on pages that do not update,
// which then load no script and do not refresh (LIVE-3).
type layoutData struct {
	Title  string
	Stream string
	Tariff string
	TZ     string
}

// listRow is one session on the list page. Source is "mirror" or "simulated";
// Location and Info ("Testco · 7.4 kW AC") are empty without a chargers row.
type listRow struct {
	ID, Charger, Source, Location, Info, Started, Ago, Status, Energy, Cost string
}

// feedLine is the mirror's status line on the list page (LIVE-4).
type feedLine struct {
	Text string
	Warn bool
}

type listPage struct {
	layoutData
	Notice                 string
	Feed                   feedLine
	Rows                   []listRow
	TotalEnergy, TotalCost string
}

type barSeg struct {
	Class, Label, Times, Width string
}

type tableRow struct {
	Label, Times, Energy, Rate, Cost string
}

type detailPage struct {
	layoutData
	Charger, Source, Status, Started, Stopped string
	Location, Operator, Power, Notice         string
	Bar                                       []barSeg
	Rows                                      []tableRow
	Energy, Total                             string
}

type errorPage struct {
	layoutData
	Heading, Message string
}

func (p *Pages) layout(title, stream string) layoutData {
	return layoutData{Title: title, Stream: stream, Tariff: tariffHeader(p.Tariff), TZ: p.Tariff.Location.String()}
}

const (
	// feedDelayedAfter is the age of the last good poll beyond which the feed
	// line turns into a warning (LIVE-4).
	feedDelayedAfter = 15 * time.Minute
	// sourceSimulated labels API sessions; mirror sessions show "mirror".
	sourceSimulated = "simulated"
)

// estimateNotice explains the mirror's estimates (LIVE-4). The cap comes from
// the mirror's own constant, so the page cannot disagree with the reconciler.
var estimateNotice = "Mirrored chargers: energy and cost are estimates at rated power (DC capped at " +
	FormatKW(mirror.DCMaxW) + `); "occupied" includes reserved connectors.`

// tariffHeader describes the tariff from its own values (LIVE-4), e.g.
// "Peak 07:00–23:00 · 62¢/kWh · Off-peak 23:00–07:00 · 48¢/kWh · Singapore time".
func tariffHeader(t billing.Tariff) string {
	peak := fmt.Sprintf("%02d:00–%02d:00", t.PeakStartHour, t.PeakEndHour)
	offPeak := fmt.Sprintf("%02d:00–%02d:00", t.PeakEndHour, t.PeakStartHour)
	return fmt.Sprintf("Peak %s · %d¢/kWh · Off-peak %s · %d¢/kWh · %s time",
		peak, t.PeakRateCentsPerKWh, offPeak, t.OffPeakRateCentsPerKWh, zoneCity(t.Location))
}

// zoneCity is the city part of a zone name: "Asia/Kuala_Lumpur" -> "Kuala Lumpur".
func zoneCity(loc *time.Location) string {
	name := loc.String()
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	return strings.ReplaceAll(name, "_", " ")
}

// sourceLabel is the row label for a session's source (LIVE-4).
func sourceLabel(s sessions.Source) string {
	if s == sessions.SourceMirror {
		return string(sessions.SourceMirror)
	}
	return sourceSimulated
}

// chargerPower formats a charger's rated power and current, e.g. "7.4 kW AC".
func chargerPower(c *store.ChargerRow) string {
	return strings.TrimSpace(FormatKW(c.PowerW) + " " + c.Current)
}

func (p *Pages) list(w http.ResponseWriter, r *http.Request) {
	data, err := p.listData(r.Context())
	if err != nil {
		p.serverError(w, r, "list page", err)
		return
	}
	p.render(w, r, http.StatusOK, "list", data)
}

// listData builds the list page's data, which the list_body fragment also
// uses. The totals add up the rows' int64 values and are formatted once.
func (p *Pages) listData(ctx context.Context) (listPage, error) {
	now := p.Now()
	feed, err := p.feedLine(ctx, now)
	if err != nil {
		return listPage{}, err
	}
	items, err := p.Store.ListRecent(ctx, sessions.ListLimit)
	if err != nil {
		return listPage{}, fmt.Errorf("list sessions: %w", err)
	}
	data := listPage{layoutData: p.layout("Charging sessions", "view=list"), Notice: estimateNotice, Feed: feed, Rows: []listRow{}}
	var energyWh, costCents int64
	for _, it := range items {
		v, err := sessions.BuildView(it.Session, it.Readings, p.Tariff, now)
		if err != nil {
			return listPage{}, fmt.Errorf("build view: %w", err)
		}
		row := listRow{
			ID:      v.Session.ID,
			Charger: v.Session.ChargerID,
			Source:  sourceLabel(v.Session.Source),
			Started: p.clock(v.Session.StartedAt, true),
			Ago:     FormatAgo(now.Sub(v.Session.StartedAt)),
			Status:  v.Status,
			Energy:  FormatWh(v.Bill.EnergyWh),
			Cost:    FormatCents(v.Bill.TotalCents),
		}
		if c := it.Charger; c != nil {
			row.Location = c.LocationName
			row.Info = joinNonEmpty(" · ", c.Operator, chargerPower(c))
		}
		data.Rows = append(data.Rows, row)
		energyWh += v.Bill.EnergyWh
		costCents += v.Bill.TotalCents
	}
	data.TotalEnergy, data.TotalCost = FormatWh(energyWh), FormatCents(costCents)
	return data, nil
}

// feedLine builds the mirror's status line (LIVE-4): off, waiting for the
// first good poll, live, or delayed once the last good poll is more than
// feedDelayedAfter old, plus the connectors held after a 48 h stop (MIR-12).
func (p *Pages) feedLine(ctx context.Context, now time.Time) (feedLine, error) {
	var st live.FeedState
	if p.Feed != nil {
		st = p.Feed.Load()
	}
	if !st.Enabled {
		return feedLine{Text: "Live mirror off"}, nil
	}
	held, err := p.Store.HeldCount(ctx)
	if err != nil {
		return feedLine{}, err
	}
	var line feedLine
	switch age := now.Sub(st.LastGood); {
	case st.LastGood.IsZero():
		line.Text = "Waiting for the first live update"
	case age > feedDelayedAfter:
		line = feedLine{Text: "Live feed delayed · last update " + p.lastUpdate(st.LastGood, age), Warn: true}
	default:
		line.Text = "Live from LTA DataMall · last update " + p.lastUpdate(st.LastGood, age)
	}
	switch {
	case held == 1:
		line.Text += " · 1 connector occupied over 48 h, not billed"
	case held > 1:
		line.Text += fmt.Sprintf(" · %d connectors occupied over 48 h, not billed", held)
	}
	return line, nil
}

// lastUpdate formats a feed time as "13:45 (2 min ago)".
func (p *Pages) lastUpdate(t time.Time, age time.Duration) string {
	return p.clock(t, false) + " (" + FormatAgo(age) + ")"
}

// joinNonEmpty joins the non-empty parts with sep.
func joinNonEmpty(sep string, parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, s := range parts {
		if s != "" {
			kept = append(kept, s)
		}
	}
	return strings.Join(kept, sep)
}

func (p *Pages) detail(w http.ResponseWriter, r *http.Request) {
	data, err := p.detailData(r.Context(), r.PathValue("id"))
	if errors.Is(err, sessions.ErrNotFound) {
		p.render(w, r, http.StatusNotFound, "error", errorPage{
			layoutData: p.layout("Not found", ""),
			Heading:    "Session not found",
			Message:    "That charging session does not exist.",
		})
		return
	}
	if err != nil {
		p.serverError(w, r, "detail page", err)
		return
	}
	p.render(w, r, http.StatusOK, "detail", data)
}

// detailData builds the detail page's data, which the detail_live fragment
// also uses. An unknown or malformed id is sessions.ErrNotFound.
func (p *Pages) detailData(ctx context.Context, id string) (detailPage, error) {
	sess, readings, err := p.Store.GetSession(ctx, id)
	if err != nil {
		return detailPage{}, fmt.Errorf("get session: %w", err)
	}
	v, err := sessions.BuildView(sess, readings, p.Tariff, p.Now())
	if err != nil {
		return detailPage{}, fmt.Errorf("build view: %w", err)
	}

	charger, found, err := p.Store.Charger(ctx, sess.ChargerID)
	if err != nil {
		return detailPage{}, err
	}

	stream := ""
	if v.Status != sessions.StatusCompleted {
		stream = "view=session&id=" + url.QueryEscape(sess.ID)
	}
	data := detailPage{
		layoutData: p.layout("Session "+sess.ChargerID, stream),
		Charger:    sess.ChargerID,
		Source:     sourceLabel(sess.Source),
		Status:     v.Status,
		Started:    p.clock(sess.StartedAt, true),
		Stopped:    "-",
		Energy:     FormatWh(v.Bill.EnergyWh),
		Total:      FormatCents(v.Bill.TotalCents),
	}
	if sess.StoppedAt != nil {
		data.Stopped = p.clock(*sess.StoppedAt, true)
	}
	if found {
		data.Location, data.Operator, data.Power = charger.LocationName, charger.Operator, chargerPower(&charger)
	}
	if sess.Source == sessions.SourceMirror {
		data.Notice = estimateNotice
	}
	widths := BarWidths(v.Bill.Segments)
	dated := lasts24h(v.Bill.Segments)
	for i, s := range v.Bill.Segments {
		label, class := windowLabel(s.Window)
		times := p.segmentTimes(s, dated)
		data.Rows = append(data.Rows, tableRow{
			Label:  label,
			Times:  times,
			Energy: FormatWh(s.EnergyWh),
			Rate:   FormatCents(s.RateCentsPerKWh) + "/kWh",
			Cost:   FormatCents(s.CostCents),
		})
		if i < len(widths) {
			data.Bar = append(data.Bar, barSeg{Class: class, Label: label, Times: times, Width: widths[i]})
		}
	}
	return data, nil
}

// clock formats t in the tariff's location: "HH:MM", or "DD Mon YYYY HH:MM" with a date.
func (p *Pages) clock(t time.Time, withDate bool) string {
	layout := "15:04"
	if withDate {
		layout = "02 Jan 2006 15:04"
	}
	return t.In(p.Tariff.Location).Format(layout)
}

// lasts24h reports whether the segments cover 24 hours or more. Only then can
// two rows share the same times of day, so only then do rows show dates.
func lasts24h(segs []billing.Segment) bool {
	return len(segs) > 0 && segs[len(segs)-1].End.Sub(segs[0].Start) >= 24*time.Hour
}

// segmentTimes formats a segment's span: "HH:MM – HH:MM", or with dates when
// dated: "02 Jan 15:04 – 15:04" within one local day, and
// "02 Jan 15:04 – 03 Jan 15:04" across midnight.
func (p *Pages) segmentTimes(s billing.Segment, dated bool) string {
	if !dated {
		return p.clock(s.Start, false) + " – " + p.clock(s.End, false)
	}
	loc := p.Tariff.Location
	start, end := s.Start.In(loc), s.End.In(loc)
	endLayout := "02 Jan 15:04"
	if sameDay(start, end) {
		endLayout = "15:04"
	}
	return start.Format("02 Jan 15:04") + " – " + end.Format(endLayout)
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

func windowLabel(w billing.Window) (label, class string) {
	if w == billing.Peak {
		return "Peak", "peak"
	}
	return "Off-peak", "offpeak"
}

// serverError logs the real error and shows a generic page.
func (p *Pages) serverError(w http.ResponseWriter, r *http.Request, what string, err error) {
	reqid.Logger(r.Context(), p.Logger).Error("status page failed", "op", what, "error", err)
	p.render(w, r, http.StatusInternalServerError, "error", errorPage{
		layoutData: p.layout("Error", ""),
		Heading:    "Something went wrong",
		Message:    "Please try again in a moment.",
	})
}

// Every page response, including errors, live.js and the event stream, gets
// these headers. The pages use one inline stylesheet, one same-origin script
// (live.js) and one same-origin connection (GET /events), and no images or
// framing (LIVE-3).
const contentSecurityPolicy = "default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'unsafe-inline'; frame-ancestors 'none'"

func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
}

// render executes into a buffer first so a template error never sends half a page.
func (p *Pages) render(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	setSecurityHeaders(w)
	var buf bytes.Buffer
	if err := pageTemplates[name].ExecuteTemplate(&buf, "layout", data); err != nil {
		reqid.Logger(r.Context(), p.Logger).Error("render template failed", "template", name, "error", err)
		http.Error(w, "Something went wrong", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if _, err := buf.WriteTo(w); err != nil {
		reqid.Logger(r.Context(), p.Logger).Error("write response failed", "error", err)
	}
}

// liveJS serves GET /static/live.js (LIVE-3). The Content-Type is set first,
// so ServeFileFS keeps it rather than guessing from the system's MIME tables.
func (p *Pages) liveJS(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFileFS(w, r, staticFS, "static/live.js")
}
