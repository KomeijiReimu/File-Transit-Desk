package main

import (
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
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
	migrate := flag.Bool("migrate-config", false, "离线迁移旧配置到 bootstrap/settings 两文件并退出")
	flag.Parse()
	abs, err := filepath.Abs(*cfgPath)
	if err != nil {
		log.Printf("配置路径无效：%v", err)
		return 1
	}
	*cfgPath = abs
	if *migrate {
		if err := config.MigrateConfig(abs); err != nil {
			log.Printf("配置迁移失败：%v", err)
			return 1
		}
		log.Printf("配置迁移完成：%s", abs)
		return 0
	}
	created, err := config.InitializeExamples(abs)
	if err != nil {
		log.Printf("首次启动配置准备失败：%v", err)
		return 1
	}
	if created {
		log.Printf("已生成首次启动配置：%s 和同目录 settings.yaml。请填写管理员用户名和密码摘要，密码工具支持 --format bootstrap。动态验证码登录默认开启；请从仓库根目录运行 python3 scripts/generate-totp-secret.py，将生成的密钥填入 settings.yaml 的 auth.totp_secret 后重新启动。", abs)
		return 1
	}

	// 启动前先完成配置校验和数据库迁移，任何关键错误都直接终止，避免以半可用状态对外服务。
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Printf("event=startup_config_failed 配置加载失败：%v", err)
		return 1
	}
	listenEndpoint, err := server.ResolveListenEndpoint(cfg.Server.Host, cfg.Server.Port)
	if err != nil {
		log.Printf("event=startup_listener_config_failed 监听地址配置校验失败：%v", err)
		return 1
	}

	st, err := store.Open(cfg.Database.Path, cfg.Audit.Retain)
	if err != nil {
		log.Printf("event=startup_database_failed 数据库初始化失败（路径：%s）：%v", cfg.Database.Path, err)
		return 1
	}

	runtime, err := server.NewRuntimeWithOptions(cfg, st, *cfgPath, server.Options{DevMode: *devMode, DevFrontendPort: *devFrontendPort})
	if err != nil {
		_ = st.DB.Close()
		log.Printf("event=startup_runtime_failed 服务初始化失败：%v", err)
		return 1
	}
	addr := listenerAddress(listenEndpoint)
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	return runServer(runtime, func() error { return runtime.App.Listen(addr) }, signals)
}

func listenerAddress(endpoint server.ListenEndpoint) string {
	// JoinHostPort brackets IPv6 exactly once and never produces malformed
	// forms such as ":::17878" from an unbracketed IPv6 configuration.
	return net.JoinHostPort(endpoint.Host, strconv.Itoa(endpoint.Port))
}

func runServer(runtime runtimeControl, listen func() error, signals <-chan os.Signal) int {
	listenErr := make(chan error, 1)
	go func() { listenErr <- listen() }()
	listenerFailed := false
	select {
	case err := <-listenErr:
		if err != nil {
			log.Printf("event=listener_failed 服务监听失败：%v", err)
		} else {
			log.Printf("event=listener_stopped_unexpectedly 服务监听意外停止")
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
