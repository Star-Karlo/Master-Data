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
	"github.com/karlo/masterdata-service/internal/routes"
	"github.com/karlo/masterdata-service/internal/services"
)

// @title           Karlo Master Data API
// @version         1.0
// @description     Reference data. Global catalogues (brands, truck types, cargo types, items, tracker models) shared by every company, and per-company registers (customers, vehicle groups, trucks, warehouses).\n\nEvery endpoint needs a bearer token from the authentication service. Karlo staff may act for a client by sending X-Acting-For: <companyId>.
// @contact.name    Karlo Engineering
// @contact.email   engineering@karlo.co.id
// @host            localhost:5002
// @BasePath        /api/v1
// @schemes         http https
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
	logger.InitFromEnv("masterdata")
	defer logger.Close()

	// The target guard. This service has its own database and must not be
	// pointed at one holding the legacy Mongoose collections, where an
	// unprefixed write would land in a collection the old app still reads.
	db, err := config.ConnectMongo(cfg)
	if err != nil {
		return err
	}

	verifier, err := authctx.NewVerifierFromEnv()
	if err != nil {
		return err
	}

	// The remote validator is the fallback for a token this service cannot
	// verify locally. Its absence is not fatal: local verification covers
	// every token minted under the key this process holds, which in a normal
	// deployment is all of them.
	var remote authctx.RemoteValidator
	authClient, err := clients.NewAuth(cfg.AuthGRPCAddr, "masterdata", cfg.ServiceToken)
	if err != nil {
		slog.Warn("authentication service unreachable; falling back to local token verification only",
			"addr", cfg.AuthGRPCAddr, "error", err)
	} else {
		remote = authClient
		defer func() { _ = authClient.Close() }()
	}

	catalogService := services.NewCatalogService(db)
	fleetService := services.NewFleetService(db)

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
		Remote:   remote,
		Catalog:  handlers.NewCatalogHandler(catalogService),
		Fleet:    handlers.NewFleetHandler(fleetService),
		Registry: handlers.NewRegistryHandler(services.NewRegistryService(db)),
	})

	httpSrv := &http.Server{
		Addr:              ":" + cfg.HTTPPort,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	errCh := make(chan error, 2)

	go func() {
		slog.Info("grpc server listening", "service", "masterdata", "addr", ":"+cfg.GRPCPort)
		if err := grpcSrv.Serve(); err != nil {
			errCh <- err
		}
	}()

	go func() {
		slog.Info("http server listening", "service", "masterdata",
			"env", cfg.Environment, "addr", ":"+cfg.HTTPPort)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return err
	case <-stop:
		slog.Info("shutting down", "service", "masterdata")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	grpcSrv.Shutdown(ctx)
	return httpSrv.Shutdown(ctx)
}
