package services

import (
	"regexp"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/karlo/masterdata-service/internal/platform/query"
)

// findOptions turns parsed query parameters into a Mongo find.
//
// Sorts are already resolved against an allowlist by the query package, so the
// field names here cannot come from the caller unchecked — which matters more
// in Mongo than in SQL, where a stray key is a filter operator rather than a
// syntax error.
func findOptions(p query.Params) *options.FindOptions {
	opts := options.Find().
		SetSkip(int64(p.Offset())).
		SetLimit(int64(p.PageSize))

	if len(p.Sorts) > 0 {
		sort := bson.D{}
		for _, s := range p.Sorts {
			direction := 1
			if s.Desc {
				direction = -1
			}
			sort = append(sort, bson.E{Key: s.Field, Value: direction})
		}
		opts.SetSort(sort)
		return opts
	}

	// A stable default. Without one, paging through a collection Mongo returns
	// in natural order can show the same document on two pages and skip
	// another, which reads as data appearing and vanishing.
	return opts.SetSort(bson.D{{Key: "_id", Value: 1}})
}

var regexSpecial = regexp.MustCompile(`[.*+?()\[\]{}|^$\\]`)

// escapeRegex neutralises a user's search string.
//
// Without it a search for "(" is a syntax error and a search for ".*" matches
// everything — and a crafted expression is a denial of service, since Mongo
// evaluates it per document.
func escapeRegex(s string) string {
	return regexSpecial.ReplaceAllString(s, `\$0`)
}

// formatTime renders a BSON timestamp as RFC3339, or empty if it is not one.
func formatTime(v any) string {
	switch t := v.(type) {
	case primitive.DateTime:
		return t.Time().UTC().Format(time.RFC3339)
	case time.Time:
		return t.UTC().Format(time.RFC3339)
	}
	return ""
}
