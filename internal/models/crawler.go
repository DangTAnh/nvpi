package models

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Upstream catalog + queue endpoints used to refresh the registry at startup.
// Both are anonymous GETs (no auth header) — the catalog is the public NIM
// index, the queue endpoint is the same anonymous probe as the playground page
// inlines for `nvcfFunctionId`.
//
//	GET https://integrate.api.nvidia.com/v1/models
//		→ {"object":"list","data":[{"id":"<publisher>/<slug>", ...}]}
//
//	GET https://buildapi.ngc.nvidia.com/v2/predict/queues/models/qc69jvmznzxy/<slug>
//		→ {"functionId":"<uuid>", "queues":[...]} on success
//		→ 404 / non-JSON on retired models (skip silently)
//
// Text filter (2026-08-24): a model enters the refreshed registry only when
// its /playground page exists:
//	GET https://build.nvidia.com/<publisher>/<slug>/playground
// build.nvidia.com answers HTTP 200 for every such path (the Next.js shell is
// flushed before the route resolves), so existence is read from the RSC
// payload: a missing playground inlines the error digest
// NEXT_HTTP_ERROR_FALLBACK;404 — verified live 2026-08-24 (deepseek-r1 and
// llama-3_3-70b-instruct lack the digest; bge-m3, riva-asr and bogus slugs
// carry it). The modelCapability JSON literal this filter used before is no
// longer inlined into the SSR HTML at all.
//
// ponytail: package-private constants + 3 fetch helpers + bounded concurrent
// probe. If the catalog call fails we keep the hardcoded registry — refresh
// is best-effort, the offline fallback already covers all known-good models.

const (
	catalogURL       = "https://integrate.api.nvidia.com/v1/models"
	queueURLBase     = "https://buildapi.ngc.nvidia.com/v2/predict/queues/models/" + Namespace + "/"
	playgroundURLFmt = "https://build.nvidia.com/%s/%s/playground"
)

// RefreshOptions tunes the crawler. Zero values pick conservative defaults.
type RefreshOptions struct {
	HTTPClient  *http.Client
	Timeout     time.Duration
	Concurrency int
}

// RefreshResult summarises what the crawler observed.
type RefreshResult struct {
	Listed   int // catalog entries returned
	Probed   int // entries actually hit by the queue probe
	OK       int // entries that returned a functionId
	Skipped  int // probe failures (retired, timeout, decode)
	WithCaps int // entries whose /playground page exists (text models)
	Cached   int // WithCaps entries served from the known-text cache, unprobed
	Duration time.Duration
}

// Refresh re-fetches the registry from upstream and merges it into Models via
// mergeRegistry — fresh probe data where it exists, carried-over entries where
// the queue probe transiently failed. On total failure (catalog unreachable OR
// no model returned a functionId), the hardcoded Models stays in place — caller
// does not need to handle that case specially; the result still reports what
// happened.
//
// ponytail: single-writer at startup, so no mutex — pointer assignment of the
// map header is atomic on every arch Go targets. Hot readers call Lookup(),
// which sees the new map on the next call.
func Refresh(ctx context.Context, opts RefreshOptions) (RefreshResult, error) {
	if opts.Timeout <= 0 {
		// Ceiling only — Refresh returns as soon as every probe finishes. The
		// two-fetch pipeline (~100 catalog entries × queue + playground page)
		// needs minutes under CDN throttling; a short ceiling silently drops
		// every model it didn't reach.
		opts.Timeout = 5 * time.Minute
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 8
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}

	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	t0 := time.Now()
	ids, err := fetchCatalog(ctx, opts.HTTPClient)
	if err != nil {
		return RefreshResult{}, fmt.Errorf("catalog: %w", err)
	}
	res := RefreshResult{Listed: len(ids)}

	knownPath := KnownTextPath()
	known := loadKnownText(knownPath)
	discovered := probeQueues(ctx, opts.HTTPClient, ids, opts.Concurrency, &res, known)

	if res.OK > 0 {
		Models = mergeRegistry(discovered, ids, Models)
	}

	// Persist the confirmed-text set. Entries absent from this catalog round
	// are pruned — same retirement rule as mergeRegistry — so the file never
	// grows stale retirees.
	kept := make([]string, 0, len(discovered))
	for _, id := range ids {
		if _, ok := discovered[id]; ok || known[id] {
			kept = append(kept, id)
		}
	}
	saveKnownText(knownPath, kept)

	res.Duration = time.Since(t0)
	if res.OK == 0 {
		return res, fmt.Errorf("no models returned a functionId (catalog reachable=%d, probed=%d)", len(ids), res.Probed)
	}
	return res, nil
}

