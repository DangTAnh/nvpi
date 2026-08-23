package captcha

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// stepClock returns a Now func advancing by step per call, starting at base.
func stepClock(base time.Time, step time.Duration) func() time.Time {
	cur := base
	return func() time.Time {
		t := cur
		cur = cur.Add(step)
		return t
	}
}

func TestNextWindowFreshTakesFirstFive(t *testing.T) {
	cands := []string{"a", "b", "c", "d", "e", "f"}
	win := nextWindow(PlaygroundState{}, cands)
	if fmt.Sprint(win) != "[a b c d e]" {
		t.Fatalf("fresh window = %v, want first five", win)
	}
}

func TestNextWindowChampionFirstThenUnbenched(t *testing.T) {
	st := PlaygroundState{
		Champion: "y",
		Benched: map[string]BenchedEntry{
			"y": {MedianMS: 100, At: time.Now()},
			"z": {MedianMS: 200, At: time.Now()},
		},
	}
	cands := []string{"a", "z", "b", "y", "c", "d", "e"}
	win := nextWindow(st, cands)
	// champion leads; benched z skipped while unbenched a,b,c,d fill the rest.
	if fmt.Sprint(win) != "[y a b c d]" {
		t.Fatalf("window = %v, want [y a b c d]", win)
	}
}

func TestNextWindowLRURefillWhenCatalogSwept(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	st := PlaygroundState{Champion: "a"}
	st.Benched = map[string]BenchedEntry{}
	for i, id := range []string{"a", "b", "c", "d", "e", "f"} {
		st.Benched[id] = BenchedEntry{At: now.Add(time.Duration(i) * time.Hour)}
	}
	cands := []string{"a", "b", "c", "d", "e", "f"}
	win := nextWindow(st, cands)
	// Champion stays; remaining four slots go to the oldest-benched (b..e).
	if fmt.Sprint(win) != "[a b c d e]" {
		t.Fatalf("window = %v, want champion + LRU refill [b c d e]", win)
	}
}

func TestNextWindowChampionNotInCandidatesDropped(t *testing.T) {
	st := PlaygroundState{
		Champion: "retired",
		Benched:  map[string]BenchedEntry{"retired": {At: time.Now()}},
	}
	win := nextWindow(st, []string{"a", "b", "c"})
	if fmt.Sprint(win) != "[a b c]" {
		t.Fatalf("window = %v, want unbenched candidates only", win)
	}
}

// fakeProbe serves canned durations (consumed in order per id; the last one
// repeats) or errors, and counts probes per id.
type fakeProbe struct {
	dur   map[string][]time.Duration
	err   map[string]error
	calls map[string]int
}

func (f *fakeProbe) probe(_ context.Context, id string) (time.Duration, error) {
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[id]++
	if err, ok := f.err[id]; ok {
		return 0, err
	}
	ds := f.dur[id]
	if len(ds) == 0 {
		return 50 * time.Millisecond, nil
	}
	i := f.calls[id] - 1
	if i >= len(ds) {
		i = len(ds) - 1
	}
	return ds[i], nil
}

