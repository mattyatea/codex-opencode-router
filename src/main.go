package main

import (
	"codex-go-router/src/provider"
	"log"
	"net/http"
	"os"
)

const listenDefault = "127.0.0.1:18789"

func main() {
	env := loadEnv(defaultEnvPath())
	if len(os.Args) > 1 {
		if len(os.Args) != 3 || os.Args[1] != "claude" || (os.Args[2] != "login" && os.Args[2] != "status") {
			log.Fatal("usage: codex-go-router [claude login|status]")
		}
		python := env["CLAUDE_AGENT_PYTHON"]
		if python == "" {
			python = os.Getenv("CLAUDE_AGENT_PYTHON")
		}
		if python == "" {
			log.Fatal("set CLAUDE_AGENT_PYTHON to the Python executable with claude-agent-sdk installed")
		}
		if err := provider.ClaudeAuth(python, os.Args[2]); err != nil {
			log.Fatal(err)
		}
		return
	}
	var err error
	providers, err = loadProviders(providerConfigPath(), env)
	if err != nil {
		log.Fatal(err)
	}
	listenAddr := os.Getenv("ROUTER_LISTEN")
	if listenAddr == "" {
		listenAddr = listenDefault
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/models", handleModels)
	mux.HandleFunc("/responses", handleResponses)
	serveAPI()
	log.Printf("codex-go-router listening on %s", listenAddr)
	log.Fatal(http.ListenAndServe(listenAddr, mux))
}
