// Package normalise produces the stored fields that unique indexes are built
// on.
//
// This exists because MongoDB has no functional indexes. PostgreSQL can index
// `upper(regexp_replace(plate, '[^A-Za-z0-9]', ”))` directly, so the database
// itself guarantees that "B 1234 XYZ" and "b1234xyz" collide. MongoDB cannot,
// so the normalised value has to be a real field, written before every save,
// with the unique index on that field instead.
//
// THAT MAKES THIS PACKAGE THE GUARANTEE. Every duplicate the schema exists to
// prevent is prevented here or nowhere: a code path that writes a document
// without calling these functions creates exactly the duplication the whole
// design is built to stop — and does it silently, because the index simply sees
// a missing field and lets the row through.
//
// So there is one place, and everything that writes master data goes through
// it. Applying the rule in a second place is how the two drift apart.
package normalise

import (
	"regexp"
	"strings"
)

var (
	nonAlphanumeric = regexp.MustCompile(`[^A-Za-z0-9]`)
	nonDigits       = regexp.MustCompile(`[^0-9]`)
	collapseSpace   = regexp.MustCompile(`\s+`)
)

// Plate normalises a licence plate.
//
// Upper-cased with every separator removed, so the same vehicle typed three
// ways is one value:
//
//	"B 1234 XYZ" -> "B1234XYZ"
//	"b1234xyz"   -> "B1234XYZ"
//	"B-1234-XYZ" -> "B1234XYZ"
//
// Indonesian plates are written with spaces, dashes or neither depending on who
// is typing, and an operator entering a truck for the second time will not
// reproduce their own earlier spacing.
func Plate(v string) string {
	return strings.ToUpper(nonAlphanumeric.ReplaceAllString(v, ""))
}

// Serial normalises a chassis, engine or hull number — anything that identifies
// a physical object rather than a registration.
//
// Same treatment as a plate: these are transcribed from a metal plate, often
// with the transcriber's own grouping.
func Serial(v string) string {
	return Plate(v)
}

// IMEI and ICCID keep digits only. Both are printed with spaces or dashes on
// the device label and typed either way.
func IMEI(v string) string  { return nonDigits.ReplaceAllString(v, "") }
func ICCID(v string) string { return nonDigits.ReplaceAllString(v, "") }

// Name normalises a human-entered name for uniqueness.
//
// Deliberately gentler than Plate: internal spaces are KEPT, because "Cikarang
// DC" and "CikarangDC" are plausibly different sites, while "Cikarang  DC" with
// a stray space is not a different site from "Cikarang DC". So case is folded
// and whitespace is collapsed, and nothing else is removed.
func Name(v string) string {
	return strings.ToLower(collapseSpace.ReplaceAllString(strings.TrimSpace(v), " "))
}

// Code normalises a short reference an operator types: an item code, a truck
// type code. Upper-cased with separators removed, like a plate — codes are
// written "AB-100" or "AB 100" or "ab100" by different people and mean one
// thing.
func Code(v string) string { return Plate(v) }

// ChannelKey is the stored form of a sensor channel.
//
// An empty string rather than a missing field, because a compound unique index
// does not treat two missing values as equal — so without this an unchannelled
// sensor could be fitted to the same device twice.
func ChannelKey(channel *string) string {
	if channel == nil {
		return ""
	}
	return strings.ToUpper(strings.TrimSpace(*channel))
}

// CompanyKey is gone. It produced a stand-in value so a nullable company id
// could sit inside a compound unique index, because two missing fields do not
// collide. The schema uses two PARTIAL indexes instead — one for global rows,
// one for company rows — which needs no stored value at all.

// Optional returns a pointer to the normalised value, or nil when the input is
// empty — so an absent field stays absent rather than becoming an empty string
// that a partial unique index would then treat as a value.
func Optional(fn func(string) string, v *string) *string {
	if v == nil || strings.TrimSpace(*v) == "" {
		return nil
	}
	out := fn(*v)
	if out == "" {
		return nil
	}
	return &out
}
