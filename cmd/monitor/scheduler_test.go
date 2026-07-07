package main

import (
	"testing"
	"time"
)

func mustLoad(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatalf("LoadLocation: %v (time/tzdata must be embedded)", err)
	}
	return loc
}

// The next fire is always the wall-clock HH:MM, strictly in the future, in loc.
func TestNextOccurrence_BasicAndBoundary(t *testing.T) {
	loc := mustLoad(t)
	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{
			name: "time still ahead today",
			now:  time.Date(2026, 7, 7, 19, 0, 0, 0, loc),
			want: time.Date(2026, 7, 7, 20, 0, 0, 0, loc),
		},
		{
			name: "time already passed -> tomorrow",
			now:  time.Date(2026, 7, 7, 20, 30, 0, 0, loc),
			want: time.Date(2026, 7, 8, 20, 0, 0, 0, loc),
		},
		{
			name: "exactly at the minute -> tomorrow (strictly after)",
			now:  time.Date(2026, 7, 7, 20, 0, 0, 0, loc),
			want: time.Date(2026, 7, 8, 20, 0, 0, 0, loc),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := nextOccurrence(c.now, 20, 0, loc)
			if !got.Equal(c.want) {
				t.Fatalf("nextOccurrence(%s) = %s, want %s", c.now, got, c.want)
			}
			if !got.After(c.now) {
				t.Fatalf("next fire %s is not strictly after now %s", got, c.now)
			}
			if got.Hour() != 20 || got.Minute() != 0 {
				t.Fatalf("next fire wall-clock = %02d:%02d, want 20:00", got.Hour(), got.Minute())
			}
		})
	}
}

// The recompute-in-location approach must keep the wall-clock hour fixed across
// DST transitions — the real-elapsed gap is 23h (spring forward) or 25h (fall
// back), never a drifted 19:00/21:00. A now.Add(24h) implementation would fail
// these (it would land an hour off), which is the whole point of the design.
func TestNextOccurrence_DSTKeepsWallClock(t *testing.T) {
	loc := mustLoad(t)
	cases := []struct {
		name    string
		now     time.Time
		wantDur time.Duration // real elapsed time until the next 20:00
	}{
		{
			// Sweden springs forward 2026-03-29 (02:00 -> 03:00): a lost hour.
			name:    "spring forward -> 23h gap",
			now:     time.Date(2026, 3, 28, 20, 0, 0, 0, loc),
			wantDur: 23 * time.Hour,
		},
		{
			// Sweden falls back 2026-10-25 (03:00 -> 02:00): a repeated hour.
			name:    "fall back -> 25h gap",
			now:     time.Date(2026, 10, 24, 20, 0, 0, 0, loc),
			wantDur: 25 * time.Hour,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := nextOccurrence(c.now, 20, 0, loc)
			if got.Hour() != 20 || got.Minute() != 0 {
				t.Fatalf("wall-clock drifted to %02d:%02d across DST, want 20:00", got.Hour(), got.Minute())
			}
			if d := got.Sub(c.now); d != c.wantDur {
				t.Fatalf("elapsed to next fire = %s, want %s (DST not handled)", d, c.wantDur)
			}
		})
	}
}
