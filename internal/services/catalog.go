// Package services holds master data's business rules.
//
// Its shape is decided by one fact: the MODEL was rewritten and the CONTRACT
// was not. Callers — the TMS frontend and the business service — still speak of
// trucks, warehouses and a catalogue addressed by `kind`, while the model now
// has vehicles, sites and a collection per reference list.
//
// Translating here rather than migrating every caller is deliberate. The old
// vocabulary is what the proto and the frontend already agree on, and the two
// changes are separable: this layer can be correct today, and callers can move
// to the new nouns one at a time. What it must NOT do is let the old shape
// leak back into storage — every write goes through repository.Store so
// BeforeWrite fills the normalised fields the unique indexes are built on.
package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/karlo/masterdata-service/internal/config"
	"github.com/karlo/masterdata-service/internal/platform/cache"
	"github.com/karlo/masterdata-service/internal/platform/query"
)

var (
	// ErrNotFound is a document that does not exist, or that this company may
	// not see — the two are answered identically on purpose, so a caller
	// cannot probe another company's ids by watching which error comes back.
	ErrNotFound = errors.New("not found")

	// ErrValidation is a rejected input.
	ErrValidation = errors.New("validation failed")

	// ErrUnknownKind is a catalogue name this service does not serve. Distinct
	// from ErrNotFound: the caller asked for a LIST that does not exist, which
	// is a client bug, not a missing row.
	ErrUnknownKind = errors.New("unknown catalogue")
)

// CatalogEntry is the common envelope every reference list is returned in.
//
// The lists genuinely differ — a brand has no dimensions, a body has no logo —
// so the shared fields are promoted and the rest travels in Attributes. That is
// the same bargain the proto's CatalogItem makes, and keeping the HTTP and gRPC
// shapes identical means one mapping to get right instead of two.
type CatalogEntry struct {
	ID        string         `json:"id"`
	Kind      string         `json:"kind"`
	CompanyID *string        `json:"companyId,omitempty"`
	Code      string         `json:"code,omitempty"`
	Name      string         `json:"name"`
	Desc      string         `json:"description,omitempty"`
	Active    bool           `json:"active"`
	Attrs     map[string]any `json:"attributes,omitempty"`
	CreatedAt string         `json:"createdAt,omitempty"`
	UpdatedAt string         `json:"updatedAt,omitempty"`
}

// kindSpec says which collection a catalogue name reads from, and how to shape
// a document from it.
//
// A table rather than a switch because the same mapping is needed by three
// callers — HTTP list, gRPC list and reference validation — and three switches
// drift. Adding a catalogue is one row here.
type kindSpec struct {
	collection string
	// parentField is the document field a hierarchical list nests on, which is
	// what `parentId` filters against. Empty for a flat list.
	parentField string

	// parentRequired marks a list whose entries cannot exist without a parent.
	//
	// A sub-category with no category is not a sub-category of anything, and
	// the collection validator says so too — but a validator rejection arrives
	// as an opaque write error and surfaces as a 500. Checking it here turns
	// that into a sentence the person filling in the form can act on.
	parentRequired bool

	// requiredFields are the fields an entry cannot be written without.
	//
	// Most lists identify an entry by `name`, but not all: a tracker model is a
	// vendor and a model, and a sensor type is a code and a name. Stating it
	// per list means the check matches the collection validator instead of
	// assuming every catalogue looks the same.
	requiredFields []string

	// owned marks a list whose documents carry a companyId at all. The two
	// device catalogues do not — they predate the Owned envelope and belong to
	// Karlo by construction.
	owned bool

	// shareable marks a list whose entries may be PLATFORM-GLOBAL — owned by
	// nobody and visible to everyone.
	//
	// Not every list can be. A vehicle group is one company's way of grouping
	// its own fleet, and an item is a thing one company ships; both carry a
	// required companyId in the model, and a "shared" one would be a document
	// nobody owns describing something that belongs to somebody. The flag
	// mirrors that: it is Owned-versus-Base in the model, stated where the
	// write path can act on it.
	shareable bool
}

