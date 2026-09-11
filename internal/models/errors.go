package models

import "errors"

// Rules the database cannot express, enforced in Go.
//
// $jsonSchema can require a field and check its type, but it cannot compare one
// field against another — so anything conditional is here, and callers must
// invoke Validate before writing.
var (
	ErrPlateRequired = errors.New(
		"a licence plate is required, and it must contain at least one letter or digit: " +
			"the unique index is on the normalised form, and a plate that normalises to " +
			"nothing would slip past it")

// ErrHeadCarriesNothing and ErrOnlyBodiesAreCoupled are gone with the fields
// they policed. The cargo figures moved to the body TYPE, so a head has nowhere
// to record a weight it could not carry; and the coupling became a field the
// database itself keeps unique. A rule enforced by the shape of the data needs
// no code behind it.
)
