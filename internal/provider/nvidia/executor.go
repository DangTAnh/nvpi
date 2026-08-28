package nvidia

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	glm52 "glm52-nvidia"
	"glm52-nvidia/internal/captcha"
	"glm52-nvidia/internal/models"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	clipexec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

const providerKey = "nvidia"

// Options configures the Nvidia playground executor.
type Options struct {
	Auto         bool
	FlagCaptcha  string
	Coalesce     time.Duration
	MaxInflight  int
	InflightWait time.Duration
	CaptchaWait  time.Duration
	HTTPClient   *http.Client
	Pool         *captcha.Pool

	// PredictURL optionally overrides models.ModelInfo.PredictEndpoint (tests).
	PredictURL func(models.ModelInfo) string

	// DefaultModel optionally rewrites an unknown requested model to this
	// registered model before Lookup (e.g. Claude Code's builtin claude-*
	// names). Empty = strict 400 on unknown models (current behavior).
	DefaultModel string

	// DebugSseDir, when non-empty, enables raw upstream SSE + post-SDK
	// Anthropic payload capture for each streaming turn. Files are named
	// <unixnano>-<model>.log so multiple parallel turns sort
	// chronologically. Empty = disabled.
	DebugSseDir string
}

// Executor implements coreauth.ProviderExecutor for NVIDIA playground predict.
type Executor struct {
	auto         bool
	coalesce     time.Duration
	httpClient   *http.Client
	inflight     chan struct{}
	inflightWait time.Duration
	captchaWait  time.Duration
	pool         *captcha.Pool
	predictURL   func(models.ModelInfo) string
	defaultModel string

	beforeInflightWait func()
	beforeSend         func()

	debugSseDir string

	mu          sync.Mutex
	flagCaptcha string
}

// NewExecutor builds an Executor from Options.
func NewExecutor(opts Options) *Executor {
	e := &Executor{
		auto:         opts.Auto,
		coalesce:     opts.Coalesce,
		httpClient:   opts.HTTPClient,
		inflightWait: opts.InflightWait,
		captchaWait:  opts.CaptchaWait,
		pool:         opts.Pool,
		predictURL:   opts.PredictURL,
		defaultModel: opts.DefaultModel,
		flagCaptcha:  opts.FlagCaptcha,
		debugSseDir:  opts.DebugSseDir,
	}
	if e.httpClient == nil {
		e.httpClient = http.DefaultClient
	}
	if opts.MaxInflight > 0 {
		e.inflight = make(chan struct{}, opts.MaxInflight)
	}
	return e
}

// Identifier returns the provider key.
func (e *Executor) Identifier() string { return providerKey }

// Pool returns the captcha pool (may be nil).
func (e *Executor) Pool() *captcha.Pool { return e.pool }

// PrepareRequest is a no-op for playground auth (captcha is per-request).
func (e *Executor) PrepareRequest(_ *http.Request, _ *coreauth.Auth) error { return nil }

// Refresh returns auth unchanged.
func (e *Executor) Refresh(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
	return a, nil
}

// HttpRequest injects nothing and executes via the shared client.
func (e *Executor) HttpRequest(ctx context.Context, a *coreauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("nvidia executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, a); err != nil {
		return nil, err
	}
	return e.httpClient.Do(httpReq)
}

// Execute handles non-streaming chat completions against NVIDIA predict.
func (e *Executor) Execute(ctx context.Context, _ *coreauth.Auth, req clipexec.Request, opts clipexec.Options) (clipexec.Response, error) {
	body, info, err := e.preparePayload(req, opts, false)
	if err != nil {
		return clipexec.Response{}, err
	}
	upResp, release, err := e.doPredict(ctx, info, body, opts)
	if err != nil {
		return clipexec.Response{}, err
	}
	defer release()
	defer upResp.Body.Close()

	raw, err := io.ReadAll(upResp.Body)
	if err != nil {
		return clipexec.Response{}, err
	}
	from := sdktranslator.FormatOpenAI
	to := clipexec.ResponseFormatOrSource(opts)
	out := sdktranslator.TranslateNonStream(ctx, from, to, req.Model, opts.OriginalRequest, body, raw, nil)
	out = patchEmptyAnthropicTurn(out)
	return clipexec.Response{Payload: out, Headers: upResp.Header.Clone()}, nil
}

