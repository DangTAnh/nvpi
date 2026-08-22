// Command captcha_cross_test verifies whether a captcha token minted on one
// playground model is accepted by another model's predict endpoint.
//
// Usage:
//
//	go run ./cmd/captcha_cross_test
//
// Important: hCaptcha tokens are SINGLE-USE. This test mints a fresh token per
// target via browser.Extract() (which fires hcaptcha.execute on the sticky
// playground tab — fast path ~300ms after warm). Each target gets its own
// token so a 400 "Token is invalid" really means model-scoped, not "already
// burned by the previous call".
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"glm52-nvidia/internal/captcha"
	"glm52-nvidia/internal/models"
)

func main() {
	playground := flag.String("playground", "minimaxai/minimax-m3", "playground model to use for hCaptcha minting (sticky tab)")
	targets := flag.String("targets", "minimaxai/minimax-m3,poolside/laguna-xs-2.1,google/gemma-4-31b-it", "comma-separated models to predict against (each gets a fresh token)")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	fmt.Printf("=== Step 1: warm Chrome on playground=%s ===\n", *playground)
	browser, err := captcha.NewBrowser(ctx, captcha.BrowserConfig{})
	if err != nil {
		fmt.Printf("ERR NewBrowser: %v\n", err)
		return
	}
	defer browser.Close()

	fmt.Println("\n=== Step 2: mint fresh token per target, predict ===")
	for _, target := range strings.Split(*targets, ",") {
		target = strings.TrimSpace(target)
		if target == "" {
			continue
		}
		token, err := browser.Extract(ctx)
		if err != nil {
			fmt.Printf("[err] %s: extract token failed: %v\n", target, err)
			continue
		}
		fmt.Printf("[mint] %s tokenLen=%d prefix=%s...\n", target, len(token), token[:min(20, len(token))])
		callPredict(ctx, target, token)
	}
}

func callPredict(ctx context.Context, model, token string) {
	info, err := models.Lookup(model)
	if err != nil {
		fmt.Printf("[err] %s: lookup: %v\n", model, err)
		return
	}
	url := info.PredictEndpoint()
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"max_tokens":8,"stream":false}`, model)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("nv-function-id", info.FunctionID)
	req.Header.Set("nv-captcha-token", token)
	req.Header.Set("Origin", "https://build.nvidia.com")
	req.Header.Set("Referer", "https://build.nvidia.com/")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("[err] %s: HTTP %v\n", model, err)
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
	fmt.Printf("[%s] %s -> status=%d body=%s\n",
		classify(resp.StatusCode), model, resp.StatusCode, truncate(string(raw), 250))
}

func classify(status int) string {
	switch {
	case status == 200:
		return " ok "
	case status == 400 || status == 401 || status == 403:
		return "rej "
	default:
		return "?? "
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "...(truncated)"
	}
	return s
}
