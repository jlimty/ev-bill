package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
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
	renderTimeout = 8 * time.Second
)

// fragmentFunc renders the HTML fragment one stream sends.
type fragmentFunc func(context.Context) ([]byte, error)

// RegisterEvents mounts GET /events on mux (LIVE-1). The caller mounts it
// outside the per-request deadline, which would otherwise end the stream.
func (p *Pages) RegisterEvents(mux *http.ServeMux) {
	mux.HandleFunc("GET /events", p.events)
}

// events serves GET /events?view=list and GET /events?view=session&id=<uuid>
// as a server-sent event stream: the current fragment straight away, then a
// re-rendered one after each hub signal, and a heartbeat when idle (LIVE-1).
func (p *Pages) events(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	if p.Hub == nil {
		http.Error(w, "Live updates are unavailable.", http.StatusServiceUnavailable)
		return
	}

	q := r.URL.Query()
	view := q.Get("view")
	var render fragmentFunc
	switch view {
	case "list":
		render = p.listFragment
	case "session":
		id := q.Get("id")
		if id == "" {
			http.Error(w, "The session view needs an id.", http.StatusBadRequest)
			return
		}
		render = func(ctx context.Context) ([]byte, error) { return p.detailFragment(ctx, id) }
	default:
		http.Error(w, "Unknown view.", http.StatusBadRequest)
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

	first, err := renderWithin(r.Context(), render)
	if errors.Is(err, sessions.ErrNotFound) {
		http.Error(w, "Session not found.", http.StatusNotFound)
		return
	}
	if err != nil {
		reqid.Logger(r.Context(), p.Logger).Error("event stream failed", "op", "first render", "error", err)
		http.Error(w, "Something went wrong.", http.StatusInternalServerError)
		return
	}
	p.stream(w, r, view, sub, first, render)
}

// stream writes first as an event, then one event per signal on sub and a
// heartbeat after each idle heartbeatEvery. It returns when sub is closed (the
// hub stopped, which is what lets http.Server.Shutdown finish, since Shutdown
// does not cancel request contexts), when the request context is done, or
// when an event cannot be rendered or delivered.
func (p *Pages) stream(w http.ResponseWriter, r *http.Request, event string, sub <-chan struct{}, first []byte, render fragmentFunc) {
	log := reqid.Logger(r.Context(), p.Logger)
	rc := http.NewResponseController(w)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // ask proxies not to buffer the stream
	w.WriteHeader(http.StatusOK)

	if err := writeEvent(w, rc, eventFrame(event, first)); err != nil {
		log.Info("event stream ended", "reason", "write failed", "error", err)
		return
	}
	heartbeat := time.NewTicker(heartbeatEvery)
	defer heartbeat.Stop()
	for {
		var frame []byte
		select {
		case <-r.Context().Done():
			return
		case _, open := <-sub:
			if !open {
				return
			}
			html, err := renderWithin(r.Context(), render)
			if err != nil {
				log.Error("event stream failed", "op", "render", "error", err)
				return
			}
			frame = eventFrame(event, html)
		case <-heartbeat.C:
			frame = []byte(": ping\n\n")
		}
		if err := writeEvent(w, rc, frame); err != nil {
			log.Info("event stream ended", "reason", "write failed", "error", err)
			return
		}
	}
}

// writeEvent sets a fresh write deadline, writes frame and flushes it.
// http.ErrNotSupported from SetWriteDeadline is not an error (test fakes
// cannot set deadlines); any other failure is.
func writeEvent(w http.ResponseWriter, rc *http.ResponseController, frame []byte) error {
	if err := rc.SetWriteDeadline(time.Now().Add(eventWriteTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return fmt.Errorf("set write deadline: %w", err)
	}
	if _, err := w.Write(frame); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := rc.Flush(); err != nil {
		return fmt.Errorf("flush: %w", err)
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

func renderWithin(ctx context.Context, render fragmentFunc) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, renderTimeout)
	defer cancel()
	return render(ctx)
}

func (p *Pages) listFragment(ctx context.Context) ([]byte, error) {
	data, err := p.listData(ctx)
	if err != nil {
		return nil, err
	}
	return executeFragment("list", "list_body", data)
}

func (p *Pages) detailFragment(ctx context.Context, id string) ([]byte, error) {
	data, err := p.detailData(ctx, id)
	if err != nil {
		return nil, err
	}
	return executeFragment("detail", "detail_live", data)
}

// executeFragment executes the fragment name from page's template set.
func executeFragment(page, name string, data any) ([]byte, error) {
	var buf bytes.Buffer
	if err := pageTemplates[page].ExecuteTemplate(&buf, name, data); err != nil {
		return nil, fmt.Errorf("render %s: %w", name, err)
	}
	return buf.Bytes(), nil
}
