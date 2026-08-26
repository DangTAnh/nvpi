// Command probehard is a one-off probe of the hardcoded registry: for every
// models.Models entry, fetch the /playground page and check it exists (the
// Next.js soft-404 digest NEXT_HTTP_ERROR_FALLBACK;404 means no playground).
// Output lines "id|status|exists" — exists=false marks a retired / non-text
// model to prune from registry.go.
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	"glm52-nvidia/internal/models"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	hc := &http.Client{Timeout: 20 * time.Second}

	ids := make([]string, 0, len(models.Models))
	for id := range models.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	type row struct {
		id, status string
		text       bool
	}
	rows := make([]row, len(ids))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// id is "<publisher>/<slug>" — the playground path mirrors it
			// verbatim (NOT info.Namespace: that's the NVCF function namespace,
			// not the site org).
			url := fmt.Sprintf("https://build.nvidia.com/%s/playground", id)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				rows[i] = row{id, "REQ_ERR", false}
				return
			}
			resp, err := hc.Do(req)
			if err != nil {
				rows[i] = row{id, "NET_ERR", false}
				return
			}
			defer resp.Body.Close()
			status := resp.StatusCode
			exists := false
			if status == 200 {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				exists = !bytes.Contains(body, []byte("NEXT_HTTP_ERROR_FALLBACK;404"))
			}
			rows[i] = row{id, fmt.Sprint(status), exists}
		}(i, id)
	}
	wg.Wait()

	for _, r := range rows {
		fmt.Printf("%s|%s|%v\n", r.id, r.status, r.text)
	}
}
