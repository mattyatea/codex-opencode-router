package main

import (
	"codex-go-router/src/provider"
	"net/http"
	"strings"
)

// router serves the union catalog and forwards requests by model prefix.
type router struct {
	providers []provider.Provider
	client    *http.Client
}

func newRouter(providers []provider.Provider) *router {
	return &router{providers: providers, client: &http.Client{}}
}

func (rt *router) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/models", rt.handleModels)
	mux.HandleFunc("/responses", rt.handleResponses)
	return mux
}

// providerForModel returns nil for models served by ChatGPT.
func (rt *router) providerForModel(model string) *provider.Provider {
	for i := range rt.providers {
		if strings.HasPrefix(model, rt.providers[i].ModelPrefix()) {
			return &rt.providers[i]
		}
	}
	return nil
}
