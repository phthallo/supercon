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
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL is empty; set it to a postgres:// URL")
	}
	store, err := OpenStore(dbURL, os.Getenv("IMPORT_SQLITE"))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	rhythm, err := OpenRhythm(store.db, env("QUADRANT_URL", "https://quadrant.underwhelm.ing"))
	if err != nil {
		log.Fatalf("open quadrant: %v", err)
	}

	admin := os.Getenv("ADMIN_PASSWORD")
	if admin == "" {
		log.Print("ADMIN_PASSWORD is empty; /admin is disabled")
	}
	srv := &Server{
		store:         store,
		limiter:       NewLimiter(cooldown),
		hub:           NewHub(),
		rhythm:        rhythm,
		adminPassword: admin,
		ipHeader:      env("TRUSTED_PROXY_HEADER", ""),
		wifiSSID:      os.Getenv("WIFI_SSID"),
		wifiPassword:  os.Getenv("WIFI_PASSWORD"),
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
