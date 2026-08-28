// cmd/serve — multi-format OpenAI/Claude/Responses gateway for NVIDIA playground.
//
// Embeds CLIProxyAPI with a custom nvidia ProviderExecutor. Upstream predict is
// already OpenAI Chat Completions shape; builtin translators expose:
//
//	POST /v1/chat/completions
//	POST /v1/responses
//	POST /v1/messages
//
// No inbound gateway API keys. Captcha via -auto pool, -captcha, or nv-captcha-token.
//
// Usage:
//
//	go run ./cmd/serve -auto
//	go run ./cmd/serve -auto -pool-size=6 -pool-workers=3 -pool-batch=6 -coalesce-ms=0 -max-inflight=8
//	go run ./cmd/serve -captcha "P1_..."
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"glm52-nvidia/internal/captcha"
	"glm52-nvidia/internal/models"
	"glm52-nvidia/internal/provider/nvidia"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"

	_ "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator/builtin"
)

// Set via -ldflags "-X main.version=v1.2.3" at release build time.
var version = "dev"

func main() {
	start := time.Now()

	// Loopback by default: this gateway is unauthenticated, so binding all
	// interfaces invites the LAN to burn the captcha pool. Dockerfile and
	// docker-compose pass "-addr :8080" explicitly, so they still expose it.
	addr := flag.String("addr", "127.0.0.1:8080", "listen address (default loopback: unauthenticated gateway; pass \"-addr :8080\" or use Docker/compose, which set it explicitly, to expose)")
	captchaFlag := flag.String("captcha", "", "one-shot hCaptcha token (consumed on first use)")
	auto := flag.Bool("auto", false, "prewarm captcha tokens via shared Chrome + pool")
	// Defaults sized so ONE Chrome serves a Claude Code burst: a batch of 6
	// one-shot tokens covers max-inflight 8 within the token TTL, instead of
	// spawning more Chromes (each ~100-350MB).
	poolSize := flag.Int("pool-size", 6, "ready captcha tokens to keep buffered (-auto)")
	poolWorkers := flag.Int("pool-workers", 3, "concurrent captcha mint workers (-auto); set >= -chromes-max so bursts can drive every Chrome; blocked-on-browser workers cost nothing")
	poolBatch := flag.Int("pool-batch", 6, "captcha tokens minted per Chrome visit (extra invisible hCaptcha widgets rendered on the sticky tab); 1 = one token per visit; capped by -pool-size free slots")
	chromesMax := flag.Int("chromes-max", 2, "elastic Chrome ceiling (-auto): group starts with 1 Chrome and spawns more under borrow pressure up to this; 1 = fixed single Chrome")
	harness := flag.Bool("captcha-harness", true, "mint on a minimal same-origin harness page instead of the full Next.js playground (~50MB vs ~150-350MB per chrome, ~2s warm); auto-falls back to the full page (with playground selection) when the sitekey scrape fails")
	chromeIdleRecycle := flag.Duration("chrome-idle-recycle", 10*time.Minute, "close Chromes idle longer than this (-auto elastic), keeping at least 1; 10min matches sticky-tab staleness so recycling loses nothing")
	maxInflight := flag.Int("max-inflight", 8, "max concurrent upstream streams (0=unlimited); 8 absorbs Claude Code parallel tool-call bursts — overflow queues at TakeLease instead of failing hard")
	inflightWait := flag.Duration("inflight-wait", 500*time.Millisecond, "how long to wait for an in-flight slot before returning 503 (0=reject immediately)")
	coalesceMs := flag.Int("coalesce-ms", 16, "merge consecutive SSE content deltas within this window (0=off); first token always flushes immediately")
	warmTimeout := flag.Duration("warm-timeout", 3*time.Minute, "wait for at least one pooled captcha before serving (-auto); 0=skip")
	poolTTL := flag.Duration("pool-ttl", 90*time.Second, "discard pooled captcha tokens older than this (-auto)")
	captchaWait := flag.Duration("captcha-wait", 30*time.Second, "max wait for a pooled captcha token per request (0=block until ready); then 503")
	chromeProxy := flag.String("chrome-proxy", "", "proxy for captcha Chrome and upstream API (e.g. socks5://host:port); falls back to CHROME_PROXY")
	captchaPlayground := flag.String("captcha-playground", "", "playground URL pinned for captcha minting; empty = auto-select the fastest alive playground at startup (sliding-window bench, memory in ~/.nvpi/playground-state.json)")
	selectBudget := flag.Duration("captcha-select-budget", 4*time.Minute, "wall-clock budget for playground auto-selection (-captcha-playground empty); dead pages cost up to 30s each, so a fully-dead window plus rescue sweep needs minutes; past it the decision uses whatever was already measured")
	defaultModel := flag.String("default-model", "", "rewrite unknown requested models (e.g. Claude Code's builtin claude-*) to this registered model instead of rejecting with 400; empty = strict")
	debugSseDir := flag.String("debug-sse-dir", "", "if non-empty, capture raw upstream SSE + post-SDK Anthropic payloads to <dir>/{raw,anth}-<unixnano>-<model>.log for debugging; empty = disabled")
	refreshRegistry := flag.Bool("refresh-registry", true, "re-fetch the model registry from upstream catalog at startup (falls back to hardcoded list on failure)")
	registryTimeout := flag.Duration("registry-timeout", 5*time.Minute, "ceiling for the startup registry refresh (returns early when probes finish)")
	flag.Parse()

	if !*auto && *captchaFlag == "" {
		log.Print("warning: no -auto/-captcha; each request must send nv-captcha-token")
	}

	proxyURL := strings.TrimSpace(*chromeProxy)
	if proxyURL == "" {
		proxyURL = strings.TrimSpace(os.Getenv("CHROME_PROXY"))
	}
	proxyFunc := http.ProxyFromEnvironment
	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			log.Fatalf("chrome-proxy: invalid URL %q", proxyURL)
		}
		proxyFunc = http.ProxyURL(u)
		log.Printf("upstream + captcha proxy=%s", proxyURL)
	}

	transport := &http.Transport{
		Proxy: proxyFunc,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   64,
		MaxConnsPerHost:       0,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}

	// SIGTERM too: Docker stop / systemd send TERM, not INT. Without it the
	// graceful path (pool.Close, browser.Close, svc.Run return) never runs and
	// the container eats the full SIGKILL grace period with Chromes unclean.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Re-fetch the model registry from the upstream NVIDIA catalog so newly
	// added / retired models show up without a rebuild. Best-effort: failure
	// logs and the hardcoded registry stays in place.
	if *refreshRegistry {
		// Route the refresh through the same proxy the executor transport
		// uses; in proxy-only networks a plain client can never reach the
		// upstream catalog.
		res, err := models.Refresh(ctx, models.RefreshOptions{
			Timeout:    *registryTimeout,
			HTTPClient: &http.Client{Timeout: 15 * time.Second, Transport: transport},
		})
		if err != nil {
			log.Printf("models: refresh failed (%v) — keeping hardcoded registry (%d models)", err, len(models.Models))
		} else {
			log.Printf("models: refreshed %d/%d (probed=%d cached=%d skipped=%d textModels=%d) in %s",
				res.OK, res.Listed, res.Probed, res.Cached, res.Skipped, res.WithCaps, res.Duration.Round(time.Millisecond))
		}
	}

	var (
		browser *captcha.BrowserGroup
		pool    *captcha.Pool
	)
	if *auto {
		pgURL := strings.TrimSpace(*captchaPlayground)
		var seed *captcha.Browser
		var sk string
		useHarness := *harness
		if useHarness {
			if pgURL == "" {
				// Harness minting is model-agnostic — the URL is only the
				// plain-HTTP scrape source for the sitekey.
				pgURL = captcha.PlaygroundURL(models.DefaultModel)
			}
			sk = captcha.LoadCachedSitekey()
			if sk != "" {
				log.Printf("captcha harness: cached sitekey (<24h old); skipped HTTP scrape. Delete %s to force a re-scrape.",
					captcha.SitekeyCachePath())
			} else {
				skCtx, skCancel := context.WithTimeout(ctx, 2*time.Minute)
				var skErr error
				sk, skErr = captcha.FetchSitekeyHTTP(skCtx, nil, pgURL)
				skCancel()
				if skErr != nil {
					log.Printf("captcha harness: sitekey scrape failed (%v); falling back to full-page mint", skErr)
					useHarness = false
					pgURL = ""
				} else {
					captcha.SaveCachedSitekey(sk)
				}
			}
		}
		if !useHarness && pgURL == "" {
			pgURL, seed = selectPlayground(ctx, proxyURL, *selectBudget)
		}
		initial := 1
		if seed != nil {
			// Probe Chrome handed over below; a factory launch here would be a
			// throwaway second Chrome.
			initial = 0
		}
		var err error
		browser, err = captcha.NewBrowserGroup(ctx, initial, captcha.BrowserConfig{
			Proxy:          proxyURL,
			Playground:     pgURL,
			Harness:        useHarness,
			HarnessSitekey: sk,
		})
		if err != nil {
			log.Fatalf("captcha browser: %v", err)
		}
		browser.EnableElastic(*chromesMax, *chromeIdleRecycle)
		if seed != nil {
			browser.AppendWarmed(seed)
		}
		pool = captcha.NewPool(ctx, browser.Extract, captcha.PoolConfig{
			Size:         *poolSize,
			Workers:      *poolWorkers,
			TTL:          *poolTTL,
			Batch:        *poolBatch,
			BatchExtract: browser.ExtractBatch,
		})
		defer func() {
			pool.Close()
			browser.Close()
		}()
		log.Printf("captcha pool: size=%d workers=%d batch=%d chromes=%d/%d ttl=%s captcha-wait=%s",
			*poolSize, *poolWorkers, *poolBatch, browser.Len(), *chromesMax, *poolTTL, *captchaWait)

		if *warmTimeout > 0 {
			log.Printf("warming captcha pool (timeout=%s)…", *warmTimeout)
			if err := waitPoolReady(ctx, pool, 1, *warmTimeout); err != nil {
				log.Printf("warning: %v — first requests may block on captcha extract", err)
			} else {
				log.Printf("captcha pool ready=%d (TTFT path unblocked)", pool.Ready())
			}
		}
	}

	exec := nvidia.NewExecutor(nvidia.Options{
		Auto:         *auto,
		FlagCaptcha:  *captchaFlag,
		Coalesce:     time.Duration(*coalesceMs) * time.Millisecond,
		MaxInflight:  *maxInflight,
		InflightWait: *inflightWait,
		CaptchaWait:  *captchaWait,
		DefaultModel: *defaultModel,
		DebugSseDir:  *debugSseDir,
		HTTPClient:   &http.Client{Timeout: 0, Transport: transport},
		Pool:         pool,
	})

	cfg, cfgPath, err := buildConfig(*addr)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	defer os.RemoveAll(cfg.AuthDir)

	tokenStore := sdkAuth.GetTokenStore()
	if dirSetter, ok := tokenStore.(interface{ SetBaseDir(string) }); ok {
		dirSetter.SetBaseDir(cfg.AuthDir)
	}

	models := nvidia.RegistryModels()
	authHook := &nvidiaAuthHook{exec: exec, models: models}
	core := coreauth.NewManager(tokenStore, nil, authHook)
	authHook.core = core
	core.RegisterExecutor(exec)
	// Do NOT Register auth before Run: coreManager.Load() resets from AuthDir.
	// Auth file is written in buildConfig so Load() picks up provider=nvidia.

	// Watcher replaces unknown providers with OpenAICompatExecutor and clears
	// models via UnregisterClient; hooks + reconciler put ours back.
	cliproxy.SetGlobalModelRegistryHook(&nvidiaModelHook{core: core, exec: exec, models: models})
	bindNvidiaRuntime(core, exec, models)

	hooks := cliproxy.Hooks{
		OnAfterStart: func(_ *cliproxy.Service) {
			ensureNvidiaAuth(core)
			n := bindNvidiaRuntime(core, exec, models)
			startNvidiaReconciler(ctx, core, exec, models)
			log.Printf("serve %s listening on http://localhost%s (models=%d auth=%d; chat/completions + responses + messages; coalesce=%s max-inflight=%d)",
				version, *addr, len(models), n, execCoalesce(*coalesceMs), *maxInflight)
		},
	}

	svc, err := cliproxy.NewBuilder().
		WithConfig(cfg).
		WithConfigPath(cfgPath).
		WithCoreAuthManager(core).
		WithServerOptions(
			// CLIProxyAPI already registers GET/HEAD /healthz; re-registering panics.
			// Install middleware before routes so we can enrich the response with pool stats.
			api.WithEngineConfigurator(func(engine *gin.Engine) {
				engine.GET("/hello", func(c *gin.Context) {
					c.JSON(http.StatusOK, gin.H{"hello": "nvpi"})
				})
				registerAPIHello(engine)
				registerWeb(engine, WebDeps{
					Version:     version,
					Start:       start,
					Auto:        *auto,
					Pool:        pool,
					Browser:     browser,
					ChromesMax:  *chromesMax,
					MaxInflight: *maxInflight,
					CoalesceMs:  *coalesceMs,
					PoolSize:    *poolSize,
					PoolBatch:   *poolBatch,
				})
				engine.Use(func(c *gin.Context) {
					if c.Request.URL.Path != "/healthz" {
						c.Next()
						return
					}
					switch c.Request.Method {
					case http.MethodHead:
						c.Status(http.StatusOK)
						c.Abort()
					case http.MethodGet:
						out := gin.H{"ok": true}
						if p := exec.Pool(); p != nil {
							fills, takes, errs, expired, staleLeases, ttl := p.Stats()
							// nav/sticky counters live on the Chrome group, not
							// the token pool; zero when -auto is off.
							var navCount, stickyCount uint64
							if browser != nil {
								navCount = browser.NavCount()
								stickyCount = browser.StickyCount()
							}
							out["pool"] = gin.H{
								"ready":       p.Ready(),
								"fills":       fills,
								"takes":       takes,
								"errors":      errs,
								"expired":     expired,
								"staleLeases": staleLeases,
								"ttlSec":      int(ttl.Seconds()),
								// Live Chrome-process count (elastic group scales
								// between 1 and -chromes-max under load).
								"chromes":     browserChromeCount(browser),
								"navCount":    navCount,
								"stickyCount": stickyCount,
							}
						}
						c.JSON(http.StatusOK, out)
						c.Abort()
					default:
						c.Next()
					}
				})
			}),
		).
		WithHooks(hooks).
		Build()
	if err != nil {
		log.Fatalf("build gateway: %v", err)
	}

	if err := svc.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}

