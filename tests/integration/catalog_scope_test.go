//go:build integration

package integration

import (
	"errors"
	"testing"

	"github.com/karlo/masterdata-service/internal/models"
	"github.com/karlo/masterdata-service/internal/platform/query"
	"github.com/karlo/masterdata-service/internal/repository"
)

// TestCatalogueScopingKeepsCompaniesApart is the isolation guarantee for
// company-extensible catalogues: item types, body types, truck types and the
// rest, where each company defines its own alongside the platform's.
//
// Three visibility rules, all enforced in the query rather than checked
// afterwards:
//
//	a company sees the platform's globals
//	a company sees its own private entries
//	a company sees nothing of anybody else's
func TestCatalogueScopingKeepsCompaniesApart(t *testing.T) {
	db := testDB(t)
	resetCollections(t, db)

	repo := repository.NewCatalogRepository(db)

	const (
		companyA = "company-a"
		companyB = "company-b"
	)

	global := &models.CatalogItem{
		CompanyID: models.GlobalCompanyID,
		Kind:      models.KindItemType,
		Code:      "PALLET",
		Name:      "Pallet",
		Active:    true,
	}
	privateA := &models.CatalogItem{
		CompanyID: companyA,
		Kind:      models.KindItemType,
		Code:      "JUMBO_BAG",
		Name:      "Jumbo Bag (A's own)",
		Active:    true,
	}
	privateB := &models.CatalogItem{
		CompanyID: companyB,
		Kind:      models.KindItemType,
		Code:      "DRUM_B",
		Name:      "Drum (B's own)",
		Active:    true,
	}

	for _, item := range []*models.CatalogItem{global, privateA, privateB} {
		if err := repo.Upsert(ctx(), item); err != nil {
			t.Fatalf("could not seed %s: %v", item.Code, err)
		}
	}

	t.Run("a company sees globals plus its own", func(t *testing.T) {
		items, total, err := repo.List(ctx(), companyA, models.KindItemType, nil, query.Params{PageSize: 50})
		if err != nil {
			t.Fatalf("list failed: %v", err)
		}
		if total != 2 {
			t.Fatalf("company A sees %d entries, want 2 (one global, one of its own)", total)
		}

		codes := map[string]bool{}
		for _, item := range items {
			codes[item.Code] = true
		}
		if !codes["PALLET"] {
			t.Error("company A cannot see the platform-global entry")
		}
		if !codes["JUMBO_BAG"] {
			t.Error("company A cannot see its own entry")
		}
		if codes["DRUM_B"] {
			t.Error("company A can see company B's private entry")
		}
	})

	t.Run("the other company sees its own, not A's", func(t *testing.T) {
		items, _, err := repo.List(ctx(), companyB, models.KindItemType, nil, query.Params{PageSize: 50})
		if err != nil {
			t.Fatalf("list failed: %v", err)
		}
		for _, item := range items {
			if item.Code == "JUMBO_BAG" {
				t.Error("company B can see company A's private entry")
			}
		}
	})

	t.Run("a direct lookup of another company's entry is a miss", func(t *testing.T) {
		// A miss rather than a permission error: a distinct response would
		// confirm the id exists, which is itself a disclosure.
		_, err := repo.FindByID(ctx(), companyA, models.KindItemType, privateB.ID)
		if !errors.Is(err, repository.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}

		// The owner still reaches it.
		if _, err := repo.FindByID(ctx(), companyB, models.KindItemType, privateB.ID); err != nil {
			t.Errorf("the owning company could not read its own entry: %v", err)
		}

		// And everyone reaches the global.
		for _, company := range []string{companyA, companyB} {
			if _, err := repo.FindByID(ctx(), company, models.KindItemType, global.ID); err != nil {
				t.Errorf("%s could not read the global entry: %v", company, err)
			}
		}
	})

	t.Run("reference validation refuses another company's entry", func(t *testing.T) {
		// This is the one that matters most: it gates an order write in the
		// business service. Validating another company's private entry as
		// usable would let an order point at data its owner cannot see.
		found, err := repo.ExistingIDs(ctx(), companyA, []repository.CatalogRef{
			{Kind: models.KindItemType, ID: privateB.ID},
		})
		if err != nil {
			t.Fatalf("validation failed: %v", err)
		}
		if found[string(models.KindItemType)+":"+privateB.ID.Hex()] {
			t.Error("company A validated a reference to company B's private entry")
		}

		found, err = repo.ExistingIDs(ctx(), companyA, []repository.CatalogRef{
			{Kind: models.KindItemType, ID: privateA.ID},
			{Kind: models.KindItemType, ID: global.ID},
		})
		if err != nil {
			t.Fatalf("validation failed: %v", err)
		}
		if !found[string(models.KindItemType)+":"+privateA.ID.Hex()] {
			t.Error("a company's own entry failed validation")
		}
		if !found[string(models.KindItemType)+":"+global.ID.Hex()] {
			t.Error("a global entry failed validation")
		}
	})
}

// TestTwoCompaniesMayShareACode: uniqueness is per company. Both may define an
// item type coded "BOX"; neither may define it twice.
func TestTwoCompaniesMayShareACode(t *testing.T) {
	db := testDB(t)
	resetCollections(t, db)

	repo := repository.NewCatalogRepository(db)

	for _, company := range []string{"company-a", "company-b", models.GlobalCompanyID} {
		item := &models.CatalogItem{
			CompanyID: company,
			Kind:      models.KindTruckBody,
			Code:      "BOX",
			Name:      "Box",
			Active:    true,
		}
		if err := repo.Upsert(ctx(), item); err != nil {
			t.Fatalf("company %q could not define BOX: %v", company, err)
		}
	}

	// Each scope resolves its own.
	for _, company := range []string{"company-a", "company-b", models.GlobalCompanyID} {
		item, err := repo.FindByCode(ctx(), company, models.KindTruckBody, "BOX")
		if err != nil {
			t.Fatalf("company %q could not resolve its own BOX: %v", company, err)
		}
		if item.CompanyID != company {
			t.Errorf("company %q resolved an entry owned by %q", company, item.CompanyID)
		}
	}

	// Upserting the same code again updates rather than duplicating.
	again := &models.CatalogItem{
		CompanyID: "company-a",
		Kind:      models.KindTruckBody,
		Code:      "BOX",
		Name:      "Box (renamed)",
		Active:    true,
	}
	if err := repo.Upsert(ctx(), again); err != nil {
		t.Fatalf("re-upsert failed: %v", err)
	}

	items, total, err := repo.List(ctx(), "company-a", models.KindTruckBody, nil, query.Params{PageSize: 50})
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	// The global BOX and company A's BOX; not two of A's.
	if total != 2 {
		t.Errorf("company A sees %d BOX entries, want 2 (its own plus the global)", total)
	}
	for _, item := range items {
		if item.CompanyID == "company-a" && item.Name != "Box (renamed)" {
			t.Errorf("the re-upsert did not update in place: %q", item.Name)
		}
	}
}
