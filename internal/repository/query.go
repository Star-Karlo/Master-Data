package repository

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/karlo/masterdata-service/internal/platform/query"
	"go.mongodb.org/mongo-driver/bson"
)

// applyFilters folds normalised filters into a Mongo filter document.
//
// Field names have already passed a repository allowlist, so they are safe as
// keys. Values are typed here rather than passed through as strings, because
// Mongo compares types strictly: the string "true" does not match the boolean
// true, and "2024" does not match the number 2024.
func applyFilters(filter bson.M, p query.Params) {
	for _, f := range p.Filters {
		value := coerce(f.Value)

		switch f.Operator {
		case query.OpNeq:
			filter[f.Field] = bson.M{"$ne": value}
		case query.OpLike:
			filter[f.Field] = regexSearch(f.Value)
		case query.OpIn:
			parts := strings.Split(f.Value, ",")
			values := make([]interface{}, 0, len(parts))
			for _, part := range parts {
				values = append(values, coerce(strings.TrimSpace(part)))
			}
			filter[f.Field] = bson.M{"$in": values}
		case query.OpGt:
			filter[f.Field] = bson.M{"$gt": value}
		case query.OpGte:
			filter[f.Field] = bson.M{"$gte": value}
		case query.OpLt:
			filter[f.Field] = bson.M{"$lt": value}
		case query.OpLte:
			filter[f.Field] = bson.M{"$lte": value}
		case query.OpBetween:
			bounds := strings.SplitN(f.Value, ",", 2)
			if len(bounds) == 2 {
				filter[f.Field] = bson.M{"$gte": coerce(bounds[0]), "$lte": coerce(bounds[1])}
			}
		default:
			filter[f.Field] = value
		}
	}
}

// sortDocument builds the sort specification, falling back when the caller
// asked for none.
func sortDocument(p query.Params, fallback bson.D) bson.D {
	if len(p.Sorts) == 0 {
		return fallback
	}
	out := make(bson.D, 0, len(p.Sorts))
	for _, s := range p.Sorts {
		dir := 1
		if s.Desc {
			dir = -1
		}
		out = append(out, bson.E{Key: s.Field, Value: dir})
	}
	return out
}

// regexSearch builds a case-insensitive contains match.
//
// The input is quoted first. Without that, a search for "a(b" is an invalid
// pattern that errors, and a search for ".*" scans the whole collection under
// the guise of a filter.
func regexSearch(term string) bson.M {
	return bson.M{"$regex": regexp.QuoteMeta(term), "$options": "i"}
}

// coerce converts a query-string value to the type Mongo will compare against.
func coerce(s string) interface{} {
	switch strings.ToLower(s) {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}
