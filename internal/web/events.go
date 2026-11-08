package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jlimty/ev-bill/internal/reqid"
	"github.com/jlimty/ev-bill/internal/sessions"
)

const (
	// heartbeatEvery is the gap after which an idle stream sends a comment,
	// so proxies and browsers do not drop it as dead (LIVE-1).
	heartbeatEvery = 25 * time.Second
	// eventWriteTimeout is the write deadline set before each event (LIVE-1).
	// The stream is outside the server's per-request deadline and outlives its
	// 10 s WriteTimeout, so every write sets its own.
	eventWriteTimeout = 10 * time.Second
	// renderTimeout bounds the database work behind each event, as the
	// per-request deadline does for the pages, which the stream sits outside.
	// It includes the wait for a render slot (maxConcurrentRenders).
	renderTimeout = 8 * time.Second
	// renderGap is the shortest time between two renders for one stream
	// (LIVE-1). Signals that arrive within it become one update when it
	// ends, so a burst of writes costs each viewer at most two renders. The
	// first event on connect is not delayed.
	renderGap = 2 * time.Second
)

// doneFrame tells live.js that a completed session will not change again, so
// it closes the stream rather than reconnecting (LIVE-1).
const doneFrame = "event: done\ndata: 1\n\n"

// RegisterEvents mounts GET /events on mux (LIVE-1). The caller mounts it
// outside the per-request deadline, which would otherwise end the stream.
func (p *Pages) RegisterEvents(mux *http.ServeMux) {
	mux.HandleFunc("GET /events", p.events)
}

// events serves GET /events?view=list (with the list page's site, sort and
// dir, NAV-5), GET /events?view=locations (LOC-7) and
// GET /events?view=session&id=<uuid> as a server-sent event stream: the current fragment straight away, then a
// re-rendered one after hub signals, at most one per renderGap, and a
// heartbeat when idle (LIVE-1). Renders are shared between the streams on one
// view through p.renders. A HEAD request (which "GET /events" also matches)
// gets the stream's headers and no body, and holds no stream slot.
func (p *Pages) events(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	if p.Hub == nil {
		http.Error(w, "Live updates are unavailable.", http.StatusServiceUnavailable)
		return
	}

	q := r.URL.Query()
	view := q.Get("view")
	var key string
	var render fragmentFunc
	switch view {
	case "list":
		lq, ok := p.streamListQuery(w, r, q)
		if !ok {
			return
		}
		key = lq.cacheKey()
		render = func(ctx context.Context) (fragment, error) { return p.listFragment(ctx, lq) }
	case "locations":
		key, render = "locations", p.locationsFragment
	case "session":
		id := q.Get("id")
		if id == "" {
			http.Error(w, "The session view needs an id.", http.StatusBadRequest)
			return
		}
		key = "session:" + id
		render = func(ctx context.Context) (fragment, error) { return p.detailFragment(ctx, id) }
	default:
		http.Error(w, "Unknown view.", http.StatusBadRequest)
		return
	}
	if r.Method == http.MethodHead {
		setStreamHeaders(w)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Subscribe before the first render, so a change made while it renders
	// still produces an event.
	sub, cancel, ok := p.Hub.Subscribe()
	if !ok {
		http.Error(w, "Too many live connections. Please try again later.", http.StatusServiceUnavailable)
		return
	}
	defer cancel()
	release := p.renders.hold(key)
	defer release()

	// The first event may reuse a render of the current generation made
	// within renderGap, so viewers arriving together share one render, but
	// not an older one: the page it lands on was rendered just now.
	first, err := p.cachedRender(key, renderGap, render)(r.Context())
	if errors.Is(err, sessions.ErrNotFound) {
		http.Error(w, "Session not found.", http.StatusNotFound)
		return
	}
	if err != nil {
		reqid.Logger(r.Context(), p.Logger).Error("event stream failed", "op", "first render", "error", err)
		http.Error(w, "Something went wrong.", http.StatusInternalServerError)
		return
	}
	p.stream(w, r, view, sub, first, p.cachedRender(key, 0, render))
}

// streamListQuery parses the list view's site, sort and dir as the page does
// (NAV-5). An invalid value, a site no chargers row has, or a cursor (only
// first pages are live) is a plain 400 that does not echo the input; a failed
// site lookup is a 500. ok is false once it has answered.
func (p *Pages) streamListQuery(w http.ResponseWriter, r *http.Request, v url.Values) (q listQuery, ok bool) {
	q, err := parseListQuery(v)
	if err == nil && q.Cursor != nil {
		err = fmt.Errorf("%w: only first pages stream", errBadCursor)
	}
	if err != nil {
		reqid.Logger(r.Context(), p.Logger).Info("bad list stream query", "error", err)
		http.Error(w, "Bad list query.", http.StatusBadRequest)
		return listQuery{}, false
	}
	if q.Site == "" {
		return q, true
	}
	_, found, err := p.Store.SiteName(r.Context(), q.Site)
	if err != nil {
		reqid.Logger(r.Context(), p.Logger).Error("event stream failed", "op", "site name", "error", err)
		http.Error(w, "Something went wrong.", http.StatusInternalServerError)
		return listQuery{}, false
	}
	if !found {
		http.Error(w, "Unknown site.", http.StatusBadRequest)
		return listQuery{}, false
	}
	return q, true
}

// cachedRender renders key through the shared cache at the hub's current
// generation. maxAge is passed to renderCache.get.
func (p *Pages) cachedRender(key string, maxAge time.Duration, render fragmentFunc) fragmentFunc {
	return func(ctx context.Context) (fragment, error) {
		return p.renders.get(ctx, key, p.Hub.Gen(), maxAge, render)
	}
}

// setStreamHeaders sets the event stream's own response headers.
func setStreamHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // ask proxies not to buffer the stream
}

