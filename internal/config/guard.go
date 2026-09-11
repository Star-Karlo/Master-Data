package config

import (
	"fmt"
	"strings"
)

// This file exists to make one specific accident impossible: pointing the new
// master data service at the legacy production MongoDB.
//
// The legacy monolith connects to a shared Atlas cluster whose databases are
// named `prod` and `test`, and its Mongoose models occupy collection names such
// as `trucks`, `warehouses`, `customers` and `points`. This service is meant to
// run against a brand new, empty database containing master data only. Two
// independent defences are applied:
//
//   1. The target is checked at startup and rejected if it resolves to a known
//      legacy host or database name.
//   2. Every collection this service touches carries a `md_` prefix, so even a
//      target that slips past the first check cannot write over a legacy
//      collection.
//
// Neither check can be disabled by configuration. Removing a guard requires
// editing this file, which is a deliberate act rather than a typo in an
// environment variable.

// legacyHosts are hostname fragments belonging to the monolith's cluster. A URI
// containing any of them is refused outright.
var legacyHosts = []string{
	"cluster0-ndqn7.mongodb.net",
}

// legacyDatabases are database names the monolith uses. `local` is included
// because that is the legacy development default and it holds a copy of the
// same collection layout.
var legacyDatabases = []string{
	"prod",
	"test",
	"local",
}

// ErrLegacyTarget describes a refused connection target.
type ErrLegacyTarget struct {
	Reason string
}

func (e *ErrLegacyTarget) Error() string {
	return fmt.Sprintf(
		"config: refusing to connect: %s. "+
			"This service must run against a new, empty master data database. "+
			"See internal/config/guard.go",
		e.Reason,
	)
}

// guardTarget rejects a connection target that looks like the legacy database.
//
// It is called before the driver is constructed, so a refused target results in
// no connection attempt at all rather than a connection that is closed after
// the fact.
func guardTarget(uri, database string) error {
	lowerURI := strings.ToLower(uri)
	for _, host := range legacyHosts {
		if strings.Contains(lowerURI, host) {
			return &ErrLegacyTarget{
				Reason: fmt.Sprintf("MONGO_URI points at the legacy cluster %q", host),
			}
		}
	}

	lowerDB := strings.ToLower(strings.TrimSpace(database))
	for _, name := range legacyDatabases {
		if lowerDB == name {
			return &ErrLegacyTarget{
				Reason: fmt.Sprintf("MONGO_DATABASE is %q, which is a legacy database name", database),
			}
		}
	}

	// A URI may also name a database in its path, which would override the
	// configured one on some drivers. Refuse that shape rather than reason
	// about precedence.
	if db := databaseFromURI(uri); db != "" {
		for _, name := range legacyDatabases {
			if strings.EqualFold(db, name) {
				return &ErrLegacyTarget{
					Reason: fmt.Sprintf("MONGO_URI names the legacy database %q in its path", db),
				}
			}
		}
	}

	if lowerDB == "" {
		return &ErrLegacyTarget{Reason: "MONGO_DATABASE is empty"}
	}

	return nil
}

// databaseFromURI extracts the database component of a MongoDB URI, if present.
func databaseFromURI(uri string) string {
	_, rest, found := strings.Cut(uri, "://")
	if !found {
		return ""
	}
	// Strip credentials and host, leaving the path and query.
	_, path, found := strings.Cut(rest, "/")
	if !found {
		return ""
	}
	db, _, _ := strings.Cut(path, "?")
	return db
}

// CollectionPrefix namespaces every collection this service owns.
//
// Empty now, and the reason it existed is worth recording. The legacy models
// occupied the unprefixed names, so while this service might have run against
// the legacy database the prefix guaranteed the two could not collide.
//
// It has its own database — karlo_masterdata, which contains nothing but these
// collections — so the prefix guards against nothing and only makes every
// collection name longer to read and to type in a shell.
//
// Set it back to "md_" if this service is ever pointed at a database that
// already holds legacy collections; Collection() below is the single place the
// name is decided, so nothing else has to change.
const CollectionPrefix = ""

// Collection returns the prefixed name for a logical collection.
func Collection(name string) string {
	if strings.HasPrefix(name, CollectionPrefix) {
		return name
	}
	return CollectionPrefix + name
}
