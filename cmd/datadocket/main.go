package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"datadocket/internal/store"
	_ "github.com/lib/pq"
)

func main() {
	if len(os.Args) != 2 || (os.Args[1] != "api" && os.Args[1] != "worker") {
		log.Fatal("usage: datadocket api|worker")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is required")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(10)
	db.SetConnMaxLifetime(30 * time.Minute)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := db.PingContext(checkCtx); err != nil {
		log.Fatalf("connect to PostgreSQL: %v", err)
	}
	st := store.New(db)
	if err := st.Migrate(checkCtx); err != nil {
		log.Fatalf("migrate PostgreSQL: %v", err)
	}
	uploadDir := envDefault("UPLOAD_DIR", "data/uploads")
	if err := os.MkdirAll(uploadDir, 0700); err != nil {
		log.Fatalf("create upload directory: %v", err)
	}
	if os.Args[1] == "worker" {
		id, _ := os.Hostname()
		id = fmt.Sprintf("%s-%d", id, os.Getpid())
		if err := runWorker(ctx, st, uploadDir, id); err != nil && !errors.Is(err, context.Canceled) {
			log.Fatal(err)
		}
		return
	}
	maxBytes, err := strconv.ParseInt(envDefault("MAX_UPLOAD_BYTES", "52428800"), 10, 64)
	if err != nil || maxBytes < 1 {
		log.Fatal("MAX_UPLOAD_BYTES must be a positive integer")
	}
	addr := envDefault("HTTP_ADDR", ":8080")
	srv := &http.Server{Addr: addr, Handler: newServer(st, uploadDir, maxBytes), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Printf("Data Docket API listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func envDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
