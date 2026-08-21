// Command server runs the master data service.
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

	"github.com/karlo/masterdata-service/internal/clients"
	"github.com/karlo/masterdata-service/internal/config"
	"github.com/karlo/masterdata-service/internal/grpcserver"
	"github.com/karlo/masterdata-service/internal/handlers"
	"github.com/karlo/masterdata-service/internal/platform/authctx"
	masterdatav1 "github.com/karlo/masterdata-service/internal/platform/genproto/karlo/masterdata/v1"
	"github.com/karlo/masterdata-service/internal/platform/grpcutil"
	"github.com/karlo/masterdata-service/internal/platform/logger"
	"github.com/karlo/masterdata-service/internal/repository"
	"github.com/karlo/masterdata-service/internal/routes"
	"github.com/karlo/masterdata-service/internal/services"
)

// @title           Karlo Master Data API
// @version         1.0
// @description     Reference data. Global catalogues (truck types, cargo types, cities) shared by every company, and per-company registers (trucks, warehouses, customers).
// @termsOfService  https://karlo.co.id/terms
//
// @contact.name    Karlo Engineering
// @contact.email   engineering@karlo.co.id
//
// @host            localhost:5002
// @BasePath        /api/v1
// @schemes         http https
//
// @securityDefinitions.apikey BearerAuth
// @in                         header
// @name                       Authorization
// @description                RS256 access token issued by the authentication service, as "Bearer <token>".
func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Logs go to stdout as JSON, and additionally to Fluentd when
	// FLUENTD_HOST is set. An unreachable collector degrades to
	// stdout-only rather than stopping the service.
	logger.InitFromEnv("masterdata")
	defer logger.Close()

	// This service verifies tokens but never mints them, so it loads only the
	// public key.
	verifier, err := authctx.NewVerifierFromEnv()
	if err != nil {
		return err
	}

	db, err := config.ConnectMongo(cfg)
	if err != nil {
		return err
	}

	indexCtx, cancelIndex := context.WithTimeout(context.Background(), 30*time.Second)
	err = config.EnsureIndexes(indexCtx, db)
	cancelIndex()
	if err != nil {
		return err
	}

	authClient, err := clients.NewAuth(
		envOr("AUTH_GRPC_ADDR", "localhost:6001"),
		"masterdata",
		cfg.ServiceToken,
	)
	if err != nil {
		return err
	}
	defer func() {
		if err := authClient.Close(); err != nil {
			slog.Error("auth client close failed", "error", err)
		}
	}()

	catalogRepo := repository.NewCatalogRepository(db)
	truckRepo := repository.NewTruckRepository(db)
	warehouseRepo := repository.NewWarehouseRepository(db)

	catalogService := services.NewCatalogService(catalogRepo, cfg.CatalogCacheTTL)
	fleetService := services.NewFleetService(truckRepo, warehouseRepo, catalogService)

	grpcSrv := grpcutil.NewServer(grpcutil.ServerConfig{
		Service:               "masterdata",
		Addr:                  ":" + cfg.GRPCPort,
		Verifier:              verifier,
		AcceptedServiceTokens: cfg.AcceptedServiceTokens,
		EnableReflection:      !cfg.IsProduction(),
	})
	masterdatav1.RegisterMasterDataServiceServer(
		grpcSrv.Registrar(),
		grpcserver.New(catalogService, fleetService),
	)

	router := routes.Setup(routes.Deps{
		Config:   cfg,
		Verifier: verifier,
		Remote:   authClient,
		Catalog:  handlers.NewCatalogHandler(catalogService),
		Fleet:    handlers.NewFleetHandler(fleetService),
	})

	httpSrv := &http.Server{
		Addr:              ":" + cfg.HTTPPort,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 2)

	go func() {
		slog.Info("http server listening", "addr", httpSrv.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	go func() {
		if err := grpcSrv.Serve(); err != nil {
			errCh <- err
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return err
	case sig := <-quit:
		slog.Info("shutting down", "signal", sig.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := httpSrv.Shutdown(ctx); err != nil {
		slog.Error("http shutdown failed", "error", err)
	}
	grpcSrv.Shutdown(ctx)

	if err := db.Client().Disconnect(ctx); err != nil {
		slog.Error("mongodb disconnect failed", "error", err)
	}

	slog.Info("stopped")
	return nil
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