// catalogKinds is every reference list this service serves, by the name callers
// already use.
//
// Names that the OLD model had and this one does not are absent rather than
// faked. `truckType` in particular: the previous model had one list of truck
// types, and the current one splits the question into truck heads and truck
// bodies because a rigid truck has a body and no head. Returning one of them
// under the old name would answer plausibly and wrongly, so the name is
// refused and the caller has to say which it means.
var catalogKinds = map[string]kindSpec{
	"brand":        {collection: "brands", owned: true, shareable: true},
	"cargoType":    {collection: "cargo_types", owned: true, shareable: true},
	"itemCategory": {collection: "item_categories", owned: true, shareable: true},

	// The parent fields are the model's bson names, not names invented here.
	// They were `itemCategoryId` and `itemSubCategoryId`, which the documents
	// do not have — so filtering a hierarchy by its parent matched nothing and
	// returned an empty list rather than an error.
	"itemSubCategory": {collection: "item_sub_categories", owned: true, parentField: "categoryId", parentRequired: true, shareable: true},
	"item":            {collection: "items", owned: true, parentField: "subCategoryId"},

	"truckHead":  {collection: "truck_heads", owned: true, shareable: true},
	"truckBody":  {collection: "truck_bodies", owned: true, shareable: true},
	"truckClass": {collection: "truck_classes", owned: true, shareable: true},

	// Company-owned by nature: a group is how ONE company organises its fleet.
	"vehicleGroup": {collection: "vehicle_groups", owned: true},

	// A company's own consignee register — the end-recipients its orders are
	// delivered to. Owned and NOT shareable: one transporter's customer list is
	// its commercial relationships, and Karlo has no business publishing it to
	// every other company on the platform.
	"customer": {collection: "customers", owned: true},

	// Karlo-maintained device catalogues. They predate the Owned envelope and
	// carry no companyId at all, so every entry is global by construction —
	// shareable is false because there is no owner to choose, not because
	// sharing is refused.
	"trackerModel": {collection: "tracker_models", requiredFields: []string{"vendor", "model"}},
	"sensorType":   {collection: "sensor_types", requiredFields: []string{"code", "name"}},
}

// Shareable reports whether a catalogue accepts platform-global entries, so a
// client can offer "shared with everyone" only where it is possible.
func Shareable(kind string) bool { return catalogKinds[kind].shareable }

// CatalogKinds lists the catalogues this service serves, for a client that
// wants to populate a picker without hardcoding the set.
func CatalogKinds() []string {
	out := make([]string, 0, len(catalogKinds))
	for kind := range catalogKinds {
		out = append(out, kind)
	}
	return out
}

// CatalogService reads and writes the reference lists.
type CatalogService struct {
	db      *mongo.Database
	writers map[string]writer
	cache   cache.Cache
}

// catalogTTL bounds how stale a catalogue read can be if an invalidation is
// missed (a write from outside this service, say a migration script). Writes
// through this service invalidate explicitly, so in practice a change is
// visible on the next read.
const catalogTTL = 15 * time.Minute

func NewCatalogService(db *mongo.Database, c cache.Cache) *CatalogService {
	if c == nil {
		c = cache.NewNoop()
	}
	return &CatalogService{db: db, writers: newWriters(db), cache: c}
}

// catalogScope is the tenant segment of a cache key. See docs/shared/CACHING.md:
// a key without it would let one company's read populate an entry another
// company then receives, and the database's own scoping never gets a say
// because the second request never reaches it. Platform staff see everything,
// so their reads live under their own segment rather than any company's.
func catalogScope(companyID string, platformStaff bool) string {
	switch {
	case platformStaff:
		return "staff"
	case companyID == "":
		return "global"
	default:
		return companyID
	}
}

func catalogEntryKey(scope, kind, id string) string {
	return cache.Key("masterdata", "catalog", scope, kind, "id", id)
}

func catalogListKey(scope, kind, parentID string, p query.Params) string {
	var b strings.Builder
	b.WriteString(p.Search)
	for _, f := range p.Filters {
		fmt.Fprintf(&b, "|f:%s:%v:%s", f.Field, f.Operator, f.Value)
	}
	for _, so := range p.Sorts {
		fmt.Fprintf(&b, "|s:%s:%t", so.Field, so.Desc)
	}
	if parentID == "" {
		parentID = "-"
	}
	return cache.Key("masterdata", "catalog", scope, kind, "list", parentID,
		fmt.Sprint(p.Page), fmt.Sprint(p.PageSize), cache.Fingerprint(b.String()))
}