// stream writes first as an event, then one re-rendered event after signals
// on sub, at most one per renderGap: a signal within renderGap of the last
// render waits for the gap to end, and any more signals meanwhile join it. It
// also writes a heartbeat after each idle heartbeatEvery. It returns after
// sending a final fragment (a completed session) and the done event, when sub
// is closed (the hub stopped, which is what lets http.Server.Shutdown finish,
// since Shutdown does not cancel request contexts), when the request context
// is done, or when an event cannot be rendered or delivered.
func (p *Pages) stream(w http.ResponseWriter, r *http.Request, event string, sub <-chan struct{}, first fragment, next fragmentFunc) {
	log := reqid.Logger(r.Context(), p.Logger)
	rc := http.NewResponseController(w)

	setStreamHeaders(w)
	w.WriteHeader(http.StatusOK)

	if !sendFragment(w, rc, log, event, first) {
		return
	}
	lastRender := time.Now()
	heartbeat := time.NewTicker(heartbeatEvery)
	defer heartbeat.Stop()
	var gap <-chan time.Time // not nil while an update waits for the gap to end
	for {
		select {
		case <-r.Context().Done():
			return
		case _, open := <-sub:
			if !open {
				return
			}
			if gap != nil {
				continue // the update already due at the gap's end covers this
			}
			if wait := renderGap - time.Since(lastRender); wait > 0 {
				gap = time.After(wait)
				continue
			}
		case <-gap:
			gap = nil
		case <-heartbeat.C:
			if err := writeEvent(w, rc, []byte(": ping\n\n")); err != nil {
				log.Info("event stream ended", "reason", "write failed", "error", err)
				return
			}
			continue
		}
		lastRender = time.Now()
		frag, err := next(r.Context())
		if err != nil {
			logRenderEnd(log, err)
			return
		}
		if !sendFragment(w, rc, log, event, frag) {
			return
		}
	}
}

