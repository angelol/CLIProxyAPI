package management

import (
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func isLoopbackIP(host string) bool {
	ip := net.ParseIP(strings.TrimSpace(host))
	return ip != nil && ip.IsLoopback()
}

// LocalWithoutKeyEnabled checks both the immutable listener configuration and
// the current config. Hot-reloading a loopback host must not unlock a listener
// that was actually started on a wildcard or LAN address.
func (h *Handler) LocalWithoutKeyEnabled() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.loopbackListener && h.cfg != nil && h.cfg.Port == h.listenerPort &&
		isLoopbackIP(h.cfg.Host) && h.cfg.RemoteManagement.AllowLocalWithoutKey &&
		!h.cfg.RemoteManagement.AllowRemote
}

// LocalWithoutKeyRequestAllowed trusts the socket peer, never forwarded client
// IP headers. Host and browser-origin checks prevent DNS rebinding and cross-site
// requests from turning a local browser into an unauthenticated management client.
func (h *Handler) LocalWithoutKeyRequestAllowed(r *http.Request) bool {
	if !h.LocalWithoutKeyEnabled() || r == nil {
		return false
	}
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !isLoopbackIP(peer) {
		return false
	}
	for _, header := range []string{"Forwarded", "X-Forwarded-For", "X-Real-IP", "X-Forwarded-Host", "X-Forwarded-Proto"} {
		if len(r.Header.Values(header)) > 0 {
			return false
		}
	}
	scheme := "http"
	// The protocol multiplexer exposes an empty TLS state even for plain HTTP.
	if r.TLS != nil && r.TLS.HandshakeComplete {
		scheme = "https"
	}
	authority, err := url.Parse("//" + r.Host)
	if err != nil || authority.User != nil || authority.Host != r.Host || authority.Path != "" || authority.RawQuery != "" || authority.Fragment != "" {
		return false
	}
	host := authority.Hostname()
	if !strings.EqualFold(host, "localhost") && !isLoopbackIP(host) {
		return false
	}
	port := authority.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	if port != strconv.Itoa(h.listenerPort) {
		return false
	}
	if site := strings.ToLower(r.Header.Get("Sec-Fetch-Site")); site != "" && site != "none" && site != "same-origin" {
		return false
	}
	for _, header := range []string{"Origin", "Referer"} {
		values := r.Header.Values(header)
		if len(values) > 1 {
			return false
		}
		if len(values) == 0 {
			continue
		}
		u, parseErr := url.Parse(values[0])
		if parseErr != nil || u.User != nil || u.Scheme != scheme || !strings.EqualFold(u.Host, r.Host) {
			return false
		}
	}
	return true
}
