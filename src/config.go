package main

import (
	"bufio"
	"codex-go-router/src/provider"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// defaultEnvPath mirrors Codex's own CODEX_HOME convention so the router reads
// the same .env file as the Codex installation on this machine.
func defaultEnvPath() string {
	if codexHome := os.Getenv("CODEX_HOME"); codexHome != "" {
		return filepath.Join(codexHome, ".env")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".codex", ".env")
	}
	return ".env"
}

func loadEnv(path string) map[string]string {
	env := map[string]string{}
	file, err := os.Open(path)
	if err != nil {
		log.Printf("no env file at %s; falling back to the process environment", path)
		return env
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		value, ok := strings.CutPrefix(line, "export ")
		if !ok {
			continue
		}
		key, val, found := strings.Cut(value, "=")
		if found {
			env[key] = val
		}
	}
	return env
}

func providerConfigPath() string {
	if path := os.Getenv("ROUTER_CONFIG"); path != "" {
		return path
	}
	return filepath.Join(filepath.Dir(defaultEnvPath()), "router.json")
}

func loadProviders(path string, env map[string]string) ([]provider.Provider, error) {
	var config struct {
		Providers []provider.Provider `json:"providers"`
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) || os.Getenv("ROUTER_CONFIG") != "" {
			return nil, err
		}
	} else if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("read provider config: %w", err)
	}
	key := func(name string) string {
		if value := env[name]; value != "" {
			return value
		}
		return os.Getenv(name)
	}
	result := []provider.Provider{}
	if value := key("OPENCODE_GO_API_KEY"); value != "" {
		p := provider.OpenCode()
		p.APIKey = value
		result = append(result, p)
	}
	seen := map[string]bool{"go": true}
	validID := regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	for _, p := range config.Providers {
		if !validID.MatchString(p.ID) || seen[p.ID] {
			return nil, fmt.Errorf("provider id %q must be unique, use lowercase letters/digits/underscores, and cannot be go", p.ID)
		}
		seen[p.ID] = true
		u, err := url.Parse(p.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("provider %s: base_url must be an http(s) URL without credentials, query, or fragment", p.ID)
		}
		if len(p.Models) == 0 {
			return nil, fmt.Errorf("provider %s: models is required", p.ID)
		}
		models := map[string]bool{}
		for _, m := range p.Models {
			if m.ID == "" || models[m.ID] {
				return nil, fmt.Errorf("provider %s: model ids must be nonempty and unique", p.ID)
			}
			models[m.ID] = true
		}
		if p.APIKeyEnv != "" {
			p.APIKey = key(p.APIKeyEnv)
			if p.APIKey == "" {
				return nil, fmt.Errorf("provider %s: missing API key in %s", p.ID, p.APIKeyEnv)
			}
		}
		if p.Name == "" {
			p.Name = p.ID
		}
		p.BaseURL = strings.TrimRight(p.BaseURL, "/")
		result = append(result, withReasoningTags(p))
	}
	return result, nil
}
