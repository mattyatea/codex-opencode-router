package main

import (
	"codex-go-router/src/provider"
	"encoding/json"
	"io"
	"log"
	"net/http"
)

// Provider models are cloned from this ChatGPT entry so Codex sees a complete model record.
const baseCatalogModel = "gpt-6-luna"

func (rt *router) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") == "" {
		http.Error(w, "missing authorization", http.StatusUnauthorized)
		return
	}
	request, err := chatgptCatalogRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	response, err := rt.client.Do(request)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if response.StatusCode == http.StatusOK {
		var catalog map[string]any
		if json.Unmarshal(body, &catalog) == nil {
			if merged, err := buildUnionCatalog(catalog, rt.providers); err == nil {
				body = merged
			}
		}
	}
	log.Printf("GET /models -> %d", response.StatusCode)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	w.Write(body)
}

func buildUnionCatalog(catalog map[string]any, providers []provider.Provider) ([]byte, error) {
	models, _ := catalog["models"].([]any)
	if len(models) == 0 {
		return json.Marshal(catalog)
	}
	base := findModel(models, baseCatalogModel)
	if base == nil {
		base, _ = models[0].(map[string]any)
	}
	if base == nil {
		return json.Marshal(catalog)
	}
	for _, p := range providers {
		for _, spec := range p.Models {
			clone, err := cloneMap(base)
			if err != nil {
				continue
			}
			applyModelSpec(clone, p, spec)
			models = append(models, clone)
		}
	}
	catalog["models"] = models
	return json.Marshal(catalog)
}

// applyModelSpec overrides the cloned ChatGPT entry with the provider model's identity and limits.
func applyModelSpec(model map[string]any, p provider.Provider, spec provider.Model) {
	model["slug"] = p.ModelPrefix() + spec.ID
	model["display_name"] = spec.Name
	if spec.Name == "" {
		model["display_name"] = p.Name + "/" + spec.ID
	}
	model["description"] = p.Name + " model via the Responses API."
	model["default_reasoning_summary"] = "auto"
	model["service_tiers"] = []any{}
	model["additional_speed_tiers"] = []any{}
	model["use_responses_lite"] = false
	model["multi_agent_version"] = nil
	if spec.ContextWindow > 0 {
		model["context_window"] = spec.ContextWindow
		maxWindow := spec.ContextWindow
		if spec.MaxContextWindow > 0 {
			maxWindow = spec.MaxContextWindow
		}
		model["max_context_window"] = maxWindow
	}
	if spec.AutoCompactTokenLimit != nil {
		model["auto_compact_token_limit"] = *spec.AutoCompactTokenLimit
	}
	if spec.ClearToolMode {
		model["tool_mode"] = nil
	}
	if spec.InputModalities != nil {
		model["input_modalities"] = spec.InputModalities
	}
	if spec.ReasoningLevels != nil {
		levels := make([]any, 0, len(spec.ReasoningLevels))
		for _, effort := range spec.ReasoningLevels {
			levels = append(levels, map[string]any{"effort": effort, "description": effort})
		}
		model["supported_reasoning_levels"] = levels
		model["default_reasoning_level"] = "low"
	}
	if priority, ok := model["priority"].(float64); ok {
		model["priority"] = priority + 20
	}
}

func findModel(models []any, slug string) map[string]any {
	for _, entry := range models {
		if model, ok := entry.(map[string]any); ok && model["slug"] == slug {
			return model
		}
	}
	return nil
}

func cloneMap(source map[string]any) (map[string]any, error) {
	encoded, err := json.Marshal(source)
	if err != nil {
		return nil, err
	}
	var clone map[string]any
	if err := json.Unmarshal(encoded, &clone); err != nil {
		return nil, err
	}
	return clone, nil
}
