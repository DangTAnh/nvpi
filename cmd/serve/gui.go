package main

import (
	_ "embed"
	"net/http"
	"sort"
	"time"

	"glm52-nvidia/internal/captcha"
	"glm52-nvidia/internal/models"

	"github.com/gin-gonic/gin"
)

//go:embed web/index.html
var indexHTML []byte

// WebDeps carries everything the dashboard needs, captured once at wire time.
// Pool/Browser are nil when -auto is off; handlers are nil-safe and report
// honestly that the pool is disabled instead of inventing zeros-as-live-data.
type WebDeps struct {
	Version string
	Start   time.Time
	Auto    bool

	Pool    *captcha.Pool         // nil when -auto is off
	Browser *captcha.BrowserGroup // nil when -auto is off

	ChromesMax  int
	MaxInflight int
	CoalesceMs  int
	PoolSize    int
	PoolBatch   int
}

// registerWeb wires the minimal web GUI: GET / (embedded single-file page),
// GET /api/status, GET /api/models. Called from the engine configurator
// closure in main.go — the only place the gin engine exists.
//
// GET / cannot be a route: CLIProxyAPI registers its own JSON handler on "/"
// during setupRoutes, and a duplicate panics. Instead we install middleware
// (same pattern as the /healthz enrichment in main.go): it attaches to every
// route the SDK registers afterwards AND to unmatched paths (gin rebuilds its
// 404 chain on Use), where Abort short-circuits before both.
func registerWeb(r *gin.Engine, d WebDeps) {
	r.GET("/api/status", d.handleStatus)
	r.GET("/api/models", d.handleModels)
	r.Use(func(c *gin.Context) {
		if c.Request.URL.Path == "/" && c.Request.Method == http.MethodGet {
			c.Data(http.StatusOK, "text/html; charset=utf-8", indexHTML)
			c.Abort()
			return
		}
		c.Next()
	})
}

// handleStatus aggregates only metrics that already exist: pool stats, Chrome
// group counters, flag values, registry size. Nothing is fabricated — with
// -auto off, pool.enabled=false and the live counters stay zero.
func (d WebDeps) handleStatus(c *gin.Context) {
	pool := gin.H{
		"enabled":      false,
		"ready":        0,
		"fills":        0,
		"takes":        0,
		"errors":       0,
		"expired":      0,
		"stale_leases": 0,
		"ttl_ms":       0,
	}
	if d.Pool != nil {
		fills, takes, errs, expired, staleLeases, ttl := d.Pool.Stats()
		pool = gin.H{
			"enabled":      true,
			"ready":        d.Pool.Ready(),
			"fills":        fills,
			"takes":        takes,
			"errors":       errs,
			"expired":      expired,
			"stale_leases": staleLeases,
			"ttl_ms":       ttl.Milliseconds(),
		}
	}
	chromes := gin.H{"live": 0, "max": d.ChromesMax, "nav_count": 0, "sticky_count": 0}
	if d.Browser != nil {
		chromes["live"] = d.Browser.Len()
		chromes["nav_count"] = d.Browser.NavCount()
		chromes["sticky_count"] = d.Browser.StickyCount()
	}
	c.JSON(http.StatusOK, gin.H{
		"version":        d.Version,
		"uptime_seconds": int64(time.Since(d.Start).Seconds()),
		"pool":           pool,
		"chromes":        chromes,
		"limits": gin.H{
			"max_inflight": d.MaxInflight,
			"coalesce_ms":  d.CoalesceMs,
			"pool_size":    d.PoolSize,
			"pool_batch":   d.PoolBatch,
		},
		"models": len(models.Models),
	})
}

// webModel is one row of GET /api/models.
type webModel struct {
	ID            string `json:"id"`
	ContextLength int    `json:"context_length"`
	Reasoning     bool   `json:"reasoning"`
	ToolCalling   bool   `json:"tool_calling"`
}

// handleModels dumps the live registry sorted by id. Reading models.Models
// directly is safe: it is written only by the startup refresh, before HTTP
// serving begins (single-writer startup).
func (d WebDeps) handleModels(c *gin.Context) {
	ids := make([]string, 0, len(models.Models))
	for id := range models.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]webModel, 0, len(ids))
	for _, id := range ids {
		info := models.Models[id]
		m := webModel{ID: id, ContextLength: info.ContextLength}
		if info.Capability != nil {
			m.Reasoning = info.Capability.Reasoning
			m.ToolCalling = info.Capability.ToolCalling
		}
		out = append(out, m)
	}
	c.JSON(http.StatusOK, out)
}
