package mirror

import "time"

// DefaultPostalCodes are watched when MIRROR_POSTAL_CODES is unset (MIR-2):
// GV Suntec City (AC and DC, a mall), Pasir Ris Mall (DC) and the Blk 29/32
// Telok Blangah Rise HDB car park (AC), 68 connectors in the 2026-11-08 sample.
var DefaultPostalCodes = []string{"038983", "519612", "090032"}

const (
	// PollInterval is the time between polls; one also runs at startup (MIR-1).
	PollInterval = 5 * time.Minute
	// GapAfter is how long a connector may go unobserved before its session is
	// closed at its last point: two intervals plus a minute (MIR-5).
	GapAfter = 2*PollInterval + time.Minute
	// RetentionEvery is how often completed mirror sessions are pruned (MIR-9).
	RetentionEvery = time.Hour
	// RetentionKeep is how long completed mirror sessions are kept (MIR-9).
	RetentionKeep = 14 * 24 * time.Hour
)
