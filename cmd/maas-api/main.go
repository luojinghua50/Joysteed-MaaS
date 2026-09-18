package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/httpapi"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--healthcheck" {
		url := "http://127.0.0.1" + env("MAAS_HTTP_ADDR", ":8080") + "/healthz"
		client := http.Client{Timeout: 2 * time.Second}
		response, err := client.Get(url)
		if err != nil || response.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		_ = response.Body.Close()
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dsn := env("MAAS_DATABASE_URL", "host=postgres port=5432 user=maas password=maas_password dbname=maas sslmode=disable")
	db, err := openDatabase(ctx, dsn)
	if err != nil {
		log.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		log.Fatal(err)
	}
	defer sqlDB.Close()

	var redisClient *redis.Client
	if addr := env("MAAS_REDIS_ADDR", "redis:6379"); addr != "" {
		redisClient = redis.NewClient(&redis.Options{Addr: addr, Password: os.Getenv("MAAS_REDIS_PASSWORD")})
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := redisClient.Ping(pingCtx).Err(); err != nil {
			cancel()
			log.Fatal(fmt.Errorf("redis is unavailable: %w", err))
		}
		cancel()
		defer redisClient.Close()
	}

	lifetime := 12 * time.Hour
	if raw := os.Getenv("MAAS_SESSION_LIFETIME"); raw != "" {
		if parsed, parseErr := time.ParseDuration(raw); parseErr == nil {
			lifetime = parsed
		}
	}
	api, err := httpapi.New(db, redisClient, httpapi.Config{
		AdminUsername:        env("MAAS_ADMIN_USERNAME", "admin"),
		AdminPassword:        env("MAAS_ADMIN_PASSWORD", "change-me"),
		SessionLifetime:      lifetime,
		CORSOrigin:           env("MAAS_CORS_ORIGIN", "http://localhost:3000"),
		KeyEncryptionKey:     env("MAAS_KEY_ENCRYPTION_KEY", "development-only-maas-key-encryption-material"),
		GatewayInternalURL:   env("MAAS_GATEWAY_INTERNAL_URL", "http://maas-gateway:8080"),
		GatewayInternalToken: env("MAAS_INTERNAL_TOKEN", "development-only-maas-internal-token"),
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := api.Migrate(ctx); err != nil {
		log.Fatal(err)
	}

	server := &http.Server{Addr: env("MAAS_HTTP_ADDR", ":8080"), Handler: api.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	log.Printf("maas-api listening on %s", server.Addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func openDatabase(ctx context.Context, dsn string) (*gorm.DB, error) {
	var last error
	for attempt := 1; attempt <= 60; attempt++ {
		db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Warn)})
		if err == nil {
			sqlDB, sqlErr := db.DB()
			if sqlErr == nil {
				pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
				pingErr := sqlDB.PingContext(pingCtx)
				cancel()
				if pingErr == nil {
					return db, nil
				}
				last = pingErr
			}
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
		log.Printf("waiting for postgres (attempt %d/60): %v", attempt, last)
	}
	return nil, fmt.Errorf("postgres unavailable: %w", last)
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