// mergeRegistry folds one refresh round into the registry. Three buckets:
//
//   - catalog-listed AND probed OK → fresh data (discovered wins);
//   - catalog-listed but probe failed → carry over current when a hardcoded
//     entry exists. A failed probe can be a network blip or timeout, not a
//     retirement; replacing wholesale used to make such a model silently
//     vanish from Lookup until restart. No hardcoded entry → skip as before;
//   - absent from the catalog list → dropped (genuinely retired upstream).
func mergeRegistry(discovered map[string]ModelInfo, catalogIDs []string, current map[string]ModelInfo) map[string]ModelInfo {
	out := make(map[string]ModelInfo, len(discovered)+len(catalogIDs))
	for id, info := range discovered {
		out[id] = info
	}
	for _, id := range catalogIDs {
		if _, ok := discovered[id]; ok {
			continue
		}
		if cur, ok := current[id]; ok {
			out[id] = cur
		}
	}
	return out
}

// fetchCatalog returns the list of "<publisher>/<slug>" ids from the catalog.
func fetchCatalog(ctx context.Context, hc *http.Client) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, catalogURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("catalog status=%d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var entries []struct {
		ID string `json:"id"`
	}
	// Catalog wraps the list as {"object":"list","data":[...]}.
	if err := json.Unmarshal(body, &struct {
		Data *[]struct {
			ID string `json:"id"`
		} `json:"data"`
	}{Data: &entries}); err != nil {
		if err2 := json.Unmarshal(body, &entries); err2 != nil {
			return nil, fmt.Errorf("catalog decode: %w", err)
		}
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if pub, slug, ok := splitPublisherSlug(e.ID); ok && pub != "" && slug != "" {
			out = append(out, e.ID)
		}
	}
	return out, nil
}

// probeQueues fans out to the queue endpoint with bounded concurrency.
// Returns the populated map (only successful models — no functionId entries).
// known carries the persisted known-text cache: an id present there skips the
// /playground fetch entirely and is recorded as kept.
func probeQueues(ctx context.Context, hc *http.Client, ids []string, concurrency int, res *RefreshResult, known map[string]bool) map[string]ModelInfo {
	out := make(map[string]ModelInfo, len(ids))
	var mu sync.Mutex
	var wg sync.WaitGroup

	sem := make(chan struct{}, concurrency)
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				mu.Lock()
				res.Skipped++
				mu.Unlock()
				return
			}

			fnID, ok := probeOne(ctx, hc, id)
			mu.Lock()
			res.Probed++
			if !ok {
				res.Skipped++
				mu.Unlock()
				return
			}
			mu.Unlock()

			// Playground existence probe is best-effort and runs after the
			// queue check so a retired model is dropped before we waste a
			// page fetch. A cached id skips it entirely.
			exists := known[id]
			if !exists {
				exists = probePlayground(ctx, hc, id)
			} else {
				mu.Lock()
				res.Cached++
				mu.Unlock()
			}

			if !exists {
				// No /playground page — not a text model (embedding, ASR,
				// retrieval…). Keep it out.
				mu.Lock()
				res.Skipped++
				mu.Unlock()
				return
			}

			mu.Lock()
			res.WithCaps++
			out[id] = ModelInfo{
				Slug:       slugOf(id),
				Namespace:  Namespace,
				FunctionID: fnID,
			}
			res.OK++
			mu.Unlock()
		}(id)
	}
	wg.Wait()
	return out
}

