// Package routes wires master data's HTTP surface.
//
// The reference lists and sites are readable and writable; vehicles are
// readable only. That split is deliberate rather than unfinished: a site is a
// name, an address and a point on a map, while creating a VEHICLE means the
// plate rules, the head-versus-body distinction and the tracker fitting
// history, none of which a generic writer can be trusted with.
//
// Every write goes through repository.Store so BeforeWrite fills the normalised
// fields the unique indexes are built on. A handler writing to a collection
// directly would produce a document those partial indexes ignore, and a
// duplicate would be created with no error at all.
package routes

import (
	"net/http"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"

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

	api := router.Group("/api/v1")
	api.Use(authctx.RequireAuth(d.Verifier, d.Remote))

	// Reference lists. One route for every catalogue, the kind selecting the
	// collection — which is what avoids ten near-identical route groups.
	api.GET("/catalog", authctx.RequireModule("masterData.read"), d.Catalog.Kinds)
	api.GET("/catalog/:kind", authctx.RequireModule("masterData.read"), d.Catalog.List)
	api.GET("/catalog/:kind/:id", authctx.RequireModule("masterData.read"), d.Catalog.Get)

	// Writes. A company adds its own private entries; Karlo staff maintain the
	// global lists everyone shares — the service decides which from the token,
	// never from the payload.
	api.POST("/catalog/:kind", authctx.RequireModule("masterData.create"), d.Catalog.Create)
	api.PUT("/catalog/:kind/:id", authctx.RequireModule("masterData.update"), d.Catalog.Update)
	api.DELETE("/catalog/:kind/:id", authctx.RequireModule("masterData.delete"), d.Catalog.Delete)

	// The fleet register, under the names the TMS frontend already uses.
	api.GET("/trucks", authctx.RequireModule("truck.read"), d.Fleet.ListTrucks)
	api.GET("/trucks/:id", authctx.RequireModule("truck.read"), d.Fleet.GetTruck)

	api.GET("/warehouses", authctx.RequireModule("warehouse.read"), d.Fleet.ListWarehouses)
	api.GET("/warehouses/:id", authctx.RequireModule("warehouse.read"), d.Fleet.GetWarehouse)
	api.POST("/warehouses", authctx.RequireModule("warehouse.create"), d.Fleet.CreateWarehouse)
	api.PUT("/warehouses/:id", authctx.RequireModule("warehouse.update"), d.Fleet.UpdateWarehouse)
	api.DELETE("/warehouses/:id", authctx.RequireModule("warehouse.delete"), d.Fleet.DeleteWarehouse)

	return router
}
