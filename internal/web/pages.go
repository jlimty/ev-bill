package web

import (
	"bytes"
	"embed"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"github.com/jlimty/ev-bill/internal/billing"
	"github.com/jlimty/ev-bill/internal/reqid"
	"github.com/jlimty/ev-bill/internal/sessions"
	"github.com/jlimty/ev-bill/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

// pageTemplates holds one template set per page, each parsed with the layout.
// The files are embedded, so a parse failure is a programming error.
var pageTemplates = map[string]*template.Template{
	"list":   parsePage("list"),
	"detail": parsePage("detail"),
	"error":  parsePage("error"),
}

func parsePage(name string) *template.Template {
	return template.Must(template.ParseFS(templateFS, "templates/layout.html", "templates/"+name+".html"))
}

// Pages serves the status pages. Logger must be non-nil.
type Pages struct {
	Store  *store.Store
	Tariff billing.Tariff
	Now    func() time.Time
	Logger *slog.Logger
}

// Register mounts the pages on mux.
func (p *Pages) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", p.list)
	mux.HandleFunc("GET /sessions/{id}", p.detail)
}

type layoutData struct {
	Title   string
	Refresh bool
	TZ      string
}

type listRow struct {
	ID, Charger, Started, Status, Energy, Cost string
}

type listPage struct {
	layoutData
	Rows []listRow
}

type barSeg struct {
	Class, Label, Times, Width string
}

type tableRow struct {
	Label, Times, Energy, Rate, Cost string
}

type detailPage struct {
	layoutData
	Charger, Status, Started, Stopped string
	Bar                               []barSeg
	Rows                              []tableRow
	Energy, Total                     string
}

type errorPage struct {
	layoutData
	Heading, Message string
}

func (p *Pages) layout(title string, refresh bool) layoutData {
	return layoutData{Title: title, Refresh: refresh, TZ: p.Tariff.Location.String()}
}

func (p *Pages) list(w http.ResponseWriter, r *http.Request) {
	items, err := p.Store.ListRecent(r.Context(), sessions.ListLimit)
	if err != nil {
		p.serverError(w, r, "list sessions", err)
		return
	}
	now := p.Now()
	data := listPage{layoutData: p.layout("Charging sessions", true), Rows: []listRow{}}
	for _, it := range items {
		v, err := sessions.BuildView(it.Session, it.Readings, p.Tariff, now)
		if err != nil {
			p.serverError(w, r, "build view", err)
			return
		}
		data.Rows = append(data.Rows, listRow{
			ID:      v.Session.ID,
			Charger: v.Session.ChargerID,
			Started: p.clock(v.Session.StartedAt, true),
			Status:  v.Status,
			Energy:  FormatWh(v.Bill.EnergyWh),
			Cost:    FormatCents(v.Bill.TotalCents),
		})
	}
	p.render(w, r, http.StatusOK, "list", data)
}

func (p *Pages) detail(w http.ResponseWriter, r *http.Request) {
	sess, readings, err := p.Store.GetSession(r.Context(), r.PathValue("id"))
	if errors.Is(err, sessions.ErrNotFound) {
		p.render(w, r, http.StatusNotFound, "error", errorPage{
			layoutData: p.layout("Not found", false),
			Heading:    "Session not found",
			Message:    "That charging session does not exist.",
		})
		return
	}
	if err != nil {
		p.serverError(w, r, "get session", err)
		return
	}
	v, err := sessions.BuildView(sess, readings, p.Tariff, p.Now())
	if err != nil {
		p.serverError(w, r, "build view", err)
		return
	}

	data := detailPage{
		layoutData: p.layout("Session "+sess.ChargerID, v.Status != sessions.StatusCompleted),
		Charger:    sess.ChargerID,
		Status:     v.Status,
		Started:    p.clock(sess.StartedAt, true),
		Stopped:    "-",
		Energy:     FormatWh(v.Bill.EnergyWh),
		Total:      FormatCents(v.Bill.TotalCents),
	}
	if sess.StoppedAt != nil {
		data.Stopped = p.clock(*sess.StoppedAt, true)
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
	p.render(w, r, http.StatusOK, "detail", data)
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
		layoutData: p.layout("Error", false),
		Heading:    "Something went wrong",
		Message:    "Please try again in a moment.",
	})
}

// Every page response, including errors, gets these headers. The pages use one
// inline stylesheet and no scripts, images or framing.
const contentSecurityPolicy = "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'"

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