func TestSelectDeadChampionReplacedByFastestAlive(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clockStep := time.Second
	st := PlaygroundState{
		Champion: "dead",
		Benched: map[string]BenchedEntry{
			"dead": {MedianMS: 10, At: base},
		},
	}
	p := &fakeProbe{
		dur: map[string][]time.Duration{
			"slow": {300 * time.Millisecond},
			"fast": {100 * time.Millisecond},
		},
		err: map[string]error{"dead": errors.New("nav failed")},
	}
	sel := &PlaygroundSelector{Probe: p.probe, Rounds: 2, Now: stepClock(base, clockStep)}
	winner, st2, err := sel.Select(context.Background(), st, []string{"dead", "slow", "fast"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if winner != "fast" {
		t.Fatalf("winner = %s, want fast", winner)
	}
	if st2.Champion != "fast" {
		t.Fatalf("state champion = %s, want fast", st2.Champion)
	}
	if e := st2.Benched["fast"]; e.MedianMS != 100 || !e.At.After(base) {
		t.Fatalf("fast entry = %+v, want median 100 fresh at", e)
	}
	// Dead candidate must gain no fresh measurement.
	if _, still := st2.Benched["dead"]; !still {
		t.Fatal("old dead entry should be preserved untouched")
	}
}

func TestSelectRunsRoundsAndKeepsMedian(t *testing.T) {
	base := time.Unix(0, 0)
	// Two probes for "m": 400ms then 200ms → upper-middle of sorted = 400.
	p := &fakeProbe{dur: map[string][]time.Duration{"m": {400 * time.Millisecond, 200 * time.Millisecond}}}
	sel := &PlaygroundSelector{Probe: p.probe, Rounds: 2, Now: stepClock(base, time.Second)}
	_, st, err := sel.Select(context.Background(), PlaygroundState{}, []string{"m"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if p.calls["m"] != 2 {
		t.Fatalf("probes = %d, want 2 rounds", p.calls["m"])
	}
	if ms := st.Benched["m"].MedianMS; ms != 400 {
		t.Fatalf("median = %.0f, want 400 (upper middle)", ms)
	}
}

func TestSelectAllDeadFallsBackToError(t *testing.T) {
	p := &fakeProbe{err: map[string]error{"x": errors.New("404")}}
	sel := &PlaygroundSelector{Probe: p.probe, Rounds: 1, Now: stepClock(time.Unix(0, 0), time.Second)}
	_, st, err := sel.Select(context.Background(), PlaygroundState{}, []string{"x"})
	if err == nil {
		t.Fatal("want error when every candidate is dead")
	}
	if st.Champion != "" {
		t.Fatalf("champion should stay empty, got %s", st.Champion)
	}
}

func TestSelectBudgetCutoffDecidesFromMeasured(t *testing.T) {
	base := time.Unix(0, 0)
	p := &fakeProbe{dur: map[string][]time.Duration{
		"a": {100 * time.Millisecond},
		"b": {50 * time.Millisecond},
	}}
	sel := &PlaygroundSelector{Probe: p.probe, Rounds: 1, Budget: 25 * time.Second, Now: stepClock(base, 10*time.Second)}
	winner, st, err := sel.Select(context.Background(), PlaygroundState{}, []string{"a", "b"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	// Each probe advances the fake clock 10s; after two probes (20s) the third
	// check sees 30s >= 25s budget and stops. Only "a" measured → it wins even
	// though unseen "b" would have been faster.
	if winner != "a" {
		t.Fatalf("winner = %s, want a (budget cut before b)", winner)
	}
	if len(st.Benched) != 1 {
		t.Fatalf("benched = %d entries, want only measured a", len(st.Benched))
	}
}

func TestSelectSweepsCatalogWhenWindowDead(t *testing.T) {
	base := time.Unix(0, 0)
	// Alphabetical head (a..e) is fully retired; first alive is "h".
	p := &fakeProbe{err: map[string]error{
		"a": errors.New("404"), "b": errors.New("timeout"), "c": errors.New("404"),
		"d": errors.New("timeout"), "e": errors.New("404"),
		"f": errors.New("404"), "g": errors.New("timeout"),
	}, dur: map[string][]time.Duration{"h": {120 * time.Millisecond}}}
	sel := &PlaygroundSelector{Probe: p.probe, Rounds: 2, Now: stepClock(base, time.Second)}
	winner, st, err := sel.Select(context.Background(), PlaygroundState{}, []string{"a", "b", "c", "d", "e", "f", "g", "h"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if winner != "h" {
		t.Fatalf("winner = %s, want h (first alive past dead window)", winner)
	}
	if st.Champion != "h" {
		t.Fatalf("champion = %s, want h", st.Champion)
	}
	// Rescue pick stops at the first alive page: f and g probed once each
	// (dead), h probed once (alive), i never reached.
	if p.calls["i"] != 0 {
		t.Fatalf("rescue sweep should stop at first alive, probed i %d times", p.calls["i"])
	}
}

func TestStateRoundtripAndCorruptSelfHeal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	// Missing file → empty state, no error.
	if st := LoadState(path); st.Champion != "" || len(st.Benched) != 0 {
		t.Fatalf("missing file state = %+v, want zero", st)
	}

	st := PlaygroundState{
		Champion: "org/model",
		Benched:  map[string]BenchedEntry{"org/model": {MedianMS: 123.5, At: time.Now().UTC().Truncate(time.Second)}},
	}
	if err := SaveState(path, st); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	got := LoadState(path)
	if got.Champion != "org/model" || got.Benched["org/model"] != st.Benched["org/model"] {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}

	// Corrupt file → discarded, never fatal.
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	if st := LoadState(path); st.Champion != "" || len(st.Benched) != 0 {
		t.Fatalf("corrupt state = %+v, want zero (self-heal)", st)
	}
}

func TestPlaygroundURLShape(t *testing.T) {
	got := PlaygroundURL("minimaxai/minimax-m3")
	if got != "https://build.nvidia.com/minimaxai/minimax-m3/playground" {
		t.Fatalf("url = %s", got)
	}
}
