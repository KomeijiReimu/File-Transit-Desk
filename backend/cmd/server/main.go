package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"filetrans-backend/internal/config"
	"filetrans-backend/internal/server"
	"filetrans-backend/internal/store"
)

func main() {
	os.Exit(realMain())
}

type runtimeControl interface {
	BeginDrain()
	Shutdown() error
}

func realMain() int {
	log.SetFlags(0)
	// 配置路径默认使用当前工作目录下的 config.yaml，便于本地和容器部署共用同一入口。
	cfgPath := flag.String("config", "config.yaml", "config path")
	devMode := flag.Bool("dev", false, "enable explicit development mode")
	devFrontendPort := flag.Int("dev-frontend-port", 5173, "development frontend port")
	flag.Parse()

	// 启动前先完成配置校验和数据库迁移，任何关键错误都直接终止，避免以半可用状态对外服务。
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Printf("event=startup_config_failed")
		return 1
	}

	st, err := store.Open(cfg.Database.Path, cfg.Audit.Retain)
	if err != nil {
		log.Printf("event=startup_database_failed")
		return 1
	}

	runtime, err := server.NewRuntimeWithOptions(cfg, st, *cfgPath, server.Options{DevMode: *devMode, DevFrontendPort: *devFrontendPort})
	if err != nil {
		_ = st.DB.Close()
		log.Printf("event=startup_runtime_failed")
		return 1
	}
	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	return runServer(runtime, func() error { return runtime.App.Listen(addr) }, signals)
}

func runServer(runtime runtimeControl, listen func() error, signals <-chan os.Signal) int {
	listenErr := make(chan error, 1)
	go func() { listenErr <- listen() }()
	listenerFailed := false
	select {
	case err := <-listenErr:
		if err != nil {
			log.Printf("event=listener_failed")
		} else {
			log.Printf("event=listener_stopped_unexpectedly")
		}
		listenerFailed = true
	case <-signals:
		log.Printf("event=server_draining")
	}
	runtime.BeginDrain()
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- runtime.Shutdown() }()
	select {
	case <-signals:
		log.Printf("event=server_force_exit")
		return 2
	case err := <-shutdownDone:
		if err != nil {
			log.Printf("[CRITICAL] event=server_shutdown_failed")
			return 1
		}
		log.Printf("event=server_shutdown_complete")
		if listenerFailed {
			return 1
		}
		return 0
	}
}
