// captchabatch probes whether one sticky playground tab can mint N tokens per
// visit by rendering extra invisible hCaptcha widgets via hcaptcha.render().
// Throwaway measurement tool for the batch-mint design — not wired into serve.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/chromedp"

	"glm52-nvidia/internal/captcha"
)

func main() {
	pg := flag.String("url", "https://build.nvidia.com/minimaxai/minimax-m3/playground", "playground URL")
	n := flag.Int("n", 4, "total widgets to try (1 primary + n-1 rendered)")
	rounds := flag.Int("rounds", 3, "batch execute rounds")
	verify := flag.Bool("verify", false, "POST first token of each round to the predict endpoint")
	bench := flag.Int("bench-pool", 0, "run a Pool+BrowserGroup throughput bench with this batch size (skips the render probe)")
	chromesMax := flag.Int("chromes-max", 0, "with -bench-pool: start at 1 Chrome and scale elastically up to this under pressure (0 = fixed 2)")
	idleRecycle := flag.Duration("idle-recycle", 0, "with -chromes-max: close Chromes idle longer than this")
	flag.Parse()

	if *bench > 0 {
		benchPool(*bench, *chromesMax, *idleRecycle)
		return
	}

	allocOpts := captcha.ChromeAllocatorOptions()
	if path := os.Getenv("CHROME_PATH"); path != "" {
		allocOpts = append(allocOpts, chromedp.ExecPath(path))
	} else if _, err := os.Stat("chrome-headless-shell-win64/chrome-headless-shell.exe"); err == nil {
		allocOpts = append(allocOpts, chromedp.ExecPath("chrome-headless-shell-win64/chrome-headless-shell.exe"))
	}
	if os.Getenv("CHROMEDP_NO_SANDBOX") == "1" {
		allocOpts = append(allocOpts,
			chromedp.Flag("no-sandbox", true),
			chromedp.Flag("disable-dev-shm-usage", true),
		)
	}
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), allocOpts...)
	defer allocCancel()
	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()

	t0 := time.Now()
	if err := chromedp.Run(ctx,
		chromedp.Navigate(*pg),
		chromedp.WaitReady("body", chromedp.ByQuery),
		chromedp.WaitReady(`[data-hcaptcha-widget-id]`, chromedp.ByQuery),
		waitHCaptchaReady(),
	); err != nil {
		log.Fatalf("warm: %v", err)
	}
	fmt.Printf("warm: %s\n", time.Since(t0).Round(time.Millisecond))

	var sitekey string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`(() => {
		// The checkbox iframe fragment lacks sitekey; the challenge iframe's
		// fragment carries it. Scan every iframe's URL (fragment first, then query).
		const grab = (u) => {
			try {
				const s = String(u);
				const hash = s.split('#')[1] || '';
				for (const kv of hash.split('&')) {
					const i = kv.indexOf('=');
					if (i > 0 && kv.slice(0, i) === 'sitekey') return decodeURIComponent(kv.slice(i + 1));
				}
				return new URL(s).searchParams.get('sitekey') || '';
			} catch (e) { return ''; }
		};
		for (const f of document.querySelectorAll('iframe')) {
			const k = grab(f.src);
			if (k) return k;
		}
		return '';
	})()`, &sitekey)); err != nil || sitekey == "" {
		var diag string
		_ = chromedp.Run(ctx, chromedp.Evaluate(`(() => {
			const out = [];
			document.querySelectorAll('iframe').forEach(f => out.push('iframe: ' + String(f.src)));
			try {
				const nd = JSON.stringify(window.__NEXT_DATA__).match(/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/g);
				out.push('next_data uuids: ' + JSON.stringify(nd && nd.filter((v,i,a) => a.indexOf(v) === i)));
			} catch (e) { out.push('no next data'); }
			return out.join('\n');
		})()`, &diag))
		fmt.Println(diag)
		log.Fatalf("sitekey not found")
	}
	fmt.Printf("sitekey: %s... (%d chars)\n", sitekey[:min(8, len(sitekey))], len(sitekey))

	var primaryIDs []string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`Array.from(document.querySelectorAll('[data-hcaptcha-widget-id]')).map(e => e.getAttribute('data-hcaptcha-widget-id'))`, &primaryIDs)); err != nil {
		log.Fatalf("primary ids: %v", err)
	}
	fmt.Printf("primary widgets: %v\n", primaryIDs)

	// Render n-1 extra invisible widgets.
	renderJS := fmt.Sprintf(`((sitekey, count) => {
		const out = [];
		for (let i = 0; i < count; i++) {
			try {
				const d = document.createElement('div');
				d.style.display = 'none';
				document.body.appendChild(d);
				out.push(String(hcaptcha.render(d, { sitekey: sitekey, size: 'invisible' })));
			} catch (e) { out.push('ERR:' + e.message); }
		}
		return out;
	})(%q, %d)`, sitekey, *n-1)
	var rendered []string
	if err := chromedp.Run(ctx, chromedp.Evaluate(renderJS, &rendered)); err != nil {
		log.Fatalf("render: %v", err)
	}
	fmt.Printf("render(%d): %v\n", *n-1, rendered)

	time.Sleep(2 * time.Second) // let extra widget frames mount

	var allIDs []string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`Array.from(document.querySelectorAll('[data-hcaptcha-widget-id]')).map(e => e.getAttribute('data-hcaptcha-widget-id'))`, &allIDs)); err != nil {
		log.Fatalf("all ids: %v", err)
	}
	fmt.Printf("widgets in DOM after render: %d %v\n", len(allIDs), allIDs)
	if len(allIDs) < 2 {
		log.Fatalf("multi-widget NOT viable (only %d widget(s))", len(allIDs))
	}

	for round := 0; round < *rounds; round++ {
		batchStart := time.Now()
		toks := batchExecute(ctx, allIDs)
		fmt.Printf("round %d: %d/%d tokens in %s lens=%v\n",
			round+1, len(toks), len(allIDs), time.Since(batchStart).Round(time.Millisecond), tokenLens(toks))

		if *verify && len(toks) > 0 {
			verifyToken(ctx, toks[0])
		}

		singleStart := time.Now()
		stok := singleExecute(ctx, primaryIDs[0])
		fmt.Printf("round %d single: ok=%t len=%d in %s\n",
			round+1, stok != "", len(stok), time.Since(singleStart).Round(time.Millisecond))
	}

	select {
	case <-ctx.Done():
		log.Printf("ctx done: %v", ctx.Err())
	default:
	}
	fmt.Println("done")
}

