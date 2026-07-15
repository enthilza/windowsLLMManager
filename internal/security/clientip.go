package security

import (
	"net"
	"net/http"
	"strings"
)

type ClientIPResolver struct {
	trustedProxy net.IP
}

func NewClientIPResolver(trustedProxy string) ClientIPResolver {
	return ClientIPResolver{trustedProxy: net.ParseIP(trustedProxy)}
}

func (r ClientIPResolver) Resolve(req *http.Request) string {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		host = req.RemoteAddr
	}
	raw := net.ParseIP(strings.TrimSpace(host))
	if r.trustedProxy != nil && raw != nil && raw.Equal(r.trustedProxy) {
		forwarded := net.ParseIP(strings.TrimSpace(req.Header.Get("CF-Connecting-IP")))
		if forwarded != nil {
			return forwarded.String()
		}
	}
	if raw != nil {
		return raw.String()
	}
	return host
}
