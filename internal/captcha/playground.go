package captcha

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Playground selection: sliding-window champion.
//
// NVIDIA retires playground models often; a retired model's page stops minting
// tokens, so the mint page must be chosen from live data, not hardcoded. On
// every -auto start with no pinned -captcha-playground the server benches a
// small window of catalog pages on one throwaway Chrome and keeps the fastest:
//
//	no state      -> bench the first playgroundWindow alive candidates
//	state present -> {champion} ∪ never-benched (else least-recently-benched)
//	                 up to the window, then keep the fastest alive
//	catalog swept -> LRU re-bench cycles back through the whole catalog
//
// Each candidate is probed Rounds times (median kept). Dead candidates (nav
// error, 404, missing widget) drop out untimed and the champion becomes the
// fastest survivor. The result persists to ~/.nvpi/playground-state.json, so a
// retirement swaps champions on the next start without a rebuild.

const (
	playgroundWindow = 5 // candidates benched per start
	defaultRounds    = 2 // timed probes per candidate; median wins
)

// BenchedEntry is one candidate's latest measurement.
type BenchedEntry struct {
	MedianMS float64   `json:"median_ms"`
	At       time.Time `json:"at"`
}

// PlaygroundState is the persisted selection memory. Keys are registry model
// ids ("org/model"); Champion names the current mint page.
type PlaygroundState struct {
	Champion string                  `json:"champion"`
	Benched  map[string]BenchedEntry `json:"benched"`
}

// DefaultStatePath is ~/.nvpi/playground-state.json, or "" when the home dir
// cannot be resolved (caller then runs selection without persistence).
func DefaultStatePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".nvpi", "playground-state.json")
}

