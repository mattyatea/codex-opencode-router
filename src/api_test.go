package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"testing"
)

func TestProxyCookieDomains(t *testing.T) {
	for _, host := range []string{"localhost:8000", "127.0.0.1:8000", "[::1]:8000", "example.com:8000"} {
		in := httptest.NewRequest("POST", "http://"+host+"/backend-api/devicecheck", nil)
		pr := &httputil.ProxyRequest{In: in, Out: in.Clone(in.Context())}
		proxy := newAPIProxy()
		proxy.Rewrite(pr)
		raw := "_devicecheck=signed-value; Domain=.chatgpt.com; Path=/; Secure; HttpOnly; SameSite=None; Max-Age=3600; Partitioned"
		other := "other=value; Domain=example.com; Path=/; Secure"
		resp := &http.Response{Request: pr.Out, StatusCode: 200, Header: http.Header{"Set-Cookie": {raw, other}}}
		if err := proxy.ModifyResponse(resp); err != nil {
			t.Fatal(err)
		}
		want := strings.Replace(raw, "; Domain=.chatgpt.com", "", 1)
		if host == "example.com:8000" {
			want = raw
		}
		if got := resp.Header.Values("Set-Cookie"); len(got) != 2 || got[0] != want || got[1] != other {
			t.Fatalf("%s: cookie attributes changed unexpectedly", host)
		}
	}
}

func TestAppcastProxyRoute(t *testing.T) {
	for _, path := range []string{"/api/codex/app/appcast", "/backend-api/devicecheck", "/api/codex/app/appcast/other"} {
		in := httptest.NewRequest("GET", "http://localhost:8000"+path+"?installation_id=test&beta=true&arch=arm64", nil)
		in.Header.Set("Authorization", "Bearer test")
		in.Header.Set("Cookie", "session=test")
		pr := &httputil.ProxyRequest{In: in, Out: in.Clone(in.Context())}
		newAPIProxy().Rewrite(pr)
		want := chatgptOrigin + path
		if path == "/api/codex/app/appcast" {
			want = "https://updates.oaistatic.com/codex/app/appcast"
			if pr.Out.Header.Get("Authorization") != "" || pr.Out.Header.Get("Cookie") != "" {
				t.Fatal("credentials forwarded to update CDN")
			}
		} else if pr.Out.Header.Get("Authorization") != "Bearer test" {
			t.Fatal("backend credentials removed")
		}
		if got := pr.Out.URL.String(); got != want+"?"+in.URL.RawQuery {
			t.Fatalf("route %s: got %s, want %s", path, got, want)
		}
		if pr.Out.Host != pr.Out.URL.Host {
			t.Fatal("incorrect upstream Host")
		}
	}
}

func TestIsUsagePath(t *testing.T) {
	cases := map[string]bool{
		"/backend-api/api/codex/usage":                    true,
		"/backend-api/wham/usage":                         true,
		"/api/wham/usage":                                 true,
		"/wham/usage/stream":                              true,
		"/backend-api/wham/usage/":                        true,
		"/backend-api/api/codex/usage/thread_usage/query": false,
		"/wham/usage/thread_usage/query":                  false,
		"/wham/usage/plan_limit_history":                  false,
		"/backend-api/wham/accounts/check":                false,
	}
	for path, want := range cases {
		if got := isUsagePath(path); got != want {
			t.Fatalf("isUsagePath(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestSanitizeUsageSnakeCase(t *testing.T) {
	payload := `{
	  "account_id": "acct",
	  "user_id": "user",
	  "plan_type": "plus",
	  "rate_limit": {
	    "allowed": false,
	    "limit_reached": true,
	    "primary_window": {"used_percent": 10, "window_minutes": 300, "resets_at": 1790346911},
	    "secondary_window": {"used_percent": 100, "window_minutes": 10080, "resets_at": 1790602865}
	  },
	  "additional_rate_limits": [
	    {"limit_name": "gpt-reserve", "rate_limit": {"allowed": false, "limit_reached": true}}
	  ],
	  "spend_control": {"reached": true},
	  "rate_limit_reached_type": {"type": "rate_limit_reached"},
	  "rate_limit_upsell": {"banner_type": "plus_rate_limit_reached"},
	  "rate_limit_warning": {"banner_type": "credits"}
	}`
	var value any
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		t.Fatal(err)
	}
	sanitizeUsage(value)
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	got := string(encoded)
	for _, forbidden := range []string{`"allowed":false`, `"limit_reached":true`, `"used_percent":100`, `"reached":true`, `rate_limit_reached_type":{"type"`, `"banner_type"`} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("payload still contains %s: %s", forbidden, got)
		}
	}
	for _, want := range []string{`"allowed":true`, `"limit_reached":false`, `"used_percent":0`, `"rate_limit_upsell":null`, `"rate_limit_warning":null`, `"rate_limit_reached_type":null`} {
		if !strings.Contains(got, want) {
			t.Fatalf("payload missing %s: %s", want, got)
		}
	}
}

func TestSanitizeUsageCamelCase(t *testing.T) {
	payload := `{
	  "ordinaryUsageAllowed": false,
	  "rateLimits": {
	    "primary": {"usedPercent": 100, "windowDurationMins": 10080},
	    "secondary": {"usedPercent": 0, "windowDurationMins": 300},
	    "credits": {"hasCredits": false, "unlimited": false, "balance": "0"},
	    "rateLimitReachedType": "rate_limit_reached"
	  },
	  "rateLimitUpsell": {"banner_type": "plus_rate_limit_reached"}
	}`
	var value any
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		t.Fatal(err)
	}
	sanitizeUsage(value)
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	got := string(encoded)
	if strings.Contains(got, `"ordinaryUsageAllowed":false`) || strings.Contains(got, `"usedPercent":100`) {
		t.Fatalf("camelCase payload not sanitized: %s", got)
	}
}