// batchExecute fires hcaptcha.execute on every widget id concurrently and polls
// each response independently. Returns distinct non-empty fresh tokens.
func batchExecute(ctx context.Context, ids []string) []string {
	fire := fmt.Sprintf(`((ids) => {
		if (typeof hcaptcha === 'undefined') return false;
		ids.forEach(id => { try { hcaptcha.execute(String(id), { async: true }); } catch (e) {} });
		return true;
	})(%s)`, jsStringList(ids))
	var fired bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(fire, &fired)); err != nil || !fired {
		fmt.Printf("  fire failed: %v fired=%v\n", err, fired)
		return nil
	}

	deadline := time.Now().Add(15 * time.Second)
	got := map[string]bool{}
	for time.Now().Before(deadline) && len(got) < len(ids) {
		read := fmt.Sprintf(`((ids) => ids.map(id => {
			let t = '';
			try { const r = typeof hcaptcha.getResponse === 'function' ? hcaptcha.getResponse(String(id)) : ''; if (typeof r === 'string') t = r; } catch (e) {}
			if (!t) {
				const el = document.querySelector('[data-hcaptcha-widget-id="'+id+'"]');
				if (el) t = el.getAttribute('data-hcaptcha-response') || '';
			}
			return t;
		}))(%s)`, jsStringList(ids))
		var toks []string
		if err := chromedp.Run(ctx, chromedp.Evaluate(read, &toks)); err != nil {
			fmt.Printf("  read err: %v\n", err)
			return nil
		}
		for _, t := range toks {
			if len(t) >= 100 { // real hCaptcha tokens ~1.9KB; short values are widget stubs
				got[t] = true
			}
		}
		if err := chromedp.Sleep(50 * time.Millisecond).Do(ctx); err != nil {
			break
		}
	}
	out := make([]string, 0, len(got))
	for t := range got {
		out = append(out, t)
	}
	return out
}

// singleExecute mirrors the production sticky path on one widget id.
func singleExecute(ctx context.Context, id string) string {
	prev := ""
	readAttr := fmt.Sprintf(`(() => { const el = document.querySelector('[data-hcaptcha-widget-id="%s"]'); return el ? (el.getAttribute('data-hcaptcha-response')||'') : ''; })()`, id)
	_ = chromedp.Run(ctx, chromedp.Evaluate(readAttr, &prev))
	fire := fmt.Sprintf(`(() => { try { hcaptcha.execute("%s", { async: true }); } catch(e){} return 1; })()`, id)
	_ = chromedp.Run(ctx, chromedp.Evaluate(fire, nil))
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var tok string
		_ = chromedp.Run(ctx, chromedp.Evaluate(readAttr, &tok))
		if tok != "" && tok != prev {
			return tok
		}
		_ = chromedp.Sleep(50 * time.Millisecond).Do(ctx)
	}
	return ""
}

