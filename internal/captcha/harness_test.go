package captcha

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSitekeyCache(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sitekey.json")
	now := time.Now()

	if got := loadCachedSitekey(p, now); got != "" {
		t.Fatalf("missing file: want empty key, got %q", got)
	}

	SaveCachedSitekey("test-key") // must not panic without HOME issues; writes real cache
	os.Remove(SitekeyCachePath()) // clean up the real-cache write above

	c := `{"sitekey":"abc","scraped_at":"` + now.Format(time.RFC3339Nano) + `"}`
	if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadCachedSitekey(p, now); got != "abc" {
		t.Fatalf("fresh cache: want abc, got %q", got)
	}
	if got := loadCachedSitekey(p, now.Add(sitekeyCacheTTL+time.Minute)); got != "" {
		t.Fatalf("expired cache: want empty key, got %q", got)
	}
	if err := os.WriteFile(p, []byte("{corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadCachedSitekey(p, now); got != "" {
		t.Fatalf("corrupt cache: want empty key, got %q", got)
	}
}
