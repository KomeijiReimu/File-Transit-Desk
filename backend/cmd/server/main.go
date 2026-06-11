package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"filetrans-backend/internal/config"
	"filetrans-backend/internal/server"
	"filetrans-backend/internal/store"
)

func main() {
	log.SetFlags(0)
	// 配置路径默认使用当前工作目录下的 config.yaml，便于本地和容器部署共用同一入口。
	cfgPath := flag.String("config", "config.yaml", "config path")
	flag.Parse()

	// 启动前先完成配置校验和数据库迁移，任何关键错误都直接终止，避免以半可用状态对外服务。
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		startupFatal("后端启动失败：配置文件无法加载", err)
	}

	st, err := store.Open(cfg.Database.Path, cfg.Audit.Retain)
	if err != nil {
		startupFatal("后端启动失败：数据库无法打开", err)
	}

	app := server.NewWithConfigPath(cfg, st, *cfgPath)
	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	// Fiber 会阻塞监听；监听失败时输出面向部署人员的中文提示。
	if err := app.Listen(addr); err != nil {
		startupFatal("后端启动失败：端口监听失败", err)
	}
}

func startupFatal(title string, err error) {
	fmt.Fprintf(os.Stderr, "\n%s\n%s\n\n", title, err)
	os.Exit(1)
}
