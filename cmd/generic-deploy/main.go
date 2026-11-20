package main

import (
	"flag"
	"log"

	"github.com/shivamkkas/generic-deployer/internal/console"
)

var version = "dev"

func main() {
	cfgPath := flag.String("config", "deploy.yaml", "path to the config file")
	flag.Parse()

	cfg, err := console.LoadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	srv, err := console.NewServer(cfg, version)
	if err != nil {
		log.Fatalf("targets: %v", err)
	}
	log.Printf("generic-deploy %s listening on %s (targets in %s)", version, cfg.Listen, cfg.TargetsFile)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
