package main

import (
	"codex-go-router/src/provider"
	"log"
	"net/http"
	"os"
)

const (
	listenDefault    = "127.0.0.1:18789"
	apiListenDefault = "127.0.0.1:8000,[::1]:8000"
)

func main() {
	env := loadEnv(defaultEnvPath())
	if len(os.Args) > 1 {
		runClaudeAuth(env)
		return
	}
	providers, err := loadProviders(providerConfigPath(), env)
	if err != nil {
		log.Fatal(err)
	}
	serveAPI(envOr("ROUTER_API_LISTEN", apiListenDefault))
	listenAddr := envOr("ROUTER_LISTEN", listenDefault)
	log.Printf("codex-go-router listening on %s", listenAddr)
	log.Fatal(http.ListenAndServe(listenAddr, newRouter(providers).handler()))
}

// runClaudeAuth handles `codex-go-router claude login|status`.
func runClaudeAuth(env map[string]string) {
	if len(os.Args) != 3 || os.Args[1] != "claude" || (os.Args[2] != "login" && os.Args[2] != "status") {
		log.Fatal("usage: codex-go-router [claude login|status]")
	}
	bridge, err := claudeBridge(env)
	if err != nil {
		log.Fatal(err)
	}
	if bridge.Node == "" {
		log.Fatal("set CLAUDE_AGENT_NODE to the Node.js executable and CLAUDE_AGENT_SDK_DIR to the Claude Agent SDK install directory")
	}
	if err := provider.ClaudeAuth(bridge, os.Args[2]); err != nil {
		log.Fatal(err)
	}
}