// ExecuteStream handles streaming chat completions against NVIDIA predict.
func (e *Executor) ExecuteStream(ctx context.Context, _ *coreauth.Auth, req clipexec.Request, opts clipexec.Options) (*clipexec.StreamResult, error) {
	body, info, err := e.preparePayload(req, opts, true)
	if err != nil {
		return nil, err
	}
	upResp, release, err := e.doPredict(ctx, info, body, opts)
	if err != nil {
		return nil, err
	}

	out := make(chan clipexec.StreamChunk, 16)
	go func() {
		defer close(out)
		defer release()
		defer upResp.Body.Close()

		from := sdktranslator.FormatOpenAI
		to := clipexec.ResponseFormatOrSource(opts)
		var param any
		guard := &emptyGuard{}

		reqID := extractRequestID(opts.OriginalRequest)
		debug := startSseDebug(e.debugSseDir, req.Model, reqID)
		defer debug.stop()

		emitLine := func(line string) error {
			debug.writeRaw(line)
			chunks := sdktranslator.TranslateStream(ctx, from, to, req.Model, opts.OriginalRequest, body, []byte(line), &param)
			for _, chunk := range chunks {
				debug.writeAnthropic(chunk)
				guard.observe(chunk)
				if guard.shouldEmitPlaceholder(chunk) {
					// SDK is closing the stream without ever opening a
					// content block; the host client would render "(no
					// content)" for this turn. Inject a single "."
					// text block so the turn is visible and the next
					// request's history is continuous.
					for _, placeholder := range emptyTextBlockChunks() {
						debug.writeAnthropic(placeholder)
						select {
						case out <- clipexec.StreamChunk{Payload: placeholder}:
						case <-ctx.Done():
							return ctx.Err()
						}
					}
				}
				select {
				case out <- clipexec.StreamChunk{Payload: chunk}:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		}

		if err := coalesceSSEEvents(upResp.Body, e.coalesce, emitLine); err != nil && ctx.Err() == nil {
			select {
			case out <- clipexec.StreamChunk{Err: err}:
			case <-ctx.Done():
			}
		}
	}()

	return &clipexec.StreamResult{Headers: upResp.Header.Clone(), Chunks: out}, nil
}

func (e *Executor) preparePayload(req clipexec.Request, opts clipexec.Options, stream bool) ([]byte, models.ModelInfo, error) {
	from := opts.SourceFormat
	if from == "" {
		from = sdktranslator.FormatOpenAI
	}

	// Step 1: resolve model BEFORE translation. If the caller's model name
	// isn't in the registry AND -default-model is set, the raw payload's
	// "model" field is rewritten here so normalizeThinking later picks the
	// right reasoning profile for the actual upstream target. Doing the
	// rewrite post-translate would build chat_template_kwargs for the wrong
	// model — e.g. Claude Code's "claude-sonnet-*" with thinking blocks
	// would emit a generic reasoning_effort that MiniMax's backend ignores,
	// leaving the model to hallucinate to fill the missing scaffold.
	finalModel, info, rawPayload, err := e.resolveModel(req.Model, req.Payload)
	if err != nil {
		return nil, models.ModelInfo{}, err
	}

	// Step 2: translate Anthropic / Responses → canonical Chat shape using
	// the resolved model name.
	payload, err := translateToChat(from, finalModel, rawPayload, stream)
	if err != nil {
		return nil, models.ModelInfo{}, requestErr(http.StatusBadRequest, "invalid json body")
	}

	// Step 3: normalizeThinking reads body["model"] to choose a reasoning
	// profile. finalModel is the authoritative name at this point.
	body, err := NormalizeRequestBody(payload)
	if err != nil {
		return nil, models.ModelInfo{}, requestErr(http.StatusBadRequest, "invalid json body")
	}

	// Step 4: ensure stream flag matches the execution mode and stream_options
	// are filled with sane defaults (skip-if-set so the caller can override
	// — e.g. reasoning models that need per-delta usage can pre-set
	// continuous_usage_stats=true).
	body, err = forceStreamFlag(body, stream)
	if err != nil {
		return nil, models.ModelInfo{}, requestErr(http.StatusBadRequest, "invalid json body")
	}
	return body, info, nil
}

// resolveModel returns the upstream model name to use, the matching ModelInfo,
// and the (possibly rewritten) raw payload bytes. If the caller's requested
// model is unknown and -default-model is set, the raw payload's "model" field
// is rewritten to the fallback so subsequent translation + normalization see
// the correct target. The rewrite is logged so misrouted requests are visible
// in stdout — silent rewrites used to hide Claude Code's "claude-*" builtin
// names being silently routed away from Kimi and friends.
func (e *Executor) resolveModel(requested string, payload []byte) (string, models.ModelInfo, []byte, error) {
	requested = strings.TrimSpace(requested)
	info, err := models.Lookup(requested)
	if err == nil {
		return requested, info, payload, nil
	}
	if e.defaultModel == "" {
		if uerr, ok := err.(*models.ErrUnknownModel); ok {
			return "", models.ModelInfo{}, nil, requestErr(http.StatusBadRequest, uerr.Error())
		}
		return "", models.ModelInfo{}, nil, err
	}
	var uerr *models.ErrUnknownModel
	if !errors.As(err, &uerr) {
		return "", models.ModelInfo{}, nil, err
	}
	rewritten, rerr := setBodyModel(payload, e.defaultModel)
	if rerr != nil {
		return "", models.ModelInfo{}, nil, requestErr(http.StatusBadRequest, "invalid json body")
	}
	info, err = models.Lookup(e.defaultModel)
	if err != nil {
		if uerr2, ok := err.(*models.ErrUnknownModel); ok {
			return "", models.ModelInfo{}, nil, requestErr(http.StatusBadRequest, uerr2.Error())
		}
		return "", models.ModelInfo{}, nil, err
	}
	log.Printf("nvidia: default-model rewrite %q -> %q (requested model not in registry; pass -default-model=\"\" to disable, or ensure crawler picked up the model)", requested, e.defaultModel)
	return e.defaultModel, info, rewritten, nil
}

// setBodyModel overwrites the model field in a JSON body. Works on both raw
// (Anthropic/Responses) and translated (OpenAI Chat) shapes since it only
// touches the top-level "model" key.
func setBodyModel(body []byte, name string) ([]byte, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	raw["model"] = name
	return json.Marshal(raw)
}

func forceStreamFlag(body []byte, stream bool) ([]byte, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	raw["stream"] = stream
	if stream {
		opts, _ := raw["stream_options"].(map[string]any)
		if opts == nil {
			opts = map[string]any{}
			raw["stream_options"] = opts
		}
		// include_usage: default true so callers see a final usage chunk.
		// Callers can pre-set it to false to opt out.
		if _, ok := opts["include_usage"]; !ok {
			opts["include_usage"] = true
		}
		// continuous_usage_stats: default false (usage once at end). Some
		// reasoning models rely on per-delta usage for token accounting —
		// callers can pre-set this to true; we never overwrite.
		if _, ok := opts["continuous_usage_stats"]; !ok {
			opts["continuous_usage_stats"] = false
		}
	}
	return json.Marshal(raw)
}

// effortLadder descends max → none. When the upstream returns 400 because
// the requested reasoning_effort is unsupported by this model, the executor
// walks down this ladder one tier at a time (max → xhigh → high → medium →
// low → none) and retries with a fresh captcha token. Each step consumes one
// captcha (single-use), so doPredict runs two independent budgets: ladder
// descents ≤ maxLadderSteps (5 = full walk) and captcha/stale-lease retries
// ≤ maxCaptchaRetries (2). Worst case 1 + 5 + 2 = 8 upstream sends instead
// of one bad request draining the whole token pool.
//
// ponytail: hardcoded list, not derived from effortRanks, because effortRanks
// is the *intent* scale (input validation) while this is the *retry* order
// (what we hand to upstream when our guess was wrong). They overlap but serve
// different purposes — keep them apart.
// ponytail: array (not slice) so len(effortLadder)-1 is a constant expression
// for maxLadderSteps below.
var effortLadder = [...]string{"max", "xhigh", "high", "medium", "low", "none"}

const (
	// maxCaptchaRetries bounds stale-lease / invalid-token refetches. A stale
	// token failing more than twice signals a systemic problem (proxy down,
	// poisoned pool) — surface 401 instead of draining the pool and starving
	// concurrent requests.
	maxCaptchaRetries = 2
	// maxLadderSteps bounds effort-ladder descents below the starting tier;
	// len(effortLadder)-1 is the full max→none walk.
	maxLadderSteps = len(effortLadder) - 1
)

// effortStartIndex returns the index in effortLadder for the reasoning_effort
// the caller already encoded in body (or chat_template_kwargs.reasoning_effort
// / thinking.reasoning_effort). Unknown efforts default to 0 ("max") so we
// start the ladder from the top — the safest assumption for a misconfigured
// caller is "they wanted max" and the upstream should walk down.
func effortStartIndex(body []byte) int {
	s := bodyCurrentEffort(body)
	for i, e := range effortLadder {
		if strings.EqualFold(e, s) {
			return i
		}
	}
	return 0
}

// bodyCurrentEffort pulls the requested effort out of body, looking at the
// three locations NVIDIA has used over time (top-level, chat_template_kwargs,
// and the thinking wrapper).
func bodyCurrentEffort(body []byte) string {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return ""
	}
	if s, ok := raw["reasoning_effort"].(string); ok && s != "" {
		return s
	}
	if kw, ok := raw["chat_template_kwargs"].(map[string]any); ok {
		if s, ok := kw["reasoning_effort"].(string); ok && s != "" {
			return s
		}
	}
	if t, ok := raw["thinking"].(map[string]any); ok {
		if s, ok := t["reasoning_effort"].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// mutateBodyEffort rewrites the reasoning_effort field in body to nextLevel.
// Returns the new body bytes and ok=true on success; ok=false means the body
// had no effort field to mutate (caller should stop the ladder).
func mutateBodyEffort(body []byte, nextLevel string) ([]byte, bool) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, false
	}
	touched := false
	if _, ok := raw["reasoning_effort"]; ok {
		raw["reasoning_effort"] = nextLevel
		touched = true
	}
	if kw, ok := raw["chat_template_kwargs"].(map[string]any); ok {
		if _, ok := kw["reasoning_effort"]; ok {
			kw["reasoning_effort"] = nextLevel
			touched = true
		}
	}
	if t, ok := raw["thinking"].(map[string]any); ok {
		if _, ok := t["reasoning_effort"]; ok {
			t["reasoning_effort"] = nextLevel
			touched = true
		}
	}
	if !touched {
		return nil, false
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}
	return out, true
}

// effortErrorRe matches a bare effort-tier word. \b matters: plain substring
// matching made "maximum" match "max" and "allow"/"below" match "low", so
// unrelated 400s burned a captcha on a pointless ladder retry.
var effortErrorRe = regexp.MustCompile(`(?i)\b(max|xhigh|high|medium|low|none)\b`)

// looksLikeEffortError checks whether the upstream error message is the kind
// that the ladder retry can recover from — i.e. "unsupported reasoning_effort"
// rather than an unrelated 400 (bad schema, missing field, etc.). We require
// the word "reasoning" plus a bare effort-tier word so we don't retry on every
// 400 by accident.
func looksLikeEffortError(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "reasoning") && effortErrorRe.MatchString(msg)
}

