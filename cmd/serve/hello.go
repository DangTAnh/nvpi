package main

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// registerAPIHello wires GET /api/hello — a minimal liveness probe under the
// API path. Kept separate from /hello in main.go; registering "/hello" here
// too would make gin panic on the duplicate route.
func registerAPIHello(r *gin.Engine) {
	r.GET("/api/hello", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "nvpi"})
	})
}
