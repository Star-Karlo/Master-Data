package unit

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/karlo/masterdata-service/internal/config"
	"github.com/karlo/masterdata-service/internal/platform/authctx"
	"github.com/karlo/masterdata-service/internal/routes"
)

// buildRouter constructs the HTTP surface with no database behind it. Handlers
// are nil, which is safe because these tests exercise routing and middleware:
// every request is rejected before a handler runs.
func buildRouter(t *testing.T, environment string) http.Handler {
	t.Helper()

	verifier, err := authctx.NewVerifier(testPublicKeyPEM(t))
	if err != nil {
		t.Fatalf("could not build verifier: %v", err)
	}

	return routes.Setup(routes.Deps{
		Config: &config.Config{
			Environment:        environment,
			CORSAllowedOrigins: []string{"http://localhost:5173"},
		},
		Verifier: verifier,
	})
}

func testPublicKeyPEM(t *testing.T) []byte {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("could not generate a key: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("could not marshal the public key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func TestHealthEndpoint(t *testing.T) {
	router := buildRouter(t, "development")

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET /health = %d, want 200", rec.Code)
	}
}

func TestSwaggerVisibility(t *testing.T) {
	dev := buildRouter(t, "development")
	req := httptest.NewRequest(http.MethodGet, "/swagger/index.html", nil)
	rec := httptest.NewRecorder()
	dev.ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Error("the Swagger UI should be served in development")
	}

	prod := buildRouter(t, "production")
	req = httptest.NewRequest(http.MethodGet, "/swagger/index.html", nil)
	rec = httptest.NewRecorder()
	prod.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("the Swagger UI should be hidden in production, got %d", rec.Code)
	}
}

// TestEveryRouteRequiresAuthentication is stronger here than for the
// authentication service: master data has no public surface at all. Catalogue
// contents and a company's fleet are both commercially sensitive.
func TestEveryRouteRequiresAuthentication(t *testing.T) {
	router := buildRouter(t, "development")

	const oid = "507f1f77bcf86cd799439011"

	routes := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/catalog"},
		{http.MethodGet, "/api/v1/catalog/truckType"},
		{http.MethodGet, "/api/v1/catalog/truckType/" + oid},
		{http.MethodPut, "/api/v1/catalog/truckType"},
		{http.MethodGet, "/api/v1/trucks"},
		{http.MethodPost, "/api/v1/trucks"},
		{http.MethodGet, "/api/v1/trucks/" + oid},
		{http.MethodPut, "/api/v1/trucks/" + oid},
		{http.MethodDelete, "/api/v1/trucks/" + oid},
		{http.MethodPost, "/api/v1/trucks/" + oid + "/drivers"},
		{http.MethodDelete, "/api/v1/trucks/" + oid + "/drivers"},
		{http.MethodGet, "/api/v1/warehouses"},
		{http.MethodPost, "/api/v1/warehouses"},
		{http.MethodGet, "/api/v1/warehouses/" + oid},
		{http.MethodPut, "/api/v1/warehouses/" + oid},
		{http.MethodDelete, "/api/v1/warehouses/" + oid},
	}

	for _, route := range routes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			req := httptest.NewRequest(route.method, route.path, nil)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("= %d, want 401 for an unauthenticated request", rec.Code)
			}
		})
	}
}

func TestCORSRejectsUnlistedOrigins(t *testing.T) {
	router := buildRouter(t, "development")

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/trucks", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if allowed := rec.Header().Get("Access-Control-Allow-Origin"); allowed != "" {
		t.Errorf("an unlisted origin was allowed: %q", allowed)
	}
}
