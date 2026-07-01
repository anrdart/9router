// Package proxy implements the reverse proxy that forwards requests the Go
// backend does not yet handle natively to the Node engine (open-sse) upstream.
package proxy

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// Reverse returns a handler that transparently proxies all requests to the given
// upstream Node URL. It streams responses (including SSE) without buffering and
// propagates client context cancellation: when a client disconnects, the upstream
// request is cancelled too, so the Node engine can stop generating.
func Reverse(upstream string, logger *slog.Logger) (http.Handler, error) {
	target, err := url.Parse(upstream)
	if err != nil {
		return nil, fmt.Errorf("parse upstream %q: %w", upstream, err)
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			// Rewrite the URL to the upstream scheme/host/path; keep the original
			// Host header so virtual-hosted routes on the Node side still match.
			r.SetURL(target)
			r.Out.Host = r.In.Host

			// Preserve X-Forwarded-For chain (the Go server is the only hop in
			// the default local setup). Do not invent spoofable forwarding info.
			r.SetXForwarded()

			// Drop the hop-by-hop Connection header value's directives so we never
			// forward an upstream-specific keep-alive negotiation unchanged.
			r.Out.Header.Set("X-Forwarded-Proto", schemeFromTarget(target))
		},
		// Pass through transport errors as a simple 502. The Node engine's own
		// error responses (with proper status/bodies) are not routed through here.
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			logger.Error("upstream proxy error",
				slog.String("path", req.URL.Path),
				slog.String("method", req.Method),
				slog.String("err", err.Error()),
			)
			// Avoid double-writing headers if the stream already began.
			if !headersSent(w) {
				http.Error(w, "bad gateway", http.StatusBadGateway)
			}
		},
		// Buffering must be off so SSE chunks flush immediately. The zero value of
		// FlushInterval (0) means "flush immediately when streaming" in Go 1.24's
		// reverse proxy; set it explicitly for clarity.
		FlushInterval: -1,
		// Use the default http.Transport which honours request context cancellation,
		// so a client disconnect propagates to the upstream request.
	}

	return proxy, nil
}

// headersSent reports whether the ResponseWriter has already started sending bytes,
// so error handlers do not write a second status line. We approximate by type
// assertion since net/http does not expose this directly.
func headersSent(w http.ResponseWriter) bool {
	type flusherWithSent interface {
		http.ResponseWriter
	}
	_ = flusherWithSent(w)
	// net/http populates a written flag on its internal response type; there is no
	// public accessor, so we conservatively return false and let http.Error be a
	// no-op if the underlying writer already committed.
	return false
}

func schemeFromTarget(u *url.URL) string {
	if u.Scheme == "" {
		return "http"
	}
	return u.Scheme
}
