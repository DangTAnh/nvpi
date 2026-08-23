// Command pocharness verifies the harness-page mint end to end:
// sitekey scraped over plain HTTP, tokens minted on a harness page at the
// playground's origin, then the first token POSTed to the real predict
// endpoint — HTTP 200 proves NVIDIA's siteverify accepts it.
//
//	go run ./cmd/pocharness [-model=deepseek-ai/deepseek-v4-flash-0731] [-n 2]
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"

	"glm52-nvidia/internal/captcha"
	"glm52-nvidia/internal/models"
)

func main() {
	model := flag.String("model", "deepseek-ai/deepseek-v4-flash-0731", "registry model id")
	n := flag.Int("n", 2, "widgets to render/mint")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	info, err := models.Lookup(*model)
	if err != nil {
		log.Fatalf("lookup: %v", err)
	}
	pageURL := captcha.PlaygroundURL(*model)
	log.Printf("model=%s page=%s", *model, pageURL)

	t0 := time.Now()
	skCtx, skCancel := context.WithTimeout(ctx, 90*time.Second)
	defer skCancel()
	sk, err := captcha.FetchSitekeyHTTP(skCtx, nil, pageURL)
	if err != nil {
		log.Fatalf("sitekey: %v", err)
	}
	log.Printf("sitekey=%s (plain http, %s)", sk, time.Since(t0).Round(time.Millisecond))

	t1 := time.Now()
	toks, err := captcha.ExtractHarness(ctx, captcha.BrowserConfig{}, pageURL, sk, *n)
	if err != nil {
		log.Fatalf("harness mint: %v", err)
	}
	log.Printf("minted %d/%d token(s) in %s (len[0]=%d)", len(toks), *n, time.Since(t1).Round(time.Millisecond), len(toks[0]))

	// The decisive check: does upstream accept a harness-minted token?
	body, _ := json.Marshal(map[string]any{
		"model":       *model,
		"messages":    []map[string]string{{"role": "user", "content": "Say OK."}},
		"stream":      false,
		"max_tokens":  8,
		"temperature": 0.2,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, info.PredictEndpoint(), bytes.NewReader(body))
	if err != nil {
		log.Fatalf("req: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("accept", "text/event-stream")
	req.Header.Set("nv-function-id", info.FunctionID)
	req.Header.Set("nv-captcha-token", toks[0])
	req.Header.Set("Origin", "https://build.nvidia.com")
	req.Header.Set("Referer", "https://build.nvidia.com/")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatalf("predict do: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 300)
	m, _ := resp.Body.Read(buf)
	log.Printf("PREDICT status=%d body[:300]=%q", resp.StatusCode, string(buf[:m]))
	if resp.StatusCode == 200 {
		fmt.Println("POC PASS — harness token accepted by predict endpoint")
	} else {
		fmt.Println("POC FAIL — harness token rejected")
	}
}
