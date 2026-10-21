package main

import (
	"log"
	"net/http"
	"os"
)

const listenDefault = "127.0.0.1:18789"

func main() {
	env := loadEnv(defaultEnvPath())
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
