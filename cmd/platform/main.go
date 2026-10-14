// platform 是 Spark Cloud 控制面服务。
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"spark-cloud/internal/app"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/db"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("platform exited", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := db.Migrate(ctx, cfg.DatabaseURL); err != nil {
		return err
	}
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	a, err := app.Build(cfg, pool, log, app.Options{})
	if err != nil {
		return err
	}
	sweeperDone := make(chan struct{})
	go func() {
		defer close(sweeperDone)
		a.Rental.RunSweeper(ctx, cfg.SweepInterval)
	}()

	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: a.Handler, ReadHeaderTimeout: 10 * time.Second}
	serverErr := make(chan error, 1)
	go func() {
		log.Info("platform listening", "addr", cfg.HTTPAddr, "env", cfg.Env)
		serverErr <- srv.ListenAndServe()
	}()

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
	}
	stop() // 确保清扫器退出
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && runErr == nil {
		runErr = err
	}
	<-sweeperDone
	return runErr
}
