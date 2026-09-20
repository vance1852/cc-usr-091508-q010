package main

import (
	"log"

	"runwayfod/internal/app"
	"runwayfod/internal/config"
	"runwayfod/internal/httpapi"
	"runwayfod/internal/store"
)

func main() {
	cfg := config.Load()
	db, err := store.Open(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("连接数据库失败: %v", err)
	}
	defer db.Close()

	if cfg.RunMigrations {
		if err := store.ApplySQLFile(db, cfg.SchemaPath); err != nil {
			log.Fatalf("迁移失败: %v", err)
		}
		log.Println("schema 已应用")
	}
	// 可选种子数据：仅在库内尚无机场时加载（重启不重复）。
	if cfg.SeedPath != "" {
		var airports int
		if err := db.QueryRow(`SELECT count(*) FROM airports`).Scan(&airports); err != nil {
			log.Fatalf("检查种子状态失败: %v", err)
		}
		if airports == 0 {
			if err := store.ApplySQLFile(db, cfg.SeedPath); err != nil {
				log.Fatalf("种子数据加载失败: %v", err)
			}
			log.Println("种子数据已加载")
		}
	}

	srv := httpapi.New(app.New(db), db)
	r := srv.Router()
	log.Printf("跑道异物处置系统启动：%s", cfg.HTTPAddr)
	if err := r.Run(cfg.HTTPAddr); err != nil {
		log.Fatalf("HTTP 启动失败: %v", err)
	}
}
