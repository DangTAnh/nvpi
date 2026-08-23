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
	url2 "net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"glm52-nvidia/internal/captcha"
	"glm52-nvidia/internal/models"
)

func main() {
	pg := flag.String("url", "https://build.nvidia.com/minimaxai/minimax-m3/playground", "playground URL")
	n := flag.Int("n", 4, "total widgets to try (1 primary + n-1 rendered)")
	rounds := flag.Int("rounds", 3, "batch execute rounds")
	verify := flag.Bool("verify", false, "POST first token of each round to the predict endpoint")
	bench := flag.Int("bench-pool", 0, "run a Pool+BrowserGroup throughput bench with this batch size (skips the render probe)")
	chromesMax := flag.Int("chromes-max", 0, "with -bench-pool: start at 1 Chrome and scale elastically up to this under pressure (0 = fixed 2)")
	idleRecycle := flag.Duration("idle-recycle", 0, "with -chromes-max: close Chromes idle longer than this")
	benchPages := flag.Int("bench-pages", 0, "time cold NewBrowser+Extract for every models.Models entry, this many rounds each (sequential, median reported)")
	netlog := flag.String("netlog", "", "navigate this URL once and dump third-party request hosts seen during the nav")
	flag.Parse()

	if *benchPages > 0 {
		benchPagesMode(*benchPages)
		return
	}
	if *netlog != "" {
		netlogMode(*netlog)
		return
	}
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
		log.Fatalf("sitekey not found")
	}
	fmt.Printf("sitekey: %s... (%d chars)\n", sitekey[:min(8, len(sitekey))], len(sitekey))

	var primaryIDs []string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`Array.from(document.querySelectorAll('[data-hcaptcha-widget-id]')).map(e => e.getAttribute('data-hcaptcha-widget-id'))`, &primaryIDs)); err != nil {
		log.Fatalf("primary ids: %v", err)
	}
	fmt.Printf("primary widgets: %v\n", primaryIDs)

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
	toksOut := make([]string, 0, len(ids))
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
			fmt.Printf("  poll failed: %v\n", err)
			return toksOut
		}
		for _, t := range toks {
			if len(t) >= minTokenLen && !got[t] {
				got[t] = true
				toksOut = append(toksOut, t)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return toksOut
}

func singleExecute(ctx context.Context, id string) string {
	fire := fmt.Sprintf(`((id) => {
		if (typeof hcaptcha === 'undefined') return '';
		try { hcaptcha.execute(String(id), { async: true }); } catch (e) {}
		const el = document.querySelector('[data-hcaptcha-widget-id="'+id+'"]');
		return el ? (el.getAttribute('data-hcaptcha-response') || '') : '';
	})("%s")`, id)
	var prev string
	if err := chromedp.Run(ctx, chromedp.Evaluate(fire, &prev)); err != nil {
		return ""
	}
	if len(prev) >= minTokenLen {
		return prev
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var tok string
		read := fmt.Sprintf(`(() => {
			const el = document.querySelector('[data-hcaptcha-widget-id]="%s"'.replace(/"/g,'"'));
			const el2 = document.querySelector('[data-hcaptcha-widget-id="%s"]');
			return el2 ? (el2.getAttribute('data-hcaptcha-response') || '') : '';
		})()`, id, id)
		if err := chromedp.Run(ctx, chromedp.Evaluate(read, &tok)); err != nil {
			return ""
		}
		if len(tok) >= minTokenLen && tok != prev {
			return tok
		}
		time.Sleep(50 * time.Millisecond)
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
			if err := chromedp.Sleep(100 * time.Millisecond).Do(ctx); err != nil {
				return err
			}
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

// minTokenLen separates real hCaptcha tokens (~1.9KB) from short stub values.
const minTokenLen = 100

// verifyToken burns one token against the real predict endpoint to prove it is
// accepted upstream like production tokens.
func verifyToken(ctx context.Context, token string) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.ngc.nvidia.com/v2/predict/models/qc69jvmznzxy/minimax-m3",
		strings.NewReader(`{"model":"minimaxai/minimax-m3","messages":[{"role":"user","content":"hi"}],"max_tokens":8,"stream":false}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("nv-function-id", "87ea0ddc-cff1-4bca-bf8b-3bd98a35ddd0")
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
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

func tokenLens(toks []string) []int {
	out := make([]int, len(toks))
	for i, t := range toks {
		out[i] = len(t)
	}
	return out
}

func pub(id string) string {
	i := strings.IndexByte(id, '/')
	if i < 0 {
		return id
	}
	return id[:i]
}

func slug(id string) string {
	i := strings.IndexByte(id, '/')
	if i < 0 {
		return id
	}
	return id[i+1:]
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

	if maxChromes > 0 && idleTTL > 0 {
		fmt.Printf("bench: idling to observe shrink (idle-recycle=%s)…\n", idleTTL)
		deadline = time.Now().Add(idleTTL + 30*time.Second)
		for g.Len() > 1 && time.Now().Before(deadline) {
			time.Sleep(250 * time.Millisecond)
		}
		fmt.Printf("bench: after idle chromes=%d (want 1)\n", g.Len())
	}
}

// pageResult accumulates one candidate's cold-path timings.
type pageResult struct {
	id       string
	navs     []time.Duration
	execs    []time.Duration
	errs     int
	verifyOK bool
}

// benchPagesMode times the production cold path for every model playground:
// NewBrowser (Chrome launch + warm nav to widget-ready — what serve's startup,
// the recycle rung and re-navigate pay) plus one sticky Extract. One Chrome at
// a time keeps bandwidth noise out of the comparison; medians over rounds.
// Each candidate's first token is verified against the predict endpoint once.
func benchPagesMode(rounds int) {
	results := map[string]*pageResult{}

	var ids []string
	for id := range models.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	fmt.Printf("bench-pages: %d candidates x %d cold rounds, sequential\n", len(ids), rounds)

	hc := &http.Client{Timeout: 20 * time.Second}
	verified := 0

	for _, id := range ids {
		res := &pageResult{id: id}
		results[id] = res
		url := fmt.Sprintf("https://build.nvidia.com/%s/%s/playground", pub(id), slug(id))
		for r := 0; r < rounds; r++ {
			t0 := time.Now()
			b, err := captcha.NewBrowser(context.Background(), captcha.BrowserConfig{Playground: url})
			nav := time.Since(t0)
			if err != nil {
				fmt.Printf("%-55s round %d: NAV FAIL %v\n", id, r+1, err)
				res.errs++
				// ponytail: a dead page (retired model) stays dead within one
				// run — don't burn the remaining rounds' 90s warm timeouts on it.
				break
			}
			res.navs = append(res.navs, nav)

			t1 := time.Now()
			tok, err := b.Extract(context.Background())
			exec := time.Since(t1)
			b.Close()
			if err != nil {
				fmt.Printf("%-55s round %d: nav=%s EXEC FAIL %v\n", id, r+1, nav.Round(time.Millisecond), err)
				res.errs++
				continue
			}
			res.execs = append(res.execs, exec)
			fmt.Printf("%-55s round %d: nav=%s exec=%s\n", id, r+1,
				nav.Round(time.Millisecond), exec.Round(time.Millisecond))

			if !res.verifyOK && postVerify(hc, tok) {
				res.verifyOK = true
				verified++
			}
		}
		time.Sleep(300 * time.Millisecond)
	}

	fmt.Printf("\nverified-tokens: %d candidates accepted by predict endpoint\n", verified)
	printPageTable(results)
}

// postVerify POSTs one minted token to the default model's predict endpoint
// and reports whether it was accepted (HTTP 200).
func postVerify(hc *http.Client, tok string) bool {
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"https://api.ngc.nvidia.com/v2/predict/models/qc69jvmznzxy/minimax-m3",
		strings.NewReader(`{"model":"minimaxai/minimax-m3","messages":[{"role":"user","content":"hi"}],"max_tokens":8,"stream":false}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("nv-function-id", "87ea0ddc-cff1-4bca-bf8b-3bd98a35ddd0")
	req.Header.Set("nv-captcha-token", tok)
	req.Header.Set("Origin", "https://build.nvidia.com")
	req.Header.Set("Referer", "https://build.nvidia.com/")
	resp, err := hc.Do(req)
	if err != nil || resp == nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200
}

func printPageTable(results map[string]*pageResult) {
	type row struct {
		id       string
		navMed   time.Duration
		execMed  time.Duration
		errs     int
		verifyOK bool
	}
	var rows []row
	for _, res := range results {
		rows = append(rows, row{
			id:       res.id,
			navMed:   medianOf(res.navs),
			execMed:  medianOf(res.execs),
			errs:     res.errs,
			verifyOK: res.verifyOK,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].navMed < rows[j].navMed })
	fmt.Printf("%-52s %10s %10s %5s %s\n", "model", "nav-med", "exec-med", "errs", "verify")
	for i, rw := range rows {
		if i >= 20 && rw.navMed > rows[4].navMed {
			continue // print top block + any outliers compactly
		}
		v := ""
		if rw.verifyOK {
			v = "OK"
		}
		fmt.Printf("%-52s %10s %10s %5d %s\n", rw.id,
			rw.navMed.Round(time.Millisecond), rw.execMed.Round(time.Millisecond), rw.errs, v)
	}
}

// medianOf returns the median duration.
func medianOf(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

// netlogMode navigates one playground URL and aggregates third-party request
// hosts seen on the wire during the warm — input for extra blocked-asset
// patterns in internal/captcha/extract.go.
func netlogMode(url string) {
	allocOpts := captcha.ChromeAllocatorOptions()
	if path := os.Getenv("CHROME_PATH"); path != "" {
		allocOpts = append(allocOpts, chromedp.ExecPath(path))
	}
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), allocOpts...)
	defer allocCancel()
	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()

	hosts := map[string]int{}
	chromedp.ListenBrowser(ctx, func(ev interface{}) {
		if req, ok := ev.(*network.EventRequestWillBeSent); ok {
			if u, err := url2.Parse(req.Request.URL); err == nil {
				hosts[u.Host]++
			}
		}
	})

	if err := chromedp.Run(ctx,
		chromedp.Navigate(url),
		chromedp.WaitReady("body", chromedp.ByQuery),
		chromedp.WaitReady(`[data-hcaptcha-widget-id]`, chromedp.ByQuery),
		waitHCaptchaReady(),
	); err != nil {
		log.Fatalf("netlog warm: %v", err)
	}
	time.Sleep(2 * time.Second)

	fmt.Println("hosts by request count:")
	type hc struct {
		host  string
		count int
	}
	var list []hc
	total := 0
	for h, c := range hosts {
		list = append(list, hc{h, c})
		total += c
	}
	sort.Slice(list, func(i, j int) bool { return list[i].count > list[j].count })
	for _, e := range list {
		tag := ""
		if e.host != "build.nvidia.com" && !strings.HasPrefix(e.host, "assets") {
			tag = "  <- third-party"
		}
		fmt.Printf("%5d %-60s%s\n", e.count, e.host, tag)
	}
	fmt.Printf("total requests: %d\n", total)
}