// invalidate drops every cached read of a kind that a write could have
// changed. A company's write touches only that company's listings and the
// staff view; a global write (companyID empty) appears in every company's
// listing, so it sweeps the kind across all scopes. Blunt, but a global
// catalogue edit is rare, and serving the change to some tenants and not
// others is worse than a moment of cache misses.
func (s *CatalogService) invalidate(ctx context.Context, kind, companyID string, platformStaff bool) {
	if platformStaff || companyID == "" {
		// Staff may have written a global entry, which every scope lists;
		// the kind sits after the scope in the key, so the sweep has to
		// cover all of them.
		s.cache.DeleteByPrefix(ctx, cache.Prefix("masterdata", "catalog"))
		return
	}
	s.cache.DeleteByPrefix(ctx, cache.Prefix("masterdata", "catalog", companyID, kind))
	s.cache.DeleteByPrefix(ctx, cache.Prefix("masterdata", "catalog", "staff", kind))
}

type cachedList struct {
	Entries []CatalogEntry `json:"entries"`
	Total   int64          `json:"total"`
}

// Writable reports whether a catalogue accepts writes, so a client can render
// the add button only where it would work.
func (s *CatalogService) Writable(kind string) bool {
	_, ok := s.writers[kind]
	return ok
}

// scopeFor decides who owns what is being written.
//
// An ordinary caller always writes their OWN company's private entries, and the
// requested owner is ignored — a company that could name another would publish
// into that company's pickers, or into everyone's.
//
// Platform staff choose, because choosing across tenants is what the flag
// means. They may write a SHARED entry (nil, visible to every company) or one
// owned by a named company — recording a client's own cargo type on their
// behalf, for instance, which is a thing support does.
//
// requestedOwner is the payload's companyId: empty means shared.
func scopeFor(callerCompanyID string, platformStaff bool, requestedOwner string) *string {
	if !platformStaff {
		return &callerCompanyID
	}
	if strings.TrimSpace(requestedOwner) == "" {
		return nil
	}
	owner := strings.TrimSpace(requestedOwner)
	return &owner
}

// scopeForEdit says which entries a caller may change: nil for staff, who may
// change anything, and the caller's own company for everyone else.
func scopeForEdit(companyID string, platformStaff bool) *string {
	if platformStaff {
		return nil
	}
	return &companyID
}

// Create adds an entry to a catalogue.
func (s *CatalogService) Create(ctx context.Context, kind, companyID string, platformStaff bool, payload map[string]any) (string, error) {
	w, ok := s.writers[kind]
	if !ok {
		if _, readable := catalogKinds[kind]; readable {
			return "", fmt.Errorf("%w: %q is read-only", ErrValidation, kind)
		}
		return "", fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
	spec := catalogKinds[kind]

	required := spec.requiredFields
	if len(required) == 0 {
		required = []string{"name"}
	}
	for _, field := range required {
		if value, _ := payload[field].(string); strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("%w: %s is required", ErrValidation, field)
		}
	}

	if spec.parentRequired {
		if parent, _ := payload[spec.parentField].(string); strings.TrimSpace(parent) == "" {
			return "", fmt.Errorf("%w: choose a parent first — %s cannot be left empty",
				ErrValidation, spec.parentField)
		}
	}

	// A catalogue with no companyId field takes no owner: writing one would
	// add a field the validator does not declare.
	if !spec.owned {
		if !platformStaff {
			return "", fmt.Errorf(
				"%w: %q is maintained by Karlo", ErrForbidden, kind)
		}
		return w.Create(ctx, nil, false, payload)
	}

	requested, _ := payload["companyId"].(string)
	owner := scopeFor(companyID, platformStaff, requested)

	// A shared entry on a list that cannot be shared would be a document owned
	// by nobody, describing something that belongs to somebody. Refused with
	// the reason rather than silently written to the caller's own company,
	// which would look like it worked and put the entry in the wrong place.
	if owner == nil && !catalogKinds[kind].shareable {
		return "", fmt.Errorf(
			"%w: %q entries belong to one company and cannot be shared — name an owner",
			ErrValidation, kind)
	}

	id, err := w.Create(ctx, owner, true, payload)
	if err == nil {
		s.invalidate(ctx, kind, companyID, platformStaff)
	}
	return id, err
}

