// Command getsitekey prints the playground hCaptcha sitekey by watching network
// traffic — hCaptcha embeds the sitekey in its API requests (getcaptcha/checkcaptcha)
// and iframe URLs once hcaptcha.execute runs. More reliable than scraping the DOM
// (the data-sitekey attribute is empty on this site; the key is bound in JS).
//
//	go run ./cmd/getsitekey [-proxy=socks5://host:port] [-model=minimaxai/minimax-m3]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"glm52-nvidia/internal/captcha"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

func main() {
	model := flag.String("model", "minimaxai/minimax-m3", "playground model path")
	proxy := flag.String("proxy", strings.TrimSpace(os.Getenv("CHROME_PROXY")), "chrome proxy URL (optional)")
	flag.Parse()

	allocOpts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("enable-automation", false),
		chromedp.Flag("disable-gpu", true),
		chromedp.UserAgent("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"),
		chromedp.WindowSize(1280, 900),
	)
	if path := captcha.ChromeExecPath(); path != "" {
		allocOpts = append(allocOpts, chromedp.ExecPath(path))
	}
	if os.Getenv("CHROMEDP_ALLOW_IMAGES") != "1" {
		allocOpts = append(allocOpts, chromedp.Flag("blink-settings", "imagesEnabled=false"))
	}
	if *proxy != "" {
		allocOpts = append(allocOpts, chromedp.ProxyServer(*proxy))
	}

	ctx, cancel := chromedp.NewExecAllocator(context.Background(), allocOpts...)
	defer cancel()
	bc, bCancel := chromedp.NewContext(ctx, chromedp.WithErrorf(func(string, ...any) {}))
	defer bCancel()

	// Capture every hCaptcha URL that flows across the network; one of them
	// carries the sitekey as a query param.
	var mu sync.Mutex
	var seen []string
	chromedp.ListenTarget(bc, func(ev any) {
		switch e := ev.(type) {
		case *network.EventResponseReceived:
			u := e.Response.URL
			if strings.Contains(u, "hcaptcha") {
				mu.Lock()
				seen = append(seen, u)
				mu.Unlock()
			}
		case *network.EventRequestWillBeSent:
			u := e.Request.URL
			if strings.Contains(u, "hcaptcha") {
				mu.Lock()
				seen = append(seen, u)
				mu.Unlock()
			}
		}
	})

	runCtx, rcancel := context.WithTimeout(bc, 90*time.Second)
	defer rcancel()

	url := "https://build.nvidia.com/" + *model + "/playground"

	var widgetID string
	if err := chromedp.Run(runCtx,
		network.Enable(),
		chromedp.Navigate(url),
		chromedp.WaitReady("body", chromedp.ByQuery),
		chromedp.Evaluate(`Object.defineProperty(navigator,'webdriver',{get:()=>undefined})`, nil),
		chromedp.WaitReady(`[data-hcaptcha-widget-id]`, chromedp.ByQuery),
		chromedp.Evaluate(`(() => {
			const el = document.querySelector('[data-hcaptcha-widget-id]');
			return el ? (el.getAttribute('data-hcaptcha-widget-id')||'') : '';
		})()`, &widgetID),
	); err != nil {
		fmt.Println("ERR:", err)
	}

	// Fire execute to trigger hCaptcha API calls (getcaptcha/<sitekey>).
	if widgetID != "" {
		fire := fmt.Sprintf(`(() => {
			if (typeof hcaptcha === 'undefined' || !hcaptcha.execute) return '';
			try { hcaptcha.execute(%q, {async:true}); } catch(e) {}
			return 'fired';
		})()`, widgetID)
		var fired string
		_ = chromedp.Run(runCtx,
			chromedp.Evaluate(fire, &fired),
			chromedp.Sleep(4*time.Second),
		)
	}

	fmt.Println("URL:", url)
	fmt.Println("WIDGET_ID:", widgetID)
	sitekey := extractSitekey(seen)
	fmt.Println("SITEKEY:", sitekey)
	fmt.Println("HCAPTCHA_URLS:")
	for _, u := range seen {
		fmt.Println("  ", u)
	}
}

// extractSitekey scans captured hCaptcha URLs for a ?sitekey=... param.
func extractSitekey(urls []string) string {
	for _, u := range urls {
		if i := strings.Index(u, "sitekey="); i >= 0 {
			rest := u[i+len("sitekey="):]
			if j := strings.IndexAny(rest, "&#"); j >= 0 {
				rest = rest[:j]
			}
			return rest
		}
	}
	return ""
}
