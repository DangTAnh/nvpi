package captcha

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// backoff schedule on extract failure. Starts small, caps so a sustained
// captcha-block doesn't busy-loop or spam logs. Reset to zero on success.
// backoffMax was 30s: during a real NVIDIA rate-limit / network outage a worker
// looped 90s+30s≈120s per mint attempt, relaunching Chrome uselessly while the
// pool sat empty. 2min backoff stops hammering a known-down endpoint and lets
// the outage clear (or an operator react) without burning Chrome churn.
const (
	backoffMin    = 1 * time.Second
	backoffMax    = 2 * time.Minute
	backoffJitter = 250 * time.Millisecond // ±25% via Int63n below
	// log every N consecutive failures instead of each one, so a persistent
	// captcha outage does not flood logs.
	logEveryNth = 10
)

// ExtractFunc obtains one one-shot captcha token.
type ExtractFunc func(ctx context.Context) (string, error)

type entry struct {
	token string
	at    time.Time
	order uint64
}

// TokenLease holds a pooled token until the caller knows whether it was sent.
// A lease can be finalized once: Commit consumes the token, while Release
// returns a still-valid token to its original FIFO position.
type TokenLease struct {
	pool  *Pool
	entry entry
	once  sync.Once
	used  bool
}

// Token returns the leased one-shot token.
func (l *TokenLease) Token() string {
	return l.entry.token
}

// Commit consumes a token only if it is still fresh at the commit boundary.
func (l *TokenLease) Commit() bool {
	l.once.Do(func() {
		l.used = l.pool.finalizeLease(l.entry, false)
	})
	return l.used
}

// Release returns the token to the pool if it is still open and the original
// token timestamp is still within TTL.
func (l *TokenLease) Release() {
	l.once.Do(func() {
		l.pool.finalizeLease(l.entry, true)
	})
}

// Pool pre-warms one-shot captcha tokens so request handlers can Take without
// waiting on a full browser navigate.
// Tokens older than TTL are discarded on Take (hCaptcha tokens expire ~2–3 min).
//
// A background reaper discards stale buffered tokens during idle so workers are
// not stuck behind a full buffer of expired entries (the "chat, then wait,
// then request hangs" failure mode).
//
// Workers wait for buffer space *before* minting. Combined with a mutex-backed
// FIFO (not a channel drain/restore), a full fresh pool truly idles Chrome —
// see runs/hangbench-2026-07-22.md.
type Pool struct {
	extract      ExtractFunc
	batch        int // >1 with batchExtract enables multi-token minting per visit
	batchExtract func(ctx context.Context, n int) ([]string, error)
	size         int
	ttl          time.Duration

	// ttl adaptive bounds: TTL floats between these based on how often tokens
	// expire before use. A high stale rate (tokens sitting stale) tightens TTL
	// so workers serve fresher tokens; a near-zero stale rate loosens TTL to
	// reduce fill rate. See adjustTTL.
	ttlMin time.Duration
	ttlMax time.Duration

	// staleWindow tracks expirations vs usable takes over a sliding window so
	// adjustTTL can react to workload, not just the cumulative Stats counters.
	staleWindowExpired uint64
	staleWindowUsed    uint64

	mu        sync.Mutex
	tokens    []entry
	reserved  int // workers currently minting for an available slot
	leased    int // tokens held by callers but still occupying capacity
	nextOrder uint64
	changed   chan struct{} // closed/replaced whenever queue capacity or data changes

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	fills   atomic.Uint64
	takes   atomic.Uint64
	errors  atomic.Uint64
	expired atomic.Uint64

	// staleLeases counts tokens a caller took but Commit rejected as expired —
	// a separate failure mode from buffer-reaped stale. Exposed via Stats.
	staleLeases atomic.Uint64
}

// PoolConfig controls prewarm depth and per-visit mint depth.
type PoolConfig struct {
	Size    int           // buffered ready tokens (default 2)
	Workers int           // concurrent extractors (default 1)
	TTL     time.Duration // starting max age before discard; floats [60s,115s] adaptively (default 90s)
	// Batch caps tokens per mint when BatchExtract is set. <=1 or nil
	// BatchExtract runs the legacy one-token-per-extract path.
	Batch        int
	BatchExtract func(ctx context.Context, n int) ([]string, error)
}