func (e *Executor) doPredict(ctx context.Context, info models.ModelInfo, body []byte, opts clipexec.Options) (*http.Response, func(), error) {
	clientToken := ""
	if opts.Headers != nil {
		clientToken = opts.Headers.Get("nv-captcha-token")
	}
	// Two independent retry budgets (see effortLadder above):
	//   - maxCaptchaRetries: stale-lease / invalid-token refetches. A stale
	//     token failing more than twice signals a systemic problem (proxy down,
	//     poisoned pool) — surface 401 instead of draining the pool and
	//     starving concurrent requests.
	//   - maxLadderSteps: effort-tier descents.
	// Worst case 1 + 2 + 5 = 8 upstream sends.
	captchaRetries := 0
	ladderSteps := 0

	var release func()
	cleanup := func() {
		if release != nil {
			release()
			release = nil
		}
	}

	endpoint := info.PredictEndpoint()
	if e.predictURL != nil {
		endpoint = e.predictURL(info)
	}

	// currentBody is mutated down the effort ladder when the upstream rejects
	// the requested tier. effortIdx points at the tier we are about to send.
	currentBody := body
	effortIdx := effortStartIndex(body)

	var upResp *http.Response
	for attempt := 1; ; attempt++ {
		token, lease, err := e.resolveCaptcha(ctx, clientToken, attempt == 1)
		if err != nil {
			cleanup()
			return nil, nil, captchaErr(err)
		}

		rel, err := e.acquireInflight(ctx)
		if err != nil {
			if lease != nil {
				lease.Release()
			}
			cleanup()
			return nil, nil, &coreauth.Error{
				Code:       "request_scoped",
				Message:    err.Error(),
				HTTPStatus: http.StatusServiceUnavailable,
			}
		}
		release = rel

		upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(currentBody))
		if err != nil {
			cleanup()
			if lease != nil {
				lease.Release()
			}
			return nil, nil, err
		}
		upReq.Header.Set("Content-Type", "application/json")
		upReq.Header.Set("Accept", "text/event-stream")
		upReq.Header.Set("nv-function-id", info.FunctionID)
		upReq.Header.Set("nv-captcha-token", token)
		upReq.Header.Set("Origin", "https://build.nvidia.com")
		upReq.Header.Set("Referer", "https://build.nvidia.com/")

		if e.beforeSend != nil {
			e.beforeSend()
		}
		if err := ctx.Err(); err != nil {
			cleanup()
			if lease != nil {
				lease.Release()
			}
			return nil, nil, captchaErr(err)
		}
		if lease != nil && !lease.Commit() {
			cleanup()
			captchaRetries++
			if captchaRetries > maxCaptchaRetries {
				return nil, nil, &coreauth.Error{
					Code:       "request_scoped",
					Message:    "captcha token invalid or expired; retry the request",
					HTTPStatus: http.StatusUnauthorized,
				}
			}
			attempt--
			continue
		}
		upResp, err = e.httpClient.Do(upReq)
		if err != nil {
			cleanup()
			return nil, nil, &coreauth.Error{
				Code:       "upstream_error",
				Message:    fmt.Sprintf("upstream: %v", err),
				HTTPStatus: http.StatusBadGateway,
			}
		}

		if upResp.StatusCode < 400 {
			return upResp, cleanup, nil
		}

		raw, _ := io.ReadAll(io.LimitReader(upResp.Body, 4<<10))
		status := upResp.StatusCode
		_ = upResp.Body.Close()
		upResp = nil
		release()
		release = nil

		retryable := isRetryableCaptchaFailure(status, raw)
		if retryable {
			if captchaRetries >= maxCaptchaRetries {
				return nil, nil, &coreauth.Error{
					Code:       "request_scoped",
					Message:    "captcha token invalid or expired; retry the request",
					HTTPStatus: http.StatusUnauthorized,
				}
			}
			captchaRetries++
			log.Printf("upstream captcha failure status=%d (attempt %d, captcha retry %d/%d); fetching a fresh token",
				status, attempt, captchaRetries, maxCaptchaRetries)
			continue
		}

		// Effort ladder: if the upstream rejected the reasoning_effort tier we
		// asked for, walk one step down and retry. We only descend on errors
		// that look effort-related (avoid mutating on every 400). Once we hit
		// the floor of the ladder or maxLadderSteps, give up and surface the
		// error verbatim.
		next := effortIdx + ladderSteps + 1
		if status == 400 && ladderSteps < maxLadderSteps && next < len(effortLadder) && looksLikeEffortError(string(raw)) {
			ladderSteps++
			rejected := effortLadder[next-1]
			nextBody, ok := mutateBodyEffort(currentBody, effortLadder[next])
			if !ok {
				log.Printf("upstream effort reject at tier=%s but body had no effort field to mutate; surfacing 400", rejected)
			} else {
				log.Printf("upstream rejected effort=%s; retrying at %s (attempt %d, ladder step %d/%d)",
					rejected, effortLadder[next], attempt, ladderSteps, maxLadderSteps)
				currentBody = nextBody
				continue
			}
		}

		msg := strings.TrimSpace(string(raw))
		if msg == "" {
			msg = "upstream request failed"
		}
		return nil, nil, &coreauth.Error{
			Code:       "upstream_error",
			Message:    msg,
			HTTPStatus: http.StatusBadGateway,
		}
	}
}

