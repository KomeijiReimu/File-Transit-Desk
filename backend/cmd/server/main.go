package main

import (
	"flag"
	"fmt"
	"log"

	"filetrans-backend/internal/config"
	"filetrans-backend/internal/server"
	"filetrans-backend/internal/store"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "config path")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}

	st, err := store.Open(cfg.Database.Path, cfg.Audit.Retain)
	if err != nil {
		log.Fatal(err)
	}

	app := server.New(cfg, st)
	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	log.Fatal(app.Listen(addr))
}