func waitHCaptchaReady() chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			var ready bool
			if err := chromedp.Evaluate(`typeof hcaptcha !== 'undefined'`, &ready).Do(ctx); err != nil {
				return err
			}
			if ready {
				return nil
			}
			_ = chromedp.Sleep(100 * time.Millisecond).Do(ctx)
		}
		return fmt.Errorf("hcaptcha global not ready")
	})
}

func jsStringList(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = fmt.Sprintf("%q", s)
	}
	return "[" + strings.Join(q, ",") + "]"
}

// verifyToken burns one batched token against the real predict endpoint to
// prove batched tokens are accepted upstream like single-path tokens.
func verifyToken(ctx context.Context, token string) {
	const (
		endpoint   = "https://api.ngc.nvidia.com/v2/predict/models/qc69jvmznzxy/minimax-m3"
		functionID = "87ea0ddc-cff1-4bca-bf8b-3bd98a35ddd0"
	)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(
		`{"model":"minimaxai/minimax-m3","messages":[{"role":"user","content":"hi"}],"max_tokens":8,"stream":false}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("nv-function-id", functionID)
	req.Header.Set("nv-captcha-token", token)
	req.Header.Set("Origin", "https://build.nvidia.com")
	req.Header.Set("Referer", "https://build.nvidia.com/")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("  verify: HTTP err %v\n", err)
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	fmt.Printf("  verify: status=%d body=%s\n", resp.StatusCode, truncateStr(string(raw), 120))
}

func truncateStr(s string, n int) string {
	if len(s) > n {
		return s[:n] + "...(truncated)"
	}
	return s
}

func tokenLens(toks []string) []int {
	out := make([]int, len(toks))
	for i, t := range toks {
		out[i] = len(t)
	}
	return out
}

// benchPool measures the production path end-to-end: BrowserGroup + Pool with
// the given batch size. Cold fill of 6 tokens, then a drain-and-refill cycle —
// the drain mimics a burst of 6 requests emptying the buffer at once.
func benchPool(batch, maxChromes int, idleTTL time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	initial := 2
	if maxChromes > 0 {
		initial = 1
	}
	g, err := captcha.NewBrowserGroup(ctx, initial, captcha.BrowserConfig{
		Playground: "https://build.nvidia.com/minimaxai/minimax-m3/playground",
	})
	if err != nil {
		log.Fatalf("browser group: %v", err)
	}
	defer g.Close()
	if maxChromes > 0 {
		g.EnableElastic(maxChromes, idleTTL)
		fmt.Printf("bench: elastic (%d->%d chromes, idle-recycle=%s)\n", initial, maxChromes, idleTTL)
	} else {
		fmt.Println("bench: 2 chromes warmed")
	}

	workers := 2
	if maxChromes > workers {
		workers = maxChromes // blocked workers are what drive elastic growth
	}
	p := captcha.NewPool(ctx, g.Extract, captcha.PoolConfig{
		Size:         6,
		Workers:      workers,
		TTL:          90 * time.Second,
		Batch:        batch,
		BatchExtract: g.ExtractBatch,
	})
	defer p.Close()

	start := time.Now()
	deadline := start.Add(120 * time.Second)
	for p.Ready() < 6 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	fills, _, _, _, _, _ := p.Stats()
	fmt.Printf("bench: cold fill 6 tokens in %s (fills=%d)\n", time.Since(start).Round(time.Millisecond), fills)

	// Drain 6 tokens concurrently (burst of requests) and time the refill.
	var wg sync.WaitGroup
	drainStart := time.Now()
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = p.Take(ctx)
		}()
	}
	wg.Wait()
	fmt.Printf("bench: drained 6 tokens in %s\n", time.Since(drainStart).Round(time.Millisecond))

	refillStart := time.Now()
	deadline = refillStart.Add(120 * time.Second)
	for p.Ready() < 6 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	fillsAfter, takes, _, _, _, _ := p.Stats()
	fmt.Printf("bench: refill 6 tokens in %s (fills=%d takes=%d)\n",
		time.Since(refillStart).Round(time.Millisecond), fillsAfter, takes)

	// Elastic smoke tail: idle past the recycle TTL and watch the shrinker
	// walk back down to the floor of 1.
	if maxChromes > 0 && idleTTL > 0 {
		fmt.Printf("bench: idling to observe shrink (idle-recycle=%s)…\n", idleTTL)
		deadline = time.Now().Add(idleTTL + 30*time.Second)
		for g.Len() > 1 && time.Now().Before(deadline) {
			time.Sleep(250 * time.Millisecond)
		}
		fmt.Printf("bench: after idle chromes=%d (want 1)\n", g.Len())
	}
}
