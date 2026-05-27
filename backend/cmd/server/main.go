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
	// 配置路径默认使用当前工作目录下的 config.yaml，便于本地和容器部署共用同一入口。
	cfgPath := flag.String("config", "config.yaml", "config path")
	flag.Parse()

	// 启动前先完成配置校验和数据库迁移，任何关键错误都直接终止，避免以半可用状态对外服务。
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}

	st, err := store.Open(cfg.Database.Path, cfg.Audit.Retain)
	if err != nil {
		log.Fatal(err)
	}

	app := server.NewWithConfigPath(cfg, st, *cfgPath)
	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	// Fiber 会阻塞监听；退出时把错误交给 log.Fatal 统一打印。
	log.Fatal(app.Listen(addr))
}
