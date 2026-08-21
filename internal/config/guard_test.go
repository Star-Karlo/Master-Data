package config

import (
	"strings"
	"testing"
)

// TestGuardRefusesLegacyTargets is the regression test for the accident this
// guard exists to prevent: the new service writing into the monolith's
// production database.
//
// The URIs below are deliberately fabricated. They carry a placeholder password
// because the guard must reject a fully-formed legacy connection string, which
// is exactly the shape gosec flags as a hardcoded credential.
//
//nolint:gosec // G101: fabricated fixtures, not real credentials
func TestGuardRefusesLegacyTargets(t *testing.T) {
	cases := []struct {
		name     string
		uri      string
		database string
	}{
		{
			name:     "legacy atlas cluster",
			uri:      "mongodb+srv://karlobe:pw@cluster0-ndqn7.mongodb.net/prod?retryWrites=true",
			database: "karlo_masterdata",
		},
		{
			name:     "legacy cluster even with a safe database name",
			uri:      "mongodb+srv://user:pw@cluster0-ndqn7.mongodb.net/",
			database: "karlo_masterdata",
		},
		{
			name:     "legacy production database name",
			uri:      "mongodb://localhost:27017",
			database: "prod",
		},
		{
			name:     "legacy test database name",
			uri:      "mongodb://localhost:27017",
			database: "test",
		},
		{
			name:     "legacy development database name",
			uri:      "mongodb://localhost:27017",
			database: "local",
		},
		{
			name:     "legacy database named in the uri path",
			uri:      "mongodb://localhost:27017/prod",
			database: "karlo_masterdata",
		},
		{
			name:     "empty database name",
			uri:      "mongodb://localhost:27017",
			database: "",
		},
		{
			name:     "database name differing only by case",
			uri:      "mongodb://localhost:27017",
			database: "PROD",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := guardTarget(tc.uri, tc.database)
			if err == nil {
				t.Fatalf("guardTarget(%q, %q) allowed a legacy target", tc.uri, tc.database)
			}
			if !strings.Contains(err.Error(), "refusing to connect") {
				t.Errorf("unexpected error text: %v", err)
			}
		})
	}
}

func TestGuardAllowsFreshTargets(t *testing.T) {
	cases := []struct {
		uri      string
		database string
	}{
		{"mongodb://localhost:27017", "karlo_masterdata"},
		{"mongodb://mongo:27017", "karlo_masterdata"},
		{"mongodb://localhost:27017/karlo_masterdata", "karlo_masterdata"},
		{"mongodb+srv://user:pw@new-cluster.mongodb.net/", "karlo_masterdata_staging"},
	}

	for _, tc := range cases {
		if err := guardTarget(tc.uri, tc.database); err != nil {
			t.Errorf("guardTarget(%q, %q) refused a valid target: %v", tc.uri, tc.database, err)
		}
	}
}

// TestCollectionPrefixAvoidsLegacyNames confirms the second line of defence:
// the legacy Mongoose models occupy the unprefixed names, so nothing this
// service writes can land in one of their collections.
func TestCollectionPrefixAvoidsLegacyNames(t *testing.T) {
	legacyCollections := []string{"trucks", "warehouses", "customers", "points", "clusters"}

	for _, legacy := range legacyCollections {
		got := Collection(legacy)
		if got == legacy {
			t.Errorf("Collection(%q) returned the legacy name unchanged", legacy)
		}
		if !strings.HasPrefix(got, CollectionPrefix) {
			t.Errorf("Collection(%q) = %q, which lacks the %q prefix", legacy, got, CollectionPrefix)
		}
	}

	// Applying the prefix twice must not double it.
	if once, twice := Collection("trucks"), Collection(Collection("trucks")); once != twice {
		t.Errorf("Collection is not idempotent: %q vs %q", once, twice)
	}
}

func TestDatabaseFromURI(t *testing.T) {
	cases := map[string]string{
		"mongodb://localhost:27017":                       "",
		"mongodb://localhost:27017/":                      "",
		"mongodb://localhost:27017/karlo_masterdata":      "karlo_masterdata",
		"mongodb://localhost:27017/prod?retryWrites=true": "prod",
		"mongodb+srv://u:p@host.mongodb.net/test?w=1":     "test",
		"not-a-uri": "",
	}

	for uri, want := range cases {
		if got := databaseFromURI(uri); got != want {
			t.Errorf("databaseFromURI(%q) = %q, want %q", uri, got, want)
		}
	}
}