// LoadState reads the state file. A missing or corrupt file yields an empty
// state — selection restarts from scratch, never fatal.
func LoadState(path string) PlaygroundState {
	if path == "" {
		return PlaygroundState{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return PlaygroundState{}
	}
	var st PlaygroundState
	if json.Unmarshal(data, &st) != nil || st.Benched == nil {
		return PlaygroundState{}
	}
	return st
}

// SaveState writes atomically (temp file + rename) so a crash mid-write cannot
// leave a half-file behind for the next start to choke on.
func SaveState(path string, st PlaygroundState) error {
	if path == "" {
		return fmt.Errorf("no playground state path")
	}
	if st.Benched == nil {
		st.Benched = map[string]BenchedEntry{}
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// nextWindow picks the round's bench list. Fresh install: the first
// playgroundWindow candidates in order. Otherwise the champion leads, the
// window fills with never-benched candidates in catalog order, and any
// remaining slots go to the least-recently-benched ones. Pure.
func nextWindow(st PlaygroundState, candidates []string) []string {
	win := make([]string, 0, playgroundWindow)
	seen := make(map[string]bool, len(candidates))
	add := func(id string) {
		if id == "" || seen[id] || len(win) >= playgroundWindow {
			return
		}
		seen[id] = true
		win = append(win, id)
	}

	fresh := st.Champion == "" && len(st.Benched) == 0
	if !fresh {
		// Champion leads — but only if it survived into the current catalog.
		for _, c := range candidates {
			if c == st.Champion {
				add(c)
				break
			}
		}
	}
	for _, c := range candidates {
		if len(win) >= playgroundWindow {
			return win
		}
		if !fresh {
			if _, ok := st.Benched[c]; ok {
				continue
			}
		}
		add(c)
	}
	if fresh {
		return win
	}
	// Catalog fully benched before the window filled: cycle oldest-first.
	type kv struct {
		id string
		at time.Time
	}
	var old []kv
	for _, c := range candidates {
		if e, ok := st.Benched[c]; ok {
			old = append(old, kv{c, e.At})
		}
	}
	sort.Slice(old, func(i, j int) bool { return old[i].at.Before(old[j].at) })
	for _, e := range old {
		if len(win) >= playgroundWindow {
			break
		}
		add(e.id)
	}
	return win
}

// medianMS is the upper-middle of the sorted samples — same convention as the
// bench harness' medianOf.
func medianMS(ds []float64) float64 {
	if len(ds) == 0 {
		return 0
	}
	s := append([]float64(nil), ds...)
	sort.Float64s(s)
	return s[len(s)/2]
}

// PlaygroundSelector ranks candidate model ids by playground warm-navigate
// time. Probe is injected so selection logic is testable without Chrome (the
// real implementation re-navigates one shared sticky tab per candidate).
type PlaygroundSelector struct {
	// Probe times one cold navigation to the candidate's playground page.
	// An error marks the candidate dead for this round (no further probes).
	Probe func(ctx context.Context, modelID string) (time.Duration, error)
	// Rounds is the number of successful probes per candidate (default 2);
	// the median is kept.
	Rounds int
	// Budget caps total wall clock across all probes (0 = unlimited). When it
	// runs out, the decision falls back to whatever was already measured.
	Budget time.Duration
	// Now overrides the clock (tests).
	Now func() time.Time
}

func (s *PlaygroundSelector) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Select benches the sliding window over candidates and returns the winning
// model id plus the updated state (the caller persists it). If every windowed
// candidate fails its probes, it returns an error and the caller should fall
// back to its own first candidate.
func (s *PlaygroundSelector) Select(ctx context.Context, st PlaygroundState, candidates []string) (string, PlaygroundState, error) {
	if s.Probe == nil {
		return "", st, fmt.Errorf("playground selector: no probe")
	}
	rounds := s.Rounds
	if rounds <= 0 {
		rounds = defaultRounds
	}
	// Clone so an error return never leaks mutations into the caller's map.
	st.Benched = cloneBenched(st.Benched)

	start := s.now()
	best, bestMS := "", 0.0
	budgetOut := false
	win := nextWindow(st, candidates)
	inWin := make(map[string]bool, len(win))
	for _, id := range win {
		inWin[id] = true
	}
	for _, id := range win {
		var times []float64
		for len(times) < rounds && ctx.Err() == nil {
			if s.Budget > 0 && s.now().Sub(start) >= s.Budget {
				budgetOut = true
				break
			}
			d, err := s.Probe(ctx, id)
			if err != nil {
				// Dead candidate: excluded untimed, remaining rounds skipped.
				log.Printf("captcha playground: %s dead (%v)", id, err)
				break
			}
			times = append(times, d.Seconds()*1000)
		}
		if len(times) > 0 {
			ms := medianMS(times)
			st.Benched[id] = BenchedEntry{MedianMS: ms, At: s.now()}
			if best == "" || ms < bestMS {
				best, bestMS = id, ms
			}
		}
		if budgetOut {
			break
		}
	}
	if best == "" {
		// Whole window dead — retirements cluster at the alphabetically-old
		// catalog head, so this is the common cold-start shape, not an edge.
		// Sweep the rest of the catalog until one page proves alive; it wins
		// on a single sample (ponytail: no median for rescue picks).
		for _, id := range candidates {
			if inWin[id] || ctx.Err() != nil {
				continue
			}
			if s.Budget > 0 && s.now().Sub(start) >= s.Budget {
				break
			}
			d, err := s.Probe(ctx, id)
			if err != nil {
				log.Printf("captcha playground: %s dead (%v)", id, err)
				continue
			}
			best, bestMS = id, d.Seconds()*1000
			st.Benched[id] = BenchedEntry{MedianMS: bestMS, At: s.now()}
			break
		}
	}
	if best == "" {
		return "", st, fmt.Errorf("playground selector: every candidate failed probing")
	}
	st.Champion = best
	return best, st, nil
}

func cloneBenched(m map[string]BenchedEntry) map[string]BenchedEntry {
	out := make(map[string]BenchedEntry, len(m)+playgroundWindow)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// PlaygroundURL maps a registry model id ("org/model") to its NVIDIA
// playground page — the URL hCaptcha tokens are minted on.
func PlaygroundURL(modelID string) string {
	org, slug, _ := strings.Cut(modelID, "/")
	return fmt.Sprintf("https://build.nvidia.com/%s/%s/playground", org, slug)
}
