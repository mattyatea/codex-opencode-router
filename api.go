// ChatGPT API base proxy for Codex Desktop and the Codex app-server.
//
// The desktop app disables its composer when the account usage payload says
// rate_limit.allowed == false, even when the selected model is served by a
// non-OpenAI provider (for example OpenCode Go through the model router on
// 18789). Point the clients at this listener instead of chatgpt.com:
//
//   - desktop app : CODEX_API_BASE_URL=http://localhost:8000/backend-api
//   - app-server  : chatgpt_base_url = "http://127.0.0.1:8000/backend-api/"
//
// Every request is forwarded to https://chatgpt.com verbatim; only the quota
// payloads (/api/codex/usage and /wham/usage, SSE included) are rewritten so
// the UI does not hard-block the composer.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	apiListenDefault = "127.0.0.1:8000,[::1]:8000"
	apiMaxBodyBytes  = 8 << 20
)

type apiContextKey int

const apiPathKey apiContextKey = 0

// serveAPI starts the ChatGPT API base listeners. It is called from main
// alongside the model router so both share one process.
func serveAPI() {
	handler := newAPIProxy()
	for _, address := range strings.Split(envOr("ROUTER_API_LISTEN", apiListenDefault), ",") {
		address = strings.TrimSpace(address)
		if address == "" {
			continue
		}
		go func(listen string) {
			log.Printf("codex-go-router api listening on %s -> %s", listen, chatgptOrigin)
			if err := http.ListenAndServe(listen, handler); err != nil {
				log.Fatalf("api listen %s: %v", listen, err)
			}
		}(address)
	}
}

func newAPIProxy() *httputil.ReverseProxy {
	upstream, err := url.Parse(chatgptOrigin)
	if err != nil {
		log.Fatalf("invalid chatgpt origin: %v", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 60 * time.Second

	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			path := pr.In.URL.Path
			pr.Out.URL.Scheme = upstream.Scheme
			pr.Out.URL.Host = upstream.Host
			pr.Out.URL.Path = path
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Host = upstream.Host
			// Let the transport negotiate and transparently decompress.
			pr.Out.Header.Del("Accept-Encoding")
			pr.Out = pr.Out.WithContext(context.WithValue(pr.Out.Context(), apiPathKey, path))
		},
		Transport:     transport,
		FlushInterval: 100 * time.Millisecond,
		ModifyResponse: func(resp *http.Response) error {
			path, _ := resp.Request.Context().Value(apiPathKey).(string)
			log.Printf("%s %s -> %d", resp.Request.Method, path, resp.StatusCode)
			return rewriteUsage(resp, path)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			path, _ := r.Context().Value(apiPathKey).(string)
			log.Printf("api proxy error %s %s: %v", r.Method, path, err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
}

// isUsagePath reports whether a backend path carries account quota data.
// Thread usage and plan history are intentionally left alone.
func isUsagePath(path string) bool {
	trimmed := strings.TrimSuffix(path, "/")
	switch {
	case strings.HasSuffix(trimmed, "/api/codex/usage"),
		strings.HasSuffix(trimmed, "/wham/usage"),
		strings.HasSuffix(trimmed, "/wham/usage/stream"):
		return true
	}
	return false
}

// rewriteUsage lifts the quota gates in a usage response, JSON or SSE. Any
// other path passes through untouched.
func rewriteUsage(resp *http.Response, path string) error {
	if resp.StatusCode != http.StatusOK || !isUsagePath(path) {
		return nil
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		resp.Body = newSSERewriter(resp.Body)
		resp.ContentLength = -1
		resp.Header.Del("Content-Length")
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, apiMaxBodyBytes))
	resp.Body.Close()
	if err != nil {
		resp.Body = io.NopCloser(bytes.NewReader(nil))
		return err
	}
	rewritten := body
	var value any
	if json.Unmarshal(body, &value) == nil {
		sanitizeUsage(value)
		if encoded, err := json.Marshal(value); err == nil {
			rewritten = encoded
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(rewritten))
	resp.ContentLength = int64(len(rewritten))
	resp.Header.Set("Content-Length", strconv.Itoa(len(rewritten)))
	resp.Header.Del("Content-Encoding")
	return nil
}

// sanitizeUsage walks the payload and lifts every quota gate it finds. It
// understands both the backend snake_case schema and the app-server
// camelCase projection.
func sanitizeUsage(value any) {
	switch node := value.(type) {
	case map[string]any:
		for key, child := range node {
			switch normalizeKey(key) {
			case "ratelimit", "ratelimits":
				if limit, ok := child.(map[string]any); ok {
					fixLimit(limit)
				}
				sanitizeUsage(child)
			case "ratelimitsbylimitid":
				if buckets, ok := child.(map[string]any); ok {
					for _, bucket := range buckets {
						if limit, ok := bucket.(map[string]any); ok {
							fixLimit(limit)
						}
					}
				}
				sanitizeUsage(child)
			case "ratelimitupsell", "ratelimitwarning", "ratelimitreachedtype", "modelpickerupsell":
				node[key] = nil
			case "spendcontrol":
				if control, ok := child.(map[string]any); ok {
					control["reached"] = false
				}
				sanitizeUsage(child)
			case "ordinaryusageallowed":
				node[key] = true
			default:
				sanitizeUsage(child)
			}
		}
	case []any:
		for _, child := range node {
			sanitizeUsage(child)
		}
	}
}

// normalizeKey folds snake_case and camelCase field names so both the backend
// and the app-server projections are recognised.
func normalizeKey(key string) string {
	return strings.ToLower(strings.ReplaceAll(key, "_", ""))
}

func fixLimit(limit map[string]any) {
	limit["allowed"] = true
	if _, ok := limit["limit_reached"]; ok {
		limit["limit_reached"] = false
	}
	if _, ok := limit["limitReached"]; ok {
		limit["limitReached"] = false
	}
	for _, key := range []string{"primary_window", "secondary_window", "primary", "secondary", "primaryWindow", "secondaryWindow"} {
		if window, ok := limit[key].(map[string]any); ok {
			fixWindow(window)
		}
	}
}

func fixWindow(window map[string]any) {
	for _, key := range []string{"used_percent", "usedPercent"} {
		if _, ok := window[key]; ok {
			window[key] = 0
		}
	}
	for _, key := range []string{"remaining_percent", "remainingPercent"} {
		if _, ok := window[key]; ok {
			window[key] = 100
		}
	}
}

// newSSERewriter rewrites each data: line of an event stream (usage.snapshot
// events included) while keeping the stream streaming.
func newSSERewriter(body io.ReadCloser) io.ReadCloser {
	reader, writer := io.Pipe()
	go func() {
		defer writer.Close()
		defer body.Close()
		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 64<<10), maxSSELineBytes)
		for scanner.Scan() {
			line := scanner.Bytes()
			if bytes.HasPrefix(line, []byte("data:")) {
				payload := bytes.TrimSpace(line[len("data:"):])
				var value any
				if json.Unmarshal(payload, &value) == nil {
					sanitizeUsage(value)
					if encoded, err := json.Marshal(value); err == nil {
						line = append([]byte("data: "), encoded...)
					}
				}
			}
			if _, err := writer.Write(append(append([]byte{}, line...), '\n')); err != nil {
				return
			}
		}
	}()
	return reader
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