func execCoalesce(ms int) time.Duration {
	return time.Duration(ms) * time.Millisecond
}

// browserChromeCount is the live Chrome-process count for healthz; nil-safe
// (zero when -auto is off).
func browserChromeCount(browser *captcha.BrowserGroup) int {
	if browser == nil {
		return 0
	}
	return browser.Len()
}

// selectPlayground benches catalog playgrounds on one throwaway Chrome and
// returns (winning URL, probe Chrome left mounted on the winner). The Chrome
// becomes seeded group capacity, so the pool's first mint skips a navigate.
// Total probe failure falls back to the first catalog entry (nil seed) — the
// group's own launch then surfaces any hard failure as before.
func selectPlayground(ctx context.Context, proxyURL string, budget time.Duration) (string, *captcha.Browser) {
	ids := make([]string, 0, len(models.Models))
	for id := range models.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	fallback := ""
	if len(ids) > 0 {
		fallback = captcha.PlaygroundURL(ids[0])
	}

	pb, err := captcha.NewBrowser(ctx, captcha.BrowserConfig{Proxy: proxyURL, NoWarm: true})
	if err != nil {
		log.Printf("captcha playground select: probe chrome unavailable (%v) — falling back to %s", err, fallback)
		return fallback, nil
	}
	last := ""
	sel := &captcha.PlaygroundSelector{
		Budget: budget,
		Probe: func(pctx context.Context, id string) (time.Duration, error) {
			last = id
			return pb.ProbeNav(pctx, captcha.PlaygroundURL(id))
		},
	}
	statePath := captcha.DefaultStatePath()
	winner, st, err := sel.Select(ctx, captcha.LoadState(statePath), ids)
	if err != nil {
		pb.Close()
		log.Printf("captcha playground select failed (%v) — falling back to %s", err, fallback)
		return fallback, nil
	}
	pgURL := captcha.PlaygroundURL(winner)
	if last != winner {
		// Leave the tab on the winner so seeding pays off; a failure here means
		// the page died in the last seconds — let the group launch fresh.
		if _, merr := pb.ProbeNav(ctx, pgURL); merr != nil {
			pb.Close()
			log.Printf("captcha playground select: mount %s failed (%v) — group launches fresh", pgURL, merr)
			return pgURL, nil
		}
	}
	if statePath == "" {
		log.Printf("captcha playground selected %s (no home dir — state not persisted)", pgURL)
		return pgURL, pb
	}
	if serr := captcha.SaveState(statePath, st); serr != nil {
		log.Printf("captcha playground state save failed: %v", serr)
	}
	log.Printf("captcha playground selected %s (%.0fms median)", pgURL, st.Benched[winner].MedianMS)
	return pgURL, pb
}

func waitPoolReady(ctx context.Context, pool *captcha.Pool, want int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()
	for {
		if pool.Ready() >= want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("captcha pool still empty after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
