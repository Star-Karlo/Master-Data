package unit

import (
	"testing"

	"github.com/karlo/masterdata-service/internal/models"
	"github.com/karlo/masterdata-service/internal/services"
)

// TestApplyPICs covers the warehouse contact list: one default always, the
// legacy single-contact form editing that default, and the mirrored
// sitePicName / sitePicPhone that older readers depend on.
func TestApplyPICs(t *testing.T) {
	t.Run("a list without a flagged default makes the first one default", func(t *testing.T) {
		site := &models.Site{}
		in := services.SiteInput{PICs: &[]services.SitePICInput{{Name: "Budi", Phone: "0811"}, {Name: "Sari", Phone: "0812"}}}
		if err := services.ApplyPICs(site, in); err != nil {
			t.Fatal(err)
		}
		if len(site.PICs) != 2 || !site.PICs[0].IsDefault || site.PICs[1].IsDefault {
			t.Fatalf("default not on the first entry: %+v", site.PICs)
		}
		if site.PICs[0].ID == "" || site.PICs[1].ID == "" {
			t.Fatalf("entries need ids: %+v", site.PICs)
		}
		if site.SitePICName == nil || *site.SitePICName != "Budi" || site.SitePICPhone == nil || *site.SitePICPhone != "0811" {
			t.Fatalf("legacy mirror not the default: %v %v", site.SitePICName, site.SitePICPhone)
		}
	})

	t.Run("only one default survives, the first flagged", func(t *testing.T) {
		site := &models.Site{}
		in := services.SiteInput{PICs: &[]services.SitePICInput{{Name: "A", IsDefault: true}, {Name: "B", IsDefault: true}}}
		if err := services.ApplyPICs(site, in); err != nil {
			t.Fatal(err)
		}
		if !site.PICs[0].IsDefault || site.PICs[1].IsDefault {
			t.Fatalf("expected only A default: %+v", site.PICs)
		}
	})

	t.Run("blank rows are dropped and a phone without a name is refused", func(t *testing.T) {
		site := &models.Site{}
		if err := services.ApplyPICs(site, services.SiteInput{PICs: &[]services.SitePICInput{{Name: "", Phone: ""}, {Name: "A"}}}); err != nil {
			t.Fatal(err)
		}
		if len(site.PICs) != 1 {
			t.Fatalf("blank row kept: %+v", site.PICs)
		}
		if err := services.ApplyPICs(&models.Site{}, services.SiteInput{PICs: &[]services.SitePICInput{{Phone: "0811"}}}); err == nil {
			t.Fatal("a phone without a name should be refused")
		}
	})

	t.Run("the legacy single fields edit the default entry", func(t *testing.T) {
		site := &models.Site{PICs: []models.SitePIC{{ID: "x", Name: "Old", Phone: "0800", IsDefault: true}, {ID: "y", Name: "Other"}}}
		if err := services.ApplyPICs(site, services.SiteInput{PICName: "New", PICPhone: "0899"}); err != nil {
			t.Fatal(err)
		}
		if site.PICs[0].Name != "New" || site.PICs[0].Phone != "0899" || len(site.PICs) != 2 {
			t.Fatalf("default not edited in place: %+v", site.PICs)
		}
		if *site.SitePICName != "New" {
			t.Fatalf("mirror stale: %v", *site.SitePICName)
		}
	})

	t.Run("an empty list clears the mirror", func(t *testing.T) {
		site := &models.Site{PICs: []models.SitePIC{{ID: "x", Name: "Old", IsDefault: true}}}
		if err := services.ApplyPICs(site, services.SiteInput{PICs: &[]services.SitePICInput{}}); err != nil {
			t.Fatal(err)
		}
		if len(site.PICs) != 0 || site.SitePICName != nil {
			t.Fatalf("expected no PICs and no mirror: %+v %v", site.PICs, site.SitePICName)
		}
	})
}
