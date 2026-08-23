// Command probehard is a one-off probe of the hardcoded registry: for every
// models.Models entry, fetch the playground page and check the pageIsText
// signal (full modelCapability object). Output lines "id|status|isText" —
// isText=false marks a retired / non-chat page to prune from registry.go.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"sync"
	"time"

	"glm52-nvidia/internal/models"
)

var capRE = regexp.MustCompile(`\\"functionCalling\\":(true|false),\\"structuredOutput\\":(true|false),\\"reasoning\\":(true|false)`)

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
			info := models.Models[id]
			url := fmt.Sprintf("https://build.nvidia.com/%s/%s", info.Namespace, info.Slug)
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
			text := false
			if status == 200 {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				text = capRE.Match(body)
			}
			rows[i] = row{id, fmt.Sprint(status), text}
		}(i, id)
	}
	wg.Wait()

	for _, r := range rows {
		fmt.Printf("%s|%s|%v\n", r.id, r.status, r.text)
	}
}