// NewPool starts background workers that keep tokens filled up to Size.
// extract must be safe for concurrent use up to Workers (e.g. Browser.Extract).
func NewPool(parent context.Context, extract ExtractFunc, cfg PoolConfig) *Pool {
	if cfg.Size < 1 {
		cfg.Size = 2
	}
	if cfg.Workers < 1 {
		cfg.Workers = 1
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 90 * time.Second
	}
	// Adaptive TTL bounds: TTL floats [60s, 115s]. hCaptcha tokens live ~120s,
	// so 115s is the safe ceiling; 60s is the floor below which fill rate
	// busy-loops. Production TTL is clamped to [60s, 115s]; sub-second TTLs
	// (only seen from unit tests asserting expire/reap behavior) are honored
	// as-is so the same code path validates both production and tests.
	ttlMin, ttlMax := 60*time.Second, 115*time.Second
	if cfg.TTL < time.Second {
		ttlMin = cfg.TTL
	} else {
		if cfg.TTL < ttlMin {
			cfg.TTL = ttlMin
		}
		if cfg.TTL > ttlMax {
			cfg.TTL = ttlMax
		}
	}
	ctx, cancel := context.WithCancel(parent)
	p := &Pool{
		extract:      extract,
		batch:        cfg.Batch,
		batchExtract: cfg.BatchExtract,
		size:         cfg.Size,
		tokens:       make([]entry, 0, cfg.Size),
		changed:      make(chan struct{}),
		ttl:          cfg.TTL,
		ttlMin:       ttlMin,
		ttlMax:       ttlMax,
		ctx:          ctx,
		cancel:       cancel,
	}
	for i := 0; i < cfg.Workers; i++ {
		p.wg.Add(1)
		go p.worker(i)
	}
	p.wg.Add(1)
	go p.reaper()
	return p
}

// wantBatch reports how many tokens one mint attempt targets: the configured
// batch size when batch minting is enabled (BatchExtract set, Batch >= 2),
// else 1 — the legacy single-token path.
func (p *Pool) wantBatch() int {
	if p.batchExtract == nil || p.batch < 2 {
		return 1
	}
	return p.batch
}

func (p *Pool) worker(id int) {
	defer p.wg.Done()
	var consecFailures int
	for {
		k, ok := p.reserveSlots(p.wantBatch())
		if !ok {
			return
		}

		if k > 1 {
			toks, err := p.batchExtract(p.ctx, k)
			if len(toks) > k {
				toks = toks[:k] // defensive: a mint must never exceed its reservation
			}
			m := len(toks)
			if err != nil && m == 0 {
				p.releaseReservations(k)
				p.errors.Add(1)
				consecFailures++
				if p.ctx.Err() != nil {
					return
				}
				if consecFailures == 1 || consecFailures%logEveryNth == 0 {
					log.Printf("captcha pool worker %d: batch mint failed: %v (consecutive failures=%d, backing off)",
						id, err, consecFailures)
				}
				backoff := backoffFor(consecFailures)
				select {
				case <-time.After(backoff):
				case <-p.ctx.Done():
					return
				}
				continue
			}
			consecFailures = 0
			if err != nil {
				log.Printf("captcha pool worker %d: batch %d/%d: %v", id, m, k, err)
			}
			placed := 0
			for _, t := range toks {
				if !p.enqueue(t) {
					return // pool closing; enqueue already consumed this reservation
				}
				placed++
			}
			p.releaseReservations(k - placed)
			continue
		}

		token, err := p.extract(p.ctx)
		if err != nil {
			p.releaseReservation()
			p.errors.Add(1)
			consecFailures++
			if p.ctx.Err() != nil {
				return
			}
			// Exponential backoff with jitter — a sustained captcha outage
			// must not busy-loop (fixed 2s did) nor drown the logs. Log the
			// first failure immediately (pool-empty hangs are otherwise silent),
			// then every Nth. Reset on success below.
			if consecFailures == 1 || consecFailures%logEveryNth == 0 {
				log.Printf("captcha pool worker %d: %v (consecutive failures=%d, backing off)",
					id, err, consecFailures)
			}
			backoff := backoffFor(consecFailures)
			select {
			case <-time.After(backoff):
			case <-p.ctx.Done():
				return
			}
			continue
		}

		consecFailures = 0
		if !p.enqueue(token) {
			return
		}
	}
}

// reserveSlots blocks until at least one capacity slot is free, then claims
// min(k, free) slots before extraction. The reservation prevents concurrent
// workers from over-minting. A batch shrinks to what fits: it must never
// overflow the buffer, and waiting for all k slots would stall filling when
// the pool is nearly full. Returns slots claimed (>= 1); false when closed.
func (p *Pool) reserveSlots(k int) (int, bool) {
	for {
		p.mu.Lock()
		if p.ctx.Err() != nil {
			p.mu.Unlock()
			return 0, false
		}
		free := p.size - (len(p.tokens) + p.reserved + p.leased)
		take := k
		if take > free {
			take = free
		}
		if take > 0 {
			p.reserved += take
			p.mu.Unlock()
			return take, true
		}
		changed := p.changed
		p.mu.Unlock()
		select {
		case <-p.ctx.Done():
			return 0, false
		case <-changed:
		}
	}
}

