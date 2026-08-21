// Package routes wires the master data HTTP surface.
package routes

import (
	"net/http"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	swaggerfiles "github.com/swaggo/files"
	ginswagger "github.com/swaggo/gin-swagger"

	// Imported for its side effect: the generated package registers the
	// OpenAPI document with the swagger runtime on init.
	_ "github.com/karlo/masterdata-service/docs"
	"github.com/karlo/masterdata-service/internal/config"
	"github.com/karlo/masterdata-service/internal/handlers"
	"github.com/karlo/masterdata-service/internal/middleware"
	"github.com/karlo/masterdata-service/internal/platform/authctx"
)

type Deps struct {
	Config   *config.Config
	Verifier *authctx.Verifier
	Remote   authctx.RemoteValidator

	Catalog *handlers.CatalogHandler
	Fleet   *handlers.FleetHandler
}

func Setup(d Deps) *gin.Engine {
	if d.Config.IsProduction() {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.New()
	router.Use(gin.Recovery())
	router.Use(middleware.RequestLogger())
	router.Use(cors.New(cors.Config{
		AllowOrigins:     d.Config.CORSAllowedOrigins,
		AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization", "X-Request-Id"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}))

	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "masterdata"})
	})

	// The interactive API browser. It is served only outside production: the
	// document describes every endpoint and its shapes, which is exactly the
	// reconnaissance an attacker would otherwise have to guess at.
	if !d.Config.IsProduction() {
		// /swagger/index.html is the browser; /swagger/doc.json is the raw
		// document, which is what client generators want.
		router.GET("/swagger/*any", ginswagger.WrapHandler(swaggerfiles.Handler))
	}

	api := router.Group("/api/v1")
	api.Use(authctx.RequireAuth(d.Verifier, d.Remote))

	// Global catalogues: readable by any authenticated user, writable only by
	// platform staff. Reference data is shared, so a company editing it would
	// change what every other company sees.
	catalog := api.Group("/catalog")
	catalog.GET("", d.Catalog.Kinds)
	catalog.GET("/:kind", d.Catalog.List)
	catalog.GET("/:kind/:id", d.Catalog.Get)
	catalog.PUT("/:kind", authctx.RequireRole("superadmin", "admin"), d.Catalog.Upsert)

	// Company catalogues: scoped to the caller's company by the handler, with
	// module permissions on the mutating routes.
	trucks := api.Group("/trucks")
	trucks.GET("", d.Fleet.ListTrucks)
	trucks.GET("/:id", d.Fleet.GetTruck)
	trucks.POST("", authctx.RequireModule("truck.create"), d.Fleet.CreateTruck)
	trucks.PUT("/:id", authctx.RequireModule("truck.update"), d.Fleet.UpdateTruck)
	trucks.DELETE("/:id", authctx.RequireModule("truck.delete"), d.Fleet.DeleteTruck)
	trucks.POST("/:id/drivers", authctx.RequireModule("truck.addDriver"), d.Fleet.AddDriver)
	trucks.DELETE("/:id/drivers", authctx.RequireModule("truck.deleteDriver"), d.Fleet.RemoveDriver)

	warehouses := api.Group("/warehouses")
	warehouses.GET("", d.Fleet.ListWarehouses)
	warehouses.GET("/:id", d.Fleet.GetWarehouse)
	warehouses.POST("", authctx.RequireModule("warehouse.create"), d.Fleet.CreateWarehouse)
	warehouses.PUT("/:id", authctx.RequireModule("warehouse.update"), d.Fleet.UpdateWarehouse)
	warehouses.DELETE("/:id", authctx.RequireModule("warehouse.delete"), d.Fleet.DeleteWarehouse)

	return router
}