// Update changes an entry the caller owns.
func (s *CatalogService) Update(ctx context.Context, kind, id, companyID string, platformStaff bool, payload map[string]any) error {
	w, ok := s.writers[kind]
	if !ok {
		return fmt.Errorf("%w: %q is read-only", ErrValidation, kind)
	}
	// Update scopes by the CALLER, never by the payload: which entries you may
	// change is not a thing the request gets to say.
	if err := w.Update(ctx, id, scopeForEdit(companyID, platformStaff), payload); err != nil {
		return err
	}
	s.invalidate(ctx, kind, companyID, platformStaff)
	return nil
}

// Delete retires an entry the caller owns.
func (s *CatalogService) Delete(ctx context.Context, kind, id, companyID string, platformStaff bool) error {
	w, ok := s.writers[kind]
	if !ok {
		return fmt.Errorf("%w: %q is read-only", ErrValidation, kind)
	}
	if err := w.Delete(ctx, id, scopeForEdit(companyID, platformStaff)); err != nil {
		return err
	}
	s.invalidate(ctx, kind, companyID, platformStaff)
	return nil
}

// visibility is the scoping rule every reference list shares: a company sees
// the entries Karlo maintains plus its own, and never another company's.
//
// Written once here because getting it wrong is a data leak rather than a bug —
// a missing clause shows one customer another's private cargo types, and the
// response looks entirely normal.
//
// Platform staff are exempt, and that is the meaning of the flag rather than a
// hole in it: they maintain the shared lists and support individual companies
// with their private ones. Without the exemption, staff creating an entry FOR a
// company could not read back what they had just written.
func visibility(companyID string, platformStaff bool) bson.M {
	if platformStaff {
		return bson.M{}
	}
	global := bson.M{"companyId": nil}
	if companyID == "" {
		return global
	}
	return bson.M{"$or": []bson.M{global, {"companyId": companyID}}}
}

// List pages through one catalogue.
func (s *CatalogService) List(ctx context.Context, kind, companyID, parentID string, platformStaff bool, p query.Params) ([]CatalogEntry, int64, error) {
	spec, ok := catalogKinds[kind]
	if !ok {
		return nil, 0, fmt.Errorf("%w: %q (have: %s)", ErrUnknownKind, kind,
			strings.Join(CatalogKinds(), ", "))
	}

	listKey := catalogListKey(catalogScope(companyID, platformStaff), kind, parentID, p)
	var hit cachedList
	if cache.GetJSON(ctx, s.cache, listKey, &hit) {
		return hit.Entries, hit.Total, nil
	}

	filter := bson.M{"deleted": bson.M{"$ne": true}}
	for k, v := range visibility(companyID, platformStaff) {
		filter[k] = v
	}

	if parentID != "" {
		if spec.parentField == "" {
			return nil, 0, fmt.Errorf("%w: %q entries have no parent", ErrValidation, kind)
		}
		filter[spec.parentField] = parentID
	}

	if p.Search != "" {
		// Anchored on the normalised name, which is what the index is on. An
		// unanchored regex cannot use it and scans the collection.
		filter["nameNormalised"] = bson.M{"$regex": "^" + escapeRegex(strings.ToLower(p.Search))}
	}

	col := s.db.Collection(collectionName(spec.collection))

	total, err := col.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, err
	}

	opts := findOptions(p)
	cursor, err := col.Find(ctx, filter, opts)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = cursor.Close(ctx) }()

	var raw []bson.M
	if err := cursor.All(ctx, &raw); err != nil {
		return nil, 0, err
	}

	out := make([]CatalogEntry, 0, len(raw))
	for _, doc := range raw {
		out = append(out, toEntry(kind, doc))
	}
	cache.SetJSON(ctx, s.cache, listKey, cachedList{Entries: out, Total: total}, catalogTTL)
	return out, total, nil
}

// Get resolves one entry.
func (s *CatalogService) Get(ctx context.Context, kind, id, companyID string, platformStaff bool) (*CatalogEntry, error) {
	spec, ok := catalogKinds[kind]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}

	oid, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		// A malformed id is answered as not-found rather than as a bad
		// request. It is not worth telling a prober that their id was the
		// wrong SHAPE as distinct from the wrong value.
		return nil, ErrNotFound
	}

	entryKey := catalogEntryKey(catalogScope(companyID, platformStaff), kind, id)
	var cached CatalogEntry
	if cache.GetJSON(ctx, s.cache, entryKey, &cached) {
		return &cached, nil
	}

	filter := bson.M{"_id": oid, "deleted": bson.M{"$ne": true}}
	for k, v := range visibility(companyID, platformStaff) {
		filter[k] = v
	}

	var doc bson.M
	err = s.db.Collection(collectionName(spec.collection)).FindOne(ctx, filter).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	entry := toEntry(kind, doc)
	cache.SetJSON(ctx, s.cache, entryKey, entry, catalogTTL)
	return &entry, nil
}