func (e *Executor) acquireInflight(ctx context.Context) (release func(), err error) {
	if e.inflight == nil {
		return func() {}, nil
	}
	release = func() { <-e.inflight }

	if e.inflightWait <= 0 {
		select {
		case e.inflight <- struct{}{}:
			return release, nil
		default:
			return nil, fmt.Errorf("max in-flight upstream streams reached; retry later")
		}
	}

	timer := time.NewTimer(e.inflightWait)
	defer timer.Stop()
	if e.beforeInflightWait != nil {
		e.beforeInflightWait()
	}
	select {
	case e.inflight <- struct{}{}:
		return release, nil
	case <-timer.C:
		return nil, fmt.Errorf("max in-flight upstream streams reached; retry later")
	case <-ctx.Done():
		return nil, fmt.Errorf("client cancelled before a stream slot opened")
	}
}

func (e *Executor) resolveCaptcha(ctx context.Context, clientToken string, allowFlag bool) (string, *captcha.TokenLease, error) {
	if clientToken != "" {
		return clientToken, nil, nil
	}

	if allowFlag {
		e.mu.Lock()
		flagToken := e.flagCaptcha
		if flagToken != "" {
			e.flagCaptcha = ""
		}
		e.mu.Unlock()
		if flagToken != "" {
			return flagToken, nil, nil
		}
	}

	if e.pool != nil {
		takeCtx := ctx
		var cancel context.CancelFunc
		if e.captchaWait > 0 {
			takeCtx, cancel = context.WithTimeout(ctx, e.captchaWait)
			defer cancel()
		}
		if e.pool.Ready() == 0 {
			waitFor := "indefinitely"
			if e.captchaWait > 0 {
				waitFor = e.captchaWait.String()
			}
			log.Printf("captcha pool empty; waiting up to %s (errors will surface from workers)", waitFor)
		}
		lease, err := e.pool.TakeLease(takeCtx)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				fills, takes, errs, expired, _, _ := e.pool.Stats()
				return "", nil, fmt.Errorf("captcha pool empty after %s (ready=%d fills=%d takes=%d errors=%d expired=%d); retry later",
					e.captchaWait, e.pool.Ready(), fills, takes, errs, expired)
			}
			return "", nil, err
		}
		token := lease.Token()
		return token, lease, nil
	}
	if e.auto {
		token, err := captcha.Extract(ctx)
		return token, nil, err
	}
	return "", nil, fmt.Errorf("captcha token required: send nv-captcha-token, or restart with -captcha / -auto")
}

