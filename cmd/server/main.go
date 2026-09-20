// 跑道异物处置系统服务入口。
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"fodsys/internal/api"
	"fodsys/internal/db"
	"fodsys/internal/notify"
	"fodsys/internal/service"
	"fodsys/internal/store"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	dsn := env("DATABASE_URL", "postgres://postgres@127.0.0.1:54329/fodsys?sslmode=disable")
	migrationsDir := env("MIGRATIONS_DIR", "migrations")
	addr := env("LISTEN_ADDR", ":8080")

	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		log.Fatalf("connect db: %v", err)
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool, migrationsDir); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	st := store.New(pool)
	svc := service.New(st)

	// 通知派发:无内存队列,重启后自动续发 pending 通知
	sender := func(ctx context.Context, channel, subject string, payload any) error {
		log.Printf("notify -> %s: %s", channel, subject)
		return nil
	}
	notifyCtx, stopNotify := context.WithCancel(ctx)
	defer stopNotify()
	go notify.NewWorker(st, 500*time.Millisecond, sender).Run(notifyCtx)

	router := api.NewRouter(svc, st)
	srv := &http.Server{Addr: addr, Handler: router}

	go func() {
		log.Printf("listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Printf("shutting down")

	stopNotify()
	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