// Resolve batch-resolves references so a caller can denormalise many ids in one
// round trip, rather than one lookup per row of a list.
//
// References that do not resolve are simply absent from the result. The caller
// asked "what are these", and the answer for a missing one is nothing; telling
// them apart from a failed query is what Validate is for.
func (s *CatalogService) Resolve(ctx context.Context, companyID string, refs map[string][]string) (map[string]CatalogEntry, error) {
	out := make(map[string]CatalogEntry)

	for kind, ids := range refs {
		spec, ok := catalogKinds[kind]
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrUnknownKind, kind)
		}

		oids := make([]primitive.ObjectID, 0, len(ids))
		for _, id := range ids {
			if oid, err := primitive.ObjectIDFromHex(id); err == nil {
				oids = append(oids, oid)
			}
		}
		if len(oids) == 0 {
			continue
		}

		filter := bson.M{"_id": bson.M{"$in": oids}, "deleted": bson.M{"$ne": true}}
		// NOT staff-exempt: Resolve runs for the business service acting on one
		// company's behalf, so a reference to another company's private entry
		// must come back unresolved.
		for k, v := range visibility(companyID, false) {
			filter[k] = v
		}

		cursor, err := s.db.Collection(collectionName(spec.collection)).Find(ctx, filter)
		if err != nil {
			return nil, err
		}
		var raw []bson.M
		if err := cursor.All(ctx, &raw); err != nil {
			_ = cursor.Close(ctx)
			return nil, err
		}
		_ = cursor.Close(ctx)

		for _, doc := range raw {
			entry := toEntry(kind, doc)
			out[entry.ID] = entry
		}
	}

	return out, nil
}

// Validate reports which of the given references do not exist.
//
// The business service calls this before committing a write, so an order
// cannot be stored pointing at a cargo type that was deleted or never existed.
// It returns the INVALID ones rather than a boolean, because "which one" is the
// only part of the answer a user can act on.
func (s *CatalogService) Validate(ctx context.Context, companyID string, refs map[string][]string) ([]string, error) {
	resolved, err := s.Resolve(ctx, companyID, refs)
	if err != nil {
		return nil, err
	}

	var invalid []string
	for _, ids := range refs {
		for _, id := range ids {
			if id == "" {
				continue
			}
			if _, ok := resolved[id]; !ok {
				invalid = append(invalid, id)
			}
		}
	}
	return invalid, nil
}

// toEntry flattens a reference document into the shared envelope.
//
// Field names are read from the document rather than from a typed struct
// because the ten collections have ten shapes; anything not promoted stays in
// Attributes, which is what lets a new catalogue be served without a contract
// change.
func toEntry(kind string, doc bson.M) CatalogEntry {
	entry := CatalogEntry{Kind: kind, Active: true, Attrs: map[string]any{}}

	for key, value := range doc {
		switch key {
		case "_id":
			if oid, ok := value.(primitive.ObjectID); ok {
				entry.ID = oid.Hex()
			}
		case "name":
			entry.Name, _ = value.(string)
		case "code":
			entry.Code, _ = value.(string)
		case "description":
			entry.Desc, _ = value.(string)
		case "isActive":
			if active, ok := value.(bool); ok {
				entry.Active = active
			}
		case "companyId":
			if owner, ok := value.(string); ok && owner != "" {
				entry.CompanyID = &owner
			}
		case "createdAt":
			entry.CreatedAt = formatTime(value)
		case "updatedAt":
			entry.UpdatedAt = formatTime(value)
		case "deleted", "nameNormalised":
			// Storage detail. The normalised twin in particular is an index
			// key, not information — returning it invites a client to display
			// or match on it.
		default:
			entry.Attrs[key] = value
		}
	}

	return entry
}

// collectionName applies whatever prefix the deployment is configured with.
// config.Collection is the single place that decision is made; going around it
// would read from an unprefixed collection the writes never land in.
func collectionName(name string) string { return config.Collection(name) }