// sendFragment writes frag as an event, followed by the done event when frag
// is final. It reports whether the stream goes on: false after a final
// fragment or a failed write, which it logs.
func sendFragment(w http.ResponseWriter, rc *http.ResponseController, log *slog.Logger, event string, frag fragment) bool {
	frame := eventFrame(event, frag.html)
	if frag.final {
		frame = append(frame, doneFrame...)
	}
	if err := writeEvent(w, rc, frame); err != nil {
		log.Info("event stream ended", "reason", "write failed", "error", err)
		return false
	}
	if frag.final {
		log.Info("event stream ended", "reason", "session completed")
		return false
	}
	return true
}

// logRenderEnd logs a mid-stream render failure, which ends the stream. A
// client that left (context.Canceled) or a session that was deleted
// (sessions.ErrNotFound) or a site that no longer has chargers rows
// (errUnknownSite) is a normal end, logged at Info; anything else is an Error.
func logRenderEnd(log *slog.Logger, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, sessions.ErrNotFound) || errors.Is(err, errUnknownSite) {
		log.Info("event stream ended", "reason", "render stopped", "error", err)
		return
	}
	log.Error("event stream failed", "op", "render", "error", err)
}

// writeEvent sets a fresh write deadline, writes frame, flushes it and then
// clears the deadline, so no stale deadline is left on the connection while
// the stream waits for the next event.
// http.ErrNotSupported from SetWriteDeadline is not an error (test fakes
// cannot set deadlines); any other failure is.
func writeEvent(w http.ResponseWriter, rc *http.ResponseController, frame []byte) error {
	if err := setWriteDeadline(rc, time.Now().Add(eventWriteTimeout)); err != nil {
		return err
	}
	if _, err := w.Write(frame); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := rc.Flush(); err != nil {
		return fmt.Errorf("flush: %w", err)
	}
	return setWriteDeadline(rc, time.Time{})
}

// setWriteDeadline sets (or, with the zero time, clears) the write deadline,
// tolerating a writer that does not support deadlines.
func setWriteDeadline(rc *http.ResponseController, t time.Time) error {
	if err := rc.SetWriteDeadline(t); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return fmt.Errorf("set write deadline: %w", err)
	}
	return nil
}

// eventFrame frames html as one server-sent event: "event: <name>", one
// "data: " line per line of html, and a blank line. CR and CRLF become LF
// first, since the SSE parser treats each of them as a line end.
func eventFrame(event string, html []byte) []byte {
	text := strings.ReplaceAll(string(html), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = strings.TrimSuffix(text, "\n")
	var b bytes.Buffer
	b.WriteString("event: " + event + "\n")
	for line := range strings.SplitSeq(text, "\n") {
		b.WriteString("data: " + line + "\n")
	}
	b.WriteString("\n")
	return b.Bytes()
}

// listFragment renders the list's live section for q, a first page.
func (p *Pages) listFragment(ctx context.Context, q listQuery) (fragment, error) {
	data, err := p.listData(ctx, q)
	if err != nil {
		return fragment{}, err
	}
	html, err := executeFragment("list", "list_body", data)
	return fragment{html: html}, err
}

// detailFragment renders a session's live section. It is final once the
// session is completed, since a completed session does not change again.
func (p *Pages) detailFragment(ctx context.Context, id string) (fragment, error) {
	data, err := p.detailData(ctx, id)
	if err != nil {
		return fragment{}, err
	}
	html, err := executeFragment("detail", "detail_live", data)
	return fragment{html: html, final: data.Status == sessions.StatusCompleted}, err
}

// executeFragment executes the fragment name from page's template set.
func executeFragment(page, name string, data any) ([]byte, error) {
	var buf bytes.Buffer
	if err := pageTemplates[page].ExecuteTemplate(&buf, name, data); err != nil {
		return nil, fmt.Errorf("render %s: %w", name, err)
	}
	return buf.Bytes(), nil
}
