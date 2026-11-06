package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	cooldown, err := time.ParseDuration(env("PAINT_COOLDOWN", "30s"))
	if err != nil {
		log.Fatalf("PAINT_COOLDOWN: %v", err)
	}
	store, err := OpenStore(env("DB_PATH", "ink.db"))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	admin := os.Getenv("ADMIN_PASSWORD")
	if admin == "" {
		log.Print("ADMIN_PASSWORD is empty; /admin is disabled")
	}
	srv := &Server{
		store:         store,
		limiter:       NewLimiter(cooldown),
		hub:           NewHub(),
		adminPassword: admin,
		ipHeader:      env("TRUSTED_PROXY_HEADER", ""),
	}
	httpSrv := &http.Server{
		Addr:              env("ADDR", ":8080"),
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutdown)
	}()
	log.Printf("listening on %s, cooldown %s", httpSrv.Addr, cooldown)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