// probeOne issues a single queue GET. ok=false means "retired or transient error".
func probeOne(ctx context.Context, hc *http.Client, id string) (string, bool) {
	_, slug, ok := splitPublisherSlug(id)
	if !ok {
		return "", false
	}
	url := queueURLBase + slug
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", false
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", false
	}
	var payload struct {
		FunctionID string `json:"functionId"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		log.Printf("models: probe %s: decode: %v", id, err)
		return "", false
	}
	if payload.FunctionID == "" {
		return "", false
	}
	return payload.FunctionID, true
}

func splitPublisherSlug(id string) (pub, slug string, ok bool) {
	for i := 0; i < len(id); i++ {
		if id[i] == '/' {
			return id[:i], id[i+1:], true
		}
	}
	return "", "", false
}

func slugOf(id string) string {
	_, slug, ok := splitPublisherSlug(id)
	if !ok {
		return id
	}
	return slug
}

// notFoundMarker is the Next.js error digest streamed into the RSC payload
// when a /playground route does not exist. The HTTP status is still 200 (the
// shell flushes before the route resolves), so the body is the only reliable
// existence signal.
var notFoundMarker = []byte("NEXT_HTTP_ERROR_FALLBACK;404")

// playgroundExists reports whether a fetched /playground body is a real
// playground page rather than the soft-404 shell.
func playgroundExists(body []byte) bool {
	return !bytes.Contains(body, notFoundMarker)
}

// probePlayground reports whether https://build.nvidia.com/<id>/playground
// exists. false means "no proof of text": non-text model or transient fetch
// failure — such models must not enter the discovered map. Hardcoded entries
// survive via the mergeRegistry carry-over; brand-new ones just wait for the
// next refresh.
//
// The site rate-limits bursts of page fetches (observed 2026-08-24: throttled
// requests come back as dropped connections or as a soft-404 shell), so a miss
// is retried once after a short backoff before the model is dropped.
func probePlayground(ctx context.Context, hc *http.Client, id string) bool {
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(750 * time.Millisecond):
			case <-ctx.Done():
				return false
			}
		}
		if fetchPlaygroundExists(ctx, hc, id) {
			return true
		}
	}
	return false
}

func fetchPlaygroundExists(ctx context.Context, hc *http.Client, id string) bool {
	pub, slug, ok := splitPublisherSlug(id)
	if !ok {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(playgroundURLFmt, pub, slug), nil)
	if err != nil {
		return false
	}
	resp, err := hc.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false
	}
	return playgroundExists(body)
}

// KnownTextPath returns ~/.nvpi/known_text_models.json ("" when HOME is unset)
// — the persisted set of ids whose /playground page has been confirmed to
// exist. Exposed so serve's startup log can point at it.
func KnownTextPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".nvpi", "known_text_models.json")
}

// loadKnownText returns the cached id set; missing/corrupt file → nil (probe
// everything). No TTL by design: a retired model leaves via the catalog
// (absent from the catalog list is dropped), so a stale entry is harmless.
func loadKnownText(path string) map[string]bool {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var ids []string
	if json.Unmarshal(data, &ids) != nil {
		return nil
	}
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// saveKnownText persists the id set (atomic tmp+rename so a crash cannot leave
// a half file). Errors are non-fatal: worst case next refresh probes again.
func saveKnownText(path string, ids []string) {
	if path == "" || len(ids) == 0 {
		return
	}
	sort.Strings(ids)
	data, err := json.Marshal(ids)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) != nil {
		return
	}
	_ = os.Rename(tmp, path)
}
