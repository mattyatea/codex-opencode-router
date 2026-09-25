package main

import (
	"encoding/json"
	"strings"
	"testing"
)

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
