package unit

import (
	"testing"

	"github.com/karlo/masterdata-service/internal/models"
)

// TestBeforeWriteFillsTheFieldsTheIndexesUse is the test that keeps the schema
// working.
//
// Every unique index in this database is on a NORMALISED field, because MongoDB
// has no functional indexes. Those fields are written by BeforeWrite and
// nowhere else. If a model stops filling one, the partial index simply ignores
// the document and duplicates are created silently — no error, no rejection,
// just two records for one truck.
func TestBeforeWriteFillsTheFieldsTheIndexesUse(t *testing.T) {
	v := &models.Vehicle{CompanyID: "c1", LicensePlate: "B 1234 XYZ"}
	chassis := "mh1-234-567"
	v.ChassisNumber = &chassis
	v.BeforeWrite()

	if v.PlateNormalised != "B1234XYZ" {
		t.Errorf("plateNormalised is %q; the unique index is on this field, so a "+
			"wrong value means the duplicate check does not happen", v.PlateNormalised)
	}
	if v.ChassisNormalised == nil || *v.ChassisNormalised != "MH1234567" {
		t.Error("chassisNormalised must be filled: it is what makes the same physical " +
			"vehicle impossible to register at two companies")
	}
	// A vehicle with no unit type would be excluded from every "show me my
	// trailers" query rather than defaulting sensibly.
	if v.UnitType != models.UnitRigid {
		t.Errorf("unitType must default to rigid, got %q", v.UnitType)
	}
}

// TestPlateThatNormalisesToNothingIsRefused covers a hole in the constraint the
// schema leans on hardest.
//
// "---" normalises to an empty string. Stored, it would sit outside the unique
// index's reach — and two such vehicles would both be accepted.
func TestPlateThatNormalisesToNothingIsRefused(t *testing.T) {
	v := &models.Vehicle{CompanyID: "c1", LicensePlate: "---"}
	v.BeforeWrite()
	if err := v.Validate(); err == nil {
		t.Error("a plate of pure punctuation normalises to nothing and would escape " +
			"the uniqueness rule, so it must be refused")
	}
}

// TestGlobalAndCompanyEntriesNormaliseAlike covers the reference lists.
//
// A global entry Karlo maintains and a company's own addition live in one
// collection. Two partial unique indexes separate them — unique on the name
// where companyId is null, unique on (companyId, name) where it is not — so two
// globals sharing a name collide while a company may still use a name a global
// entry already has. Verified against the live database.
//
// What the models must guarantee is that both sides normalise the name the SAME
// way; if they did not, the two indexes would be comparing different values and
// neither rule would hold.
func TestGlobalAndCompanyEntriesNormaliseAlike(t *testing.T) {
	global := &models.Brand{Name: "Hino"}
	global.BeforeWrite()

	owned := &models.Brand{Name: "  HINO  "}
	company := "11111111-1111-1111-1111-111111111111"
	owned.CompanyID = &company
	owned.BeforeWrite()

	if owned.NameNormalised != global.NameNormalised {
		t.Errorf("the same name must normalise identically regardless of owner: "+
			"%q versus %q", owned.NameNormalised, global.NameNormalised)
	}
	if global.NameNormalised != "hino" {
		t.Errorf("expected the folded form, got %q", global.NameNormalised)
	}
}

// TestChannelKeyIsFilledForUnchannelledSensors covers the other place a missing
// field would defeat a unique index.
func TestChannelKeyIsFilledForUnchannelledSensors(t *testing.T) {
	s := &models.TrackerSensor{TrackerID: "t1", SensorTypeID: "fuel"}
	s.BeforeWrite()
	if s.ChannelKey != "" {
		t.Errorf("expected an empty channel key, got %q", s.ChannelKey)
	}
	// The point is that it is PRESENT and comparable, not absent — an absent
	// field is skipped by the index and the same sensor could be fitted twice.
	ain1 := "ain1"
	s2 := &models.TrackerSensor{TrackerID: "t1", SensorTypeID: "fuel", Channel: &ain1}
	s2.BeforeWrite()
	if s2.ChannelKey != "AIN1" {
		t.Errorf("channel case must fold, got %q", s2.ChannelKey)
	}
}
