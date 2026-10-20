package main

import (
	"fmt"
	"net/http"
)

const chatgptOrigin = "https://chatgpt.com"
const minCatalogClientVersion = "0.160.0"

func chatgptHeaders(incoming http.Header) (http.Header, error) {
	authorization := incoming.Get("Authorization")
	if authorization == "" {
		return nil, fmt.Errorf("missing authorization")
	}
	headers := http.Header{}
	headers.Set("Authorization", authorization)
	copyHeaders(headers, incoming)
	return headers, nil
}

func chatgptCatalogRequest(r *http.Request) (*http.Request, error) {
	request, err := http.NewRequestWithContext(
		r.Context(), http.MethodGet, chatgptOrigin+"/backend-api/codex"+r.URL.RequestURI(), nil,
	)
	if err != nil {
		return nil, err
	}
	// The upstream catalog hides newer models from older Codex clients.
	// Ask for at least the version that first includes GPT-6.1 Sol.
	var major, minor, patch int
	if n, _ := fmt.Sscanf(r.URL.Query().Get("client_version"), "%d.%d.%d", &major, &minor, &patch); n == 3 && major == 0 && minor < 160 {
		query := request.URL.Query()
		query.Set("client_version", minCatalogClientVersion)
		request.URL.RawQuery = query.Encode()
	}
	request.Header.Set("Authorization", r.Header.Get("Authorization"))
	return request, nil
}
