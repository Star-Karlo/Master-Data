package authctx

import (
	_ "embed"
	"encoding/json"
)

// The registry of sellable entitlements.
//
// A "feature" is what a Karlo admin grants a company. Permissions in the
// catalogues gate against these names, and HasPermission refuses any permission
// whose gating feature the company does not hold. That makes the names
// load-bearing in a way that is easy to miss: a feature misspelled in a
// catalogue gates against something no company can ever be granted, so the
// permission is silently dead. Nothing errors, no test fails, and the key
// simply never matches — which is the same shape as the stale-permission
// problem the catalogue was designed to prevent, arriving from the other side.
//
// Declaring the sellable set here, rather than deriving it from the catalogues,
// is what makes that detectable. FeaturesFor() reads the catalogue and so can
// only ever agree with it; this list is written independently, and the test
// that compares the two is the check.
//
// It also answers a question the catalogue cannot: what is there to sell? An
// entitlement administration screen needs the list of features a company could
// hold, including any not yet referenced by a permission.

// Feature is one sellable entitlement.
type Feature struct {
	// Name is the value stored in company_modules.module.
	//
	// Tagged because this struct is served to the entitlement editor. Without
	// tags Go marshals the field names verbatim — Name, Owner, Roadmap — which
	// the client reads as undefined and renders as an empty list.
	Name string `json:"name"`

	// Owner is the product whose entitlement this is. ProductShared marks the
	// entitlements belonging to neither product — accounting and telemetry are
	// separate services both products consume, and BuildAccess folds shared
	// entitlements into every product's feature list.
	Owner Product `json:"owner"`

	// Description is what the company gets, for an administration screen.
	Description string `json:"description,omitempty"`

	// Parent nests a feature under another for presentation, matching the
	// product's own sidebar. It is presentation only: granting a parent does
	// NOT imply its children here, because entitlement is stored one row per
	// feature and BuildAccess reads those rows literally. If that should ever
	// change, it is a decision about what a sale includes, not a detail of
	// this struct.
	Parent string `json:"parent,omitempty"`

	// Roadmap marks a feature the product has declared but not built. It can
	// be pre-enabled for a company, but nothing gates on it yet.
	Roadmap bool `json:"roadmap,omitempty"`
}

// tmsFeatures are the entitlements a Karlo admin can grant for TMS.
var tmsFeatures = []Feature{
	{Name: "order", Owner: ProductTMS, Description: "Place, approve and track orders"},
	{Name: "agreement", Owner: ProductTMS, Description: "Rate agreements between shipper and transporter"},
	{Name: "invoice", Owner: ProductTMS, Description: "Billing and settlement"},
	{Name: "shipment", Owner: ProductTMS, Description: "Loading, transit and unloading"},
	{Name: "truck", Owner: ProductTMS, Description: "Fleet register and driver pairing"},
	{Name: "warehouse", Owner: ProductTMS, Description: "Warehouse register and geofencing"},
	{Name: "customer", Owner: ProductTMS, Description: "The company's own customer register"},
	{Name: "dashboard", Owner: ProductTMS, Description: "Operational dashboards and summaries"},
	{Name: "notification", Owner: ProductTMS, Description: "In-app, email and WhatsApp notifications"},
}

// sharedFeatures belong to neither product.
var sharedFeatures = []Feature{
	{Name: "accounting", Owner: ProductShared, Description: "The separately sold accounting service"},
	{Name: "telemetry", Owner: ProductShared, Description: "The separately sold telemetry service, by tier"},
}

// fmsFeatures are the entitlements a Karlo admin can grant for FMS.
//
// They are NOT written here. FMS owns this list, and a hand-copied mirror is
// drift waiting to happen: rename a feature there and a token minted here goes
// on describing an entitlement that no longer exists, with nothing to notice.
// So the list is loaded from fms-features.json, which is a verbatim copy of
// FMS's own features.Registry — updating the mirror is a file copy, and a test
// diffs it against the upstream repository when that is present on disk.
//
// Entries marked dev are roadmap stubs with no working page and no permission
// keys behind them. They are excluded rather than filtered by the caller: a
// company can pre-enable one in FMS's own configurator, but nothing here should
// treat it as sellable, and an FMS permission gating on one would be a mistake
// this file should not quietly accommodate.
//
//go:embed fms-features.json
var fmsRegistryJSON []byte

// fmsDef mirrors FMS's features.Def, field for field, so the JSON they generate
// deserialises here without translation.
type fmsDef struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Parent string `json:"parent"`
	Dev    bool   `json:"dev,omitempty"`
}

// FMSRegistry returns the mirrored FMS registry exactly as published, roadmap
// stubs included. Use it to compare against upstream; use SellableFeatures for
// what can actually be granted.
func FMSRegistry() []Feature {
	return decodeFMSRegistry(false)
}

func decodeFMSRegistry(liveOnly bool) []Feature {
	var defs []fmsDef
	if err := json.Unmarshal(fmsRegistryJSON, &defs); err != nil {
		// The file is embedded at build time and comes from a generator, so a
		// parse failure is a broken build rather than bad input. Failing here
		// beats serving a silently empty FMS entitlement set, which would deny
		// every gated FMS permission.
		panic("authctx: fms-features.json is not valid: " + err.Error())
	}

	out := make([]Feature, 0, len(defs))
	for _, d := range defs {
		if liveOnly && d.Dev {
			continue
		}
		out = append(out, Feature{
			Name:        d.Key,
			Owner:       ProductFMS,
			Description: d.Label,
			Parent:      d.Parent,
			Roadmap:     d.Dev,
		})
	}
	return out
}

var fmsFeatures = decodeFMSRegistry(true)

// featureRegistry indexes every declared feature by product.
//
// Shared entitlements appear under every product, because a permission gated by
// `accounting` resolves in whichever product the user is in — that is the point
// of them being shared.
var featureRegistry = func() map[Product]map[string]Feature {
	out := map[Product]map[string]Feature{
		ProductTMS:    {},
		ProductFMS:    {},
		ProductShared: {},
	}
	for _, f := range tmsFeatures {
		out[ProductTMS][f.Name] = f
	}
	for _, f := range fmsFeatures {
		out[ProductFMS][f.Name] = f
	}
	for _, f := range sharedFeatures {
		out[ProductShared][f.Name] = f
		out[ProductTMS][f.Name] = f
		out[ProductFMS][f.Name] = f
	}
	return out
}()

// IsSellableFeature reports whether a feature is one a company can be granted
// in a product. Use it when validating an entitlement grant, so a typo is
// refused where it is made rather than becoming a row that gates nothing.
func IsSellableFeature(product Product, name string) bool {
	_, ok := featureRegistry[product][name]
	return ok
}

// SellableFeatures returns everything a Karlo admin can grant a company for a
// product, shared entitlements included, for an administration screen.
func SellableFeatures(product Product) []Feature {
	out := make([]Feature, 0, len(featureRegistry[product]))
	for _, f := range featureRegistry[product] {
		out = append(out, f)
	}
	return out
}
