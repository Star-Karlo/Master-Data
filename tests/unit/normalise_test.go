package unit

import (
	"testing"

	"github.com/karlo/masterdata-service/internal/platform/normalise"
)

// TestPlateNormalisationCatchesWhatTheIndexCannot is the test that guards the
// whole schema.
//
// MongoDB has no functional indexes, so uniqueness on a licence plate is
// enforced against a STORED normalised field. That makes this function the
// guarantee: if it stops folding these variants together, the unique index
// silently starts accepting duplicates and one truck becomes several records —
// which is the exact failure the master data schema exists to prevent.
//
// The old TMS service had a unique index on the raw plate and accepted
// "B 1234 XYZ", "b1234xyz" and "B1234XYZ" as three separate trucks.
func TestPlateNormalisationCatchesWhatTheIndexCannot(t *testing.T) {
	sameTruck := []string{
		"B 1234 XYZ",
		"b1234xyz",
		"B1234XYZ",
		"B-1234-XYZ",
		"  b 1234  xyz  ",
	}

	want := normalise.Plate(sameTruck[0])
	for _, spelling := range sameTruck[1:] {
		if got := normalise.Plate(spelling); got != want {
			t.Errorf("%q normalised to %q, but %q gives %q — the unique index "+
				"would accept both as separate vehicles",
				spelling, got, sameTruck[0], want)
		}
	}

	// And genuinely different plates must stay different, or the index refuses
	// a truck somebody legitimately owns.
	if normalise.Plate("B 1234 XYZ") == normalise.Plate("B 1234 XYY") {
		t.Error("two different plates must not collapse to one value")
	}
}

// TestNameNormalisationIsGentlerThanPlate covers a deliberate difference.
//
// A plate strips every separator, because "B1234XYZ" is the same registration
// however it is spaced. A NAME keeps its internal spaces, because "Cikarang DC"
// and "CikarangDC" are plausibly two different sites — while a stray double
// space is not.
func TestNameNormalisationIsGentlerThanPlate(t *testing.T) {
	if normalise.Name("Cikarang  DC") != normalise.Name("cikarang dc") {
		t.Error("case and repeated whitespace must not create a second site")
	}
	if normalise.Name("Cikarang DC") == normalise.Name("CikarangDC") {
		t.Error("removing the space entirely makes a different name; a name is " +
			"not a plate, and collapsing them would refuse a site somebody meant to create")
	}
}

// TestChannelKeyMakesAbsenceComparable covers why the stored key exists.
//
// A compound unique index does not treat two missing fields as equal, so an
// unchannelled sensor could otherwise be fitted to the same device twice.
func TestChannelKeyMakesAbsenceComparable(t *testing.T) {
	if normalise.ChannelKey(nil) != "" {
		t.Error("a missing channel must become an empty string, not stay absent")
	}
	ain1, lower := "AIN1", "ain1"
	if normalise.ChannelKey(&ain1) != normalise.ChannelKey(&lower) {
		t.Error("channel case must not create a second fitting")
	}
}

// TestOptionalLeavesAbsentFieldsAbsent covers the partial indexes.
//
// A partial unique index keys on rows where the field EXISTS. Turning an empty
// input into an empty string would put every such row into the index and make
// them all collide with each other.
func TestOptionalLeavesAbsentFieldsAbsent(t *testing.T) {
	if normalise.Optional(normalise.Serial, nil) != nil {
		t.Error("a nil input must stay nil")
	}
	if normalise.Optional(normalise.Serial, ptr("   ")) != nil {
		t.Error("whitespace is not a value; it must stay absent rather than " +
			"becoming an empty string that collides with every other blank")
	}
	if got := normalise.Optional(normalise.Serial, ptr("mh1-234")); got == nil || *got != "MH1234" {
		t.Errorf("a real value must normalise, got %v", got)
	}
}

func ptr(s string) *string { return &s }

// TestIMEIKeepsDigitsOnly covers the device identifier, which is printed on a
// label with spaces and typed either way.
func TestIMEIKeepsDigitsOnly(t *testing.T) {
	if normalise.IMEI("860 123 456 789 012") != "860123456789012" {
		t.Error("an IMEI must fold to digits, or the same device registers twice")
	}
}
