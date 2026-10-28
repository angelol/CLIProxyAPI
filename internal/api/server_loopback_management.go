package api

import (
	"bytes"
	_ "embed"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
)

//go:embed loopback_management_bootstrap.js
var loopbackManagementBootstrap []byte

func (s *Server) serveLoopbackManagementPanel(c *gin.Context, path string) {
	c.Header("Access-Control-Allow-Origin", "")
	c.Header("Cache-Control", "no-store")
	c.Header("Content-Security-Policy", "frame-ancestors 'none'")
	c.Header("X-Frame-Options", "DENY")
	if !s.mgmt.LocalWithoutKeyRequestAllowed(c.Request) {
		c.AbortWithStatus(http.StatusForbidden)
		return
	}
	body, err := os.ReadFile(path)
	if err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	if !bytes.Contains(body, []byte("<head>")) {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	bootstrap := append([]byte("<head><script>"), loopbackManagementBootstrap...)
	bootstrap = append(bootstrap, []byte("</script>")...)
	body = bytes.Replace(body, []byte("<head>"), bootstrap, 1)
	c.Data(http.StatusOK, "text/html; charset=utf-8", body)
}