func (p *Pool) releaseReservation() {
	p.mu.Lock()
	p.reserved--
	p.notifyLocked()
	p.mu.Unlock()
}

// releaseReservations returns n unused claimed slots (a batch that minted
// fewer tokens than it reserved). No-op for n <= 0.
func (p *Pool) releaseReservations(n int) {
	if n <= 0 {
		return
	}
	p.mu.Lock()
	p.reserved -= n
	p.notifyLocked()
	p.mu.Unlock()
}

func (p *Pool) enqueue(token string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reserved--
	if p.ctx.Err() != nil {
		p.notifyLocked()
		return false
	}
	p.tokens = append(p.tokens, entry{token: token, at: time.Now(), order: p.nextOrder})
	p.nextOrder++
	p.fills.Add(1)
	p.notifyLocked()
	return true
}

// notifyLocked wakes waiters without polling. p.mu must be held.
func (p *Pool) notifyLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

// reaper drops expired FIFO-front entries during idle so workers can refill,
// and adjusts TTL adaptively each tick: a high stale-to-used ratio tightens TTL
// (serve fresher tokens at higher fill cost); a near-zero ratio loosens TTL
// (reduce fill rate). Ticker follows the current TTL so it co-scales with it.
func (p *Pool) reaper() {
	defer p.wg.Done()
	t := time.NewTicker(reaperInterval(p.ttl))
	defer t.Stop()
	var lastLog time.Time
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-t.C:
			n := p.discardStale()
			if prevTTL, newTTL, adjusted := p.adjustTTL(n); adjusted {
				// Re-arm the ticker to the new TTL cadence.
				t.Reset(reaperInterval(newTTL))
				log.Printf("captcha pool: ttl adapted %.0fs→%.0fs (stale/used=%d/%d, reaped %d); ready=%d",
					prevTTL.Seconds(), newTTL.Seconds(),
					p.staleWindowExpired, p.staleWindowUsed, n, p.Ready())
			} else if n > 0 {
				if time.Since(lastLog) < time.Minute && p.Ready() > 0 {
					continue
				}
				lastLog = time.Now()
				log.Printf("captcha pool: reaped %d stale token(s); ready=%d (workers refill)", n, p.Ready())
			}
		}
	}
}

// reaperInterval ticks at ttl/4, clamped to [10s, 30s] for production TTLs
// (hCaptcha ~120s). Short-TTL test pools (TTL < 1s) get ttl/4 uncapped to keep
// reaper fires proportional — otherwise a 200ms TTL with a 10s reaper misses
// every reap window in the test. The 10s floor avoids busy-loop with TTL=90s.
func reaperInterval(ttl time.Duration) time.Duration {
	d := ttl / 4
	if ttl >= time.Second {
		if d > 30*time.Second {
			d = 30 * time.Second
		}
		if d < 10*time.Second {
			d = 10 * time.Second
		}
	}
	return d
}

// adjustTTL nudges TTL based on the recent stale-to-used ratio. Returns the
// previous and new TTL, and whether it changed. A stale rate above staleHi
// tightens TTL (tokens sit unused → serve fresher ones); below staleLo loosens
// it (tokens get used well before expiring → reduce fill churn). The window
// resets after each evaluation so the controller reacts to current load, not
// cumulative lifetime stats.
//
// ponytail: single-step P-controller (±10s/tick). Simpler than integral/derivative
// terms and stable here — the signal (stale rate) is already smoothed by the
// reaper tick. If TTL oscillates under steady load, add deadband or windowing.
func (p *Pool) adjustTTL(staleThisTick int) (prev, next time.Duration, adjusted bool) {
	const (
		staleHi = 0.30 // >30% stale → tighten
		staleLo = 0.05 // <5% stale  → loosen
		step    = 10 * time.Second
	)
	// p.ttl is read concurrently by TTL()/Stats() (healthz) — mutate it under
	// the same mutex those readers take.
	p.mu.Lock()
	defer p.mu.Unlock()
	prev = p.ttl
	total := p.staleWindowExpired + p.staleWindowUsed
	if total < 10 {
		// Too little signal this window to judge — don't thrash TTL on noise.
		p.staleWindowExpired, p.staleWindowUsed = 0, 0
		return prev, prev, false
	}
	ratio := float64(p.staleWindowExpired) / float64(total)
	switch {
	case ratio > staleHi:
		p.ttl -= step
	case ratio < staleLo:
		p.ttl += step
	}
	if p.ttl < p.ttlMin {
		p.ttl = p.ttlMin
	}
	if p.ttl > p.ttlMax {
		p.ttl = p.ttlMax
	}
	p.staleWindowExpired, p.staleWindowUsed = 0, 0
	return prev, p.ttl, p.ttl != prev
}

