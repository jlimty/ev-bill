// Package datamall fetches LTA DataMall's EV Charging Points Batch file and
// parses it. It is a leaf package: it knows nothing about billing sessions.
package datamall

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Status is a connector's availability.
type Status int

const (
	// Unavailable covers "" and every status the parser does not know.
	Unavailable Status = iota
	// Available is feed status "1".
	Available
	// Occupied is feed status "0". It covers OCPI CHARGING, RESERVED and
	// BLOCKED, so occupied does not always mean charging.
	Occupied
)

func (s Status) String() string {
	switch s {
	case Available:
		return "available"
	case Occupied:
		return "occupied"
	default:
		return "unavailable"
	}
}

func parseStatus(s string) Status {
	switch s {
	case "1":
		return Available
	case "0":
		return Occupied
	default:
		return Unavailable
	}
}

// Connector is one evId in the feed, with its location and plug fields.
type Connector struct {
	ChargerID, LocationName, Address, PostalCode, Operator, Position, PlugType string
	Current                                                                    string // "AC" or "DC"
	PowerW                                                                     int64
	Price, PriceType                                                           string
	Status                                                                     Status
}

// Snapshot is one parsed batch file.
type Snapshot struct {
	UpdatedAt  time.Time   // LastUpdatedTime, read in the caller's location
	Connectors []Connector // only the requested postal codes, sorted by ChargerID
	Skipped    []string    // charger IDs dropped for a bad power rating, sorted
}

const (
	timestampLayout = "2006-01-02 15:04:05"
	maxPowerW       = 1_000_000 // 1000 kW
)

// ParseKW converts a kW decimal string such as "7.4" to integer watts. It
// accepts digits with at most one point and at most 3 decimals, and rejects
// anything above 1000 kW. It uses no floating point (MIR-4).
func ParseKW(s string) (int64, error) {
	whole, frac, _ := strings.Cut(s, ".")
	hasPoint := strings.Contains(s, ".")
	if whole == "" || len(whole) > 4 || (hasPoint && (frac == "" || len(frac) > 3)) {
		return 0, fmt.Errorf("power rating %q is not a kW value with up to 3 decimals", s)
	}
	var w, f int64
	for _, r := range whole {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("power rating %q has a non-digit", s)
		}
		w = w*10 + int64(r-'0')
	}
	for _, r := range frac {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("power rating %q has a non-digit", s)
		}
		f = f*10 + int64(r-'0')
	}
	for i := len(frac); i < 3; i++ {
		f *= 10
	}
	watts := w*1000 + f
	if watts > maxPowerW {
		return 0, fmt.Errorf("power rating %q is above 1000 kW", s)
	}
	return watts, nil
}

type feedLocation struct {
	Address        string `json:"address"`
	Name           string `json:"name"`
	PostalCode     string `json:"postalCode"`
	ChargingPoints []struct {
		Operator  string `json:"operator"`
		Position  string `json:"position"`
		PlugTypes []struct {
			PlugType    string `json:"plugType"`
			Price       string `json:"price"`
			Current     string `json:"current"`
			PowerRating string `json:"powerRating"`
			PriceType   string `json:"priceType"`
			EvIDs       []struct {
				EvCpID string `json:"evCpId"`
				Status string `json:"status"`
			} `json:"evIds"`
		} `json:"plugTypes"`
	} `json:"chargingPoints"`
}

// Decode streams a batch file. It decodes evLocationsData one location at a
// time, so the whole tree is never in memory, and ignores unknown keys. Only
// connectors at the requested postal codes are returned (MIR-2). The
// timestamp is read in loc (MIR-3). A connector whose rating does not parse
// is left out and listed in Skipped (MIR-4).
func Decode(r io.Reader, loc *time.Location, postalCodes map[string]bool) (Snapshot, error) {
	var snap Snapshot
	if loc == nil {
		return snap, errors.New("datamall: nil location")
	}
	dec := json.NewDecoder(r)
	if err := expectDelim(dec, '{'); err != nil {
		return snap, fmt.Errorf("datamall: top level: %w", err)
	}
	var updated string
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return snap, fmt.Errorf("datamall: read key: %w", err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return snap, fmt.Errorf("datamall: unexpected token %v", keyTok)
		}
		switch key {
		case "LastUpdatedTime":
			if err := dec.Decode(&updated); err != nil {
				return snap, fmt.Errorf("datamall: LastUpdatedTime: %w", err)
			}
		case "evLocationsData":
			if err := decodeLocations(dec, postalCodes, &snap); err != nil {
				return snap, err
			}
		default:
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return snap, fmt.Errorf("datamall: skip %q: %w", key, err)
			}
		}
	}
	if err := expectDelim(dec, '}'); err != nil {
		return snap, fmt.Errorf("datamall: end of file: %w", err)
	}
	if updated == "" {
		return snap, errors.New("datamall: LastUpdatedTime is missing")
	}
	t, err := time.ParseInLocation(timestampLayout, updated, loc)
	if err != nil {
		return snap, fmt.Errorf("datamall: LastUpdatedTime: %w", err)
	}
	snap.UpdatedAt = t
	sort.Slice(snap.Connectors, func(i, j int) bool { return snap.Connectors[i].ChargerID < snap.Connectors[j].ChargerID })
	sort.Strings(snap.Skipped)
	return snap, nil
}

func expectDelim(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != want {
		return fmt.Errorf("expected %q, found %v", rune(want), tok)
	}
	return nil
}

func decodeLocations(dec *json.Decoder, postalCodes map[string]bool, snap *Snapshot) error {
	if err := expectDelim(dec, '['); err != nil {
		return fmt.Errorf("datamall: evLocationsData: %w", err)
	}
	for dec.More() {
		var loc feedLocation
		if err := dec.Decode(&loc); err != nil {
			return fmt.Errorf("datamall: location: %w", err)
		}
		if !postalCodes[loc.PostalCode] {
			continue
		}
		for _, cp := range loc.ChargingPoints {
			for _, pt := range cp.PlugTypes {
				watts, werr := ParseKW(pt.PowerRating)
				for _, id := range pt.EvIDs {
					if werr != nil {
						snap.Skipped = append(snap.Skipped, id.EvCpID)
						continue
					}
					snap.Connectors = append(snap.Connectors, Connector{
						ChargerID: id.EvCpID, LocationName: loc.Name, Address: loc.Address,
						PostalCode: loc.PostalCode, Operator: cp.Operator, Position: cp.Position,
						PlugType: pt.PlugType, Current: pt.Current, PowerW: watts,
						Price: pt.Price, PriceType: pt.PriceType, Status: parseStatus(id.Status),
					})
				}
			}
		}
	}
	if err := expectDelim(dec, ']'); err != nil {
		return fmt.Errorf("datamall: evLocationsData: %w", err)
	}
	return nil
}
