package models

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
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
// Capabilities are scraped from the playground HTML:
//	GET https://build.nvidia.com/<publisher>/<slug>
//		→ next.js SSR HTML carrying an embedded JSON literal like
//		  {\"functionCalling\":true,\"structuredOutput\":true,\"reasoning\":true}
//		  The visible "Capabilities" sidebar is rendered client-side from the
//		  same data, so we cannot parse the rendered DOM without a JS engine.
//
// Text filter (2026-08-23): a model enters the refreshed registry only when
// its page carries a FULL modelCapability object — the chat-playground signal
// (pageIsText). outputModalities proved unreliable and was removed as the
// signal: embedding pages render an empty array or omit it, while ASR/retrieval
// pages omit modelCapability entirely; bge-m3 additionally shows that an
// object with only functionCalling is an embedding stub, not a chat page.
//
// ponytail: package-private constants + 3 fetch helpers + bounded concurrent
// probe. If the catalog call fails we keep the hardcoded registry — refresh
// is best-effort, the offline fallback already covers all known-good models.

const (
	catalogURL       = "https://integrate.api.nvidia.com/v1/models"
	queueURLBase     = "https://buildapi.ngc.nvidia.com/v2/predict/queues/models/" + Namespace + "/"
	playgroundURLFmt = "https://build.nvidia.com/%s/%s"
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
	WithCaps int // entries whose capability JSON literal parsed successfully
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
		opts.Timeout = 30 * time.Second
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

	discovered := probeQueues(ctx, opts.HTTPClient, ids, opts.Concurrency, &res)

	if res.OK > 0 {
		Models = mergeRegistry(discovered, ids, Models)
	}

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
func probeQueues(ctx context.Context, hc *http.Client, ids []string, concurrency int, res *RefreshResult) map[string]ModelInfo {
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

			// Capability + context length probe are best-effort and run after
			// the queue check so a retired model is dropped before we waste a
			// page fetch.
			caps, ctxLen, isText := probeCapabilitiesAndContext(ctx, hc, id)

			mu.Lock()
			if !isText {
				// Page shows no full modelCapability object — not a chat/text
				// playground (embedding stub, ASR, retrieval…). Keep it out.
				res.Skipped++
				mu.Unlock()
				return
			}
			res.WithCaps++
			out[id] = ModelInfo{
				Slug:          slugOf(id),
				Namespace:     Namespace,
				FunctionID:    fnID,
				Capability:    caps,
				ContextLength: ctxLen,
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

// capabilityJSONRE captures the embedded JSON literal
// `{\"functionCalling\":<bool>,\"structuredOutput\":<bool>,\"reasoning\":<bool>}`
// that Next.js inlines into the playground HTML for the Capabilities sidebar.
// The JSON is escaped because it lives inside a <script> tag, so we match
// the escaped quotes literally.
var capabilityJSONRE = regexp.MustCompile(`\\"functionCalling\\":(true|false),\\"structuredOutput\\":(true|false),\\"reasoning\\":(true|false)`)

// contextLengthRE captures `\\"contextLength\\":<int>` from the same
// specifications JSON literal. The value is in tokens (e.g. 1048576 for
// minimax-m3, 32768 for gemma-3n-e4b-it). 0 when absent.
var contextLengthRE = regexp.MustCompile(`\\"contextLength\\":([0-9]+)`)

// outputModalities proved unreliable in practice (arrays missing or empty on
// pages that are clearly not chat, e.g. baai/bge-m3 renders an empty []). The
// keep/drop signal is now the modelCapability object itself: see pageIsText.

// pageIsText reports whether the page carries a FULL modelCapability object
// (`{\"functionCalling\":…,\"structuredOutput\":…,\"reasoning\":…}`). Chat/text
// playgrounds always inline all three flags; embedding/ASR/retrieval pages omit
// the object entirely or render only the one-field stub
// `{\"functionCalling\":false}` (verified live 2026-08-23: minimax-m3 and
// nemotron-nano-12b-v2-vl carry all three; bge-m3 renders the stub;
// llama-3_2-nv-embedqa and riva-asr have none).
func pageIsText(body []byte) bool {
	return capabilityJSONRE.FindSubmatch(body) != nil
}

// probeCapabilitiesAndContext fetches the playground HTML once and extracts
// both the capability flags and the contextLength. isText=false means the page
// shows no full modelCapability object (see pageIsText) — such models must not
// enter the discovered map. An unreachable/malformed page also yields
// isText=false ("no proof of text"): hardcoded entries survive via the
// mergeRegistry carry-over; brand-new ones just wait for the next refresh.
// ctxLen stays 0 when the page lacks the specifications literal.
func probeCapabilitiesAndContext(ctx context.Context, hc *http.Client, id string) (caps *ModelCapability, ctxLen int, isText bool) {
	pub, slug, ok := splitPublisherSlug(id)
	if !ok {
		return nil, 0, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(playgroundURLFmt, pub, slug), nil)
	if err != nil {
		return nil, 0, false
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, 0, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, 0, false
	}

	m := capabilityJSONRE.FindSubmatch(body)
	if m == nil {
		return nil, 0, false
	}
	capsOut := &ModelCapability{
		ToolCalling:      string(m[1]) == "true",
		StructuredOutput: string(m[2]) == "true",
		Reasoning:        string(m[3]) == "true",
	}

	ctxLen = 0
	if m := contextLengthRE.FindSubmatch(body); m != nil {
		// ponytail: parseInt is one line; strconv.Atoi + err check is two.
		// A bad regex match (impossible: `[0-9]+`) would still surface as 0.
		ctxLen, _ = strconv.Atoi(string(m[1]))
	}
	return capsOut, ctxLen, true
}