// discardStale drops only expired entries from the FIFO front without touching
// fresh tokens (inspect under mutex — no evacuate/restore race). Stale drops
// feed the adaptive window so adjustTTL sees them next tick.
func (p *Pool) discardStale() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for len(p.tokens) > 0 && time.Since(p.tokens[0].at) > p.ttl {
		p.tokens = p.tokens[1:]
		p.expired.Add(1)
		p.staleWindowExpired++
		n++
	}
	if n > 0 {
		p.notifyLocked()
	}
	return n
}

// backoffFor computes 2^n * backoffMin capped at backoffMax, ±jitter.
// n=1 → ~1s, n=4 → ~8s, n≥5 → capped near 30s.
func backoffFor(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	d := backoffMin
	for i := 1; i < n; i++ {
		d *= 2
		if d >= backoffMax {
			d = backoffMax
			break
		}
	}
	jitter := time.Duration(rand.Int63n(int64(2*backoffJitter))) - backoffJitter
	d += jitter
	if d < 0 {
		d = 0
	}
	return d
}

// TakeLease returns a prewarmed token that remains pool capacity until its
// lease is committed or released.
func (p *Pool) TakeLease(ctx context.Context) (*TokenLease, error) {
	for {
		p.mu.Lock()
		if err := ctx.Err(); err != nil {
			p.mu.Unlock()
			return nil, err
		}
		if p.ctx.Err() != nil {
			p.mu.Unlock()
			return nil, fmt.Errorf("captcha pool closed")
		}
		if len(p.tokens) > 0 {
			e := p.tokens[0]
			p.tokens = p.tokens[1:]
			if time.Since(e.at) > p.ttl {
				p.expired.Add(1)
				p.notifyLocked()
				p.mu.Unlock()
				continue
			}
			p.leased++
			p.mu.Unlock()
			return &TokenLease{pool: p, entry: e}, nil
		}
		changed := p.changed
		p.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-p.ctx.Done():
			return nil, fmt.Errorf("captcha pool closed")
		case <-changed:
		}
	}
}

// Take preserves the original consume-on-take API.
func (p *Pool) Take(ctx context.Context) (string, error) {
	for {
		lease, err := p.TakeLease(ctx)
		if err != nil {
			return "", err
		}
		token := lease.Token()
		if lease.Commit() {
			return token, nil
		}
	}
}

func (p *Pool) finalizeLease(e entry, release bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.leased--
	if !release {
		if time.Since(e.at) > p.ttl {
			p.expired.Add(1)
			p.staleLeases.Add(1)
			p.notifyLocked()
			return false
		}
		p.takes.Add(1)
		p.staleWindowUsed++
		p.notifyLocked()
		return true
	}
	if p.ctx.Err() != nil {
		p.notifyLocked()
		return false
	}
	if time.Since(e.at) > p.ttl {
		p.expired.Add(1)
		p.notifyLocked()
		return false
	}

	i := 0
	for i < len(p.tokens) && p.tokens[i].order < e.order {
		i++
	}
	p.tokens = append(p.tokens, entry{})
	copy(p.tokens[i+1:], p.tokens[i:])
	p.tokens[i] = e
	p.notifyLocked()
	return false
}

// Ready returns how many tokens are currently buffered (may include soon-to-expire).
func (p *Pool) Ready() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.tokens)
}

// TTL returns the current adaptive TTL.
func (p *Pool) TTL() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ttl
}

// Stats returns fill/take/error/expired/staleLeases counters and current TTL.
func (p *Pool) Stats() (fills, takes, errors, expired, staleLeases uint64, ttl time.Duration) {
	fills = p.fills.Load()
	takes = p.takes.Load()
	errors = p.errors.Load()
	expired = p.expired.Load()
	staleLeases = p.staleLeases.Load()
	p.mu.Lock()
	ttl = p.ttl
	p.mu.Unlock()
	return fills, takes, errors, expired, staleLeases, ttl
}

// Close stops workers and drains the browser-facing extract loop.
func (p *Pool) Close() {
	p.cancel()
	p.wg.Wait()
}
