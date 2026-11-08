package sessions

import (
	"fmt"
	"time"

	"github.com/jlimty/ev-bill/internal/billing"
)

// View is a session with its derived status and bill.
type View struct {
	Session       Session
	Status        string
	LastReadingAt time.Time
	Bill          billing.Bill
}

// BuildView derives the status (SES-4) and prices the session (BIL-7). An
// active session is billed up to its latest reading; a stopped one includes
// its stop point.
func BuildView(s Session, readings []billing.Reading, t billing.Tariff, now time.Time) (View, error) {
	timeline := Timeline(s, readings)
	last := timeline[len(timeline)-1].At

	status := StatusCharging
	switch {
	case isStopped(s):
		status = StatusCompleted
	case now.Sub(last) > StaleAfter:
		status = StatusStale
	}

	bill, err := billing.Compute(t, timeline)
	if err != nil {
		return View{}, fmt.Errorf("compute bill for session %s: %w", s.ID, err)
	}
	return View{Session: s, Status: status, LastReadingAt: last, Bill: bill}, nil
}