func captchaErr(err error) error {
	status := http.StatusUnauthorized
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "captcha pool empty after") {
		status = http.StatusServiceUnavailable
	} else if errors.Is(err, context.Canceled) {
		status = http.StatusRequestTimeout
	}
	return &coreauth.Error{
		Code:       "request_scoped",
		Message:    err.Error(),
		HTTPStatus: status,
	}
}

func requestErr(status int, msg string) error {
	return &coreauth.Error{
		Code:       "request_scoped",
		Message:    msg,
		HTTPStatus: status,
	}
}

// isRetryableCaptchaFailure reports whether an upstream 4xx is a captcha /
// hCaptcha token failure fixed by fetching a fresh token and retrying.
func isRetryableCaptchaFailure(status int, raw []byte) bool {
	if status < 400 || status >= 500 {
		return false
	}
	if len(raw) == 0 {
		return false
	}
	var er glm52.ErrorResponse
	if json.Unmarshal(raw, &er) == nil {
		desc := strings.ToLower(er.RequestStatus.StatusDescription)
		if strings.Contains(desc, "token is invalid") || strings.Contains(desc, "invalid token") {
			return true
		}
		if er.RequestStatus.StatusCode == "INVALID_REQUEST" && strings.Contains(desc, "token") {
			return true
		}
	}
	low := strings.ToLower(string(raw))
	if strings.Contains(low, "token is invalid") {
		return true
	}
	return strings.Contains(low, "captcha") || strings.Contains(low, "hcaptcha")
}
