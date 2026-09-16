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
	"fmt"
	"net/http"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	swaggerfiles "github.com/swaggo/files"
	ginswagger "github.com/swaggo/gin-swagger"

	// Registers the generated OpenAPI document with the swagger runtime on init.
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

	Catalog  *handlers.CatalogHandler
	Fleet    *handlers.FleetHandler
	Registry *handlers.RegistryHandler
}

func Setup(d Deps) *gin.Engine {
	if d.Config.IsProduction() {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.New()
	// Behind a load balancer every request arrives from a VPC address with the
	// real client in X-Forwarded-For. Gin trusts that header from anyone by
	// default, which lets a caller choose the IP the audit log records. When
	// the operator names the proxies, trust only those; an unset list keeps
	// the default, so this is opt-in and changes nothing until configured.
	if len(d.Config.TrustedProxies) > 0 {
		if err := router.SetTrustedProxies(d.Config.TrustedProxies); err != nil {
			panic(fmt.Sprintf("routes: TRUSTED_PROXIES: %v", err))
		}
	}
	router.Use(gin.Recovery())
	router.Use(middleware.RequestLogger())
	router.Use(cors.New(cors.Config{
		AllowOrigins:     d.Config.CORSAllowedOrigins,
		AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization", "X-Request-Id", "X-Acting-For"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}))

	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "masterdata"})
	})

	// The interactive API browser: /swagger/index.html for people,
	// /swagger/doc.json for client generators. Served in every environment,
	// as the other services do — the document only describes shapes, and
	// every endpoint it lists still demands a token.
	router.GET("/swagger/*any", ginswagger.WrapHandler(swaggerfiles.Handler))

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

	// The writable fleet register, shared by both products. Each route lists
	// every product's spelling of the ability; any one lets the caller in.
	any := authctx.RequireAnyOf
	r := d.Registry

	api.GET("/drivers", any("tms:masterData.read", "fms:drivers.view"), r.ListDrivers)
	api.GET("/drivers/:id", any("tms:masterData.read", "fms:drivers.view"), r.GetDriver)
	api.POST("/drivers", any("tms:masterData.create", "fms:drivers.edit"), r.CreateDriver)
	api.PUT("/drivers/:id", any("tms:masterData.update", "fms:drivers.edit"), r.UpdateDriver)
	api.DELETE("/drivers/:id", any("tms:masterData.delete", "fms:drivers.edit"), r.DeleteDriver)

	api.GET("/vehicles", any("tms:truck.read", "fms:vehicles.view"), r.ListVehicles)
	api.GET("/vehicles/:id", any("tms:truck.read", "fms:vehicles.view"), r.GetVehicle)
	api.POST("/vehicles", any("tms:truck.create", "fms:vehicles.edit"), r.CreateVehicle)
	api.PUT("/vehicles/:id", any("tms:truck.update", "fms:vehicles.edit"), r.UpdateVehicle)
	api.DELETE("/vehicles/:id", any("tms:truck.delete", "fms:vehicles.edit"), r.DeleteVehicle)

	api.GET("/trackers", any("tms:masterData.read", "fms:vehicles.view", "fms:dashcams.manage"), r.ListTrackers)
	api.GET("/trackers/:id", any("tms:masterData.read", "fms:vehicles.view", "fms:dashcams.manage"), r.GetTracker)
	api.GET("/trackers/:id/assignments", any("tms:masterData.read", "fms:vehicles.view", "fms:dashcams.manage"), r.Assignments)
	api.POST("/trackers", any("tms:masterData.create", "fms:vehicles.edit", "fms:dashcams.manage"), r.CreateTracker)
	api.PUT("/trackers/:id", any("tms:masterData.update", "fms:vehicles.edit", "fms:dashcams.manage"), r.UpdateTracker)
	api.DELETE("/trackers/:id", any("tms:masterData.delete", "fms:vehicles.edit", "fms:dashcams.manage"), r.DeleteTracker)
	api.POST("/trackers/:id/fit", any("tms:masterData.update", "fms:vehicles.edit", "fms:dashcams.manage"), r.Fit)
	api.POST("/trackers/:id/unfit", any("tms:masterData.update", "fms:vehicles.edit", "fms:dashcams.manage"), r.Unfit)

	api.GET("/documents", any("tms:truck.read", "fms:vehicles.view", "fms:drivers.view"), r.ListDocuments)
	api.POST("/documents", any("tms:truck.update", "fms:vehicles.edit", "fms:drivers.edit"), r.CreateDocument)
	api.PUT("/documents/:id", any("tms:truck.update", "fms:vehicles.edit", "fms:drivers.edit"), r.UpdateDocument)
	api.DELETE("/documents/:id", any("tms:truck.update", "fms:vehicles.edit", "fms:drivers.edit"), r.DeleteDocument)

	return router
}
