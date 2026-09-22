// Master data schema for MongoDB.
//
// Idempotent: safe to run against an existing database. Creates each collection
// with a $jsonSchema validator, then its indexes.
//
//   docker compose exec -T mongodb mongosh karlo_masterdata < schema/master_data.js
//
// ---------------------------------------------------------------------------
// What MongoDB enforces here, and what it cannot
// ---------------------------------------------------------------------------
// Enforced by the database:
//   * required fields, types and enums, through $jsonSchema validators
//   * unique indexes, including compound and PARTIAL ones
//   * uniqueness on a NORMALISED field, which is how "B 1234 XYZ" and
//     "b1234xyz" are made to collide — see the note on plateNormalised
//
// NOT enforced, and therefore the application's responsibility:
//   * referential integrity. Nothing stops a vehicle naming a truckBodyId
//     that was deleted, and nothing cascades when one is.
//   * cross-document consistency. vehicles.trackerId is a copy of the live
//     tracker_assignment; in Postgres a trigger kept them in step, and here
//     every writer must.
//   * multi-document transactions, on THIS deployment. mongod is running
//     standalone, and transactions need a replica set — even a single-node one.
//     Until that changes, an operation touching two collections can half-fail.
//
// Each of those is a real guarantee that moved from the database into code.
// They are listed so the trade is written down rather than discovered.

// Always the master data database, whatever database the connection string
// or the shell prompt happens to name. A connection string with a query
// string and no path lands in `test`, and a schema applied there is a
// schema applied nowhere — silently.
db = db.getSiblingDB("karlo_masterdata");

const NORMALISE_NOTE =
  "Written by the application on every save. The unique index is on THIS field, " +
  "not the human one, so formatting cannot create a duplicate.";

// Every index this script declares, by collection, so anything else found on a
// collection can be recognised as left over from an earlier shape.
const DECLARED = {};
let FAILURES = 0;

function ensure(name, validator, indexes) {
  const existing = db.getCollectionNames();
  if (existing.indexOf(name) === -1) {
    db.createCollection(name, { validator: validator, validationLevel: "moderate" });
  } else {
    db.runCommand({ collMod: name, validator: validator, validationLevel: "moderate" });
  }

  indexes = indexes || [];
  DECLARED[name] = indexes.map(function (ix) { return (ix.opts || {}).name; })
                          .filter(Boolean).concat(["_id_"]);

  // Drop anything this script does not declare.
  //
  // Without this, an index from an earlier shape of the schema survives and can
  // QUIETLY OVERRIDE the one declared here — a full unique index beside a
  // partial one enforces the stricter rule, and an index on a field the
  // validator no longer has collapses to a global unique on whatever remains.
  // Both happened: a stale uq_item_category refused a company naming a category
  // a global entry already used, and a legacy imei_1 made a soft-deleted device
  // block its own IMEI forever.
  db[name].getIndexes().forEach(function (live) {
    if (DECLARED[name].indexOf(live.name) === -1) {
      db[name].dropIndex(live.name);
      print("    dropped stale index " + name + "." + live.name);
    }
  });

  // Create, and REPORT a failure rather than swallowing it.
  //
  // These calls used to run bare. A createIndex that conflicts with an existing
  // one throws, and an uncaught throw inside forEach abandoned the rest of the
  // collection's indexes — so three declared indexes silently did not exist
  // while the script still printed "Done".
  let made = 0;
  indexes.forEach(function (ix) {
    try {
      db[name].createIndex(ix.keys, ix.opts || {});
      made++;
    } catch (e) {
      FAILURES++;
      print("    FAILED " + name + "." + ((ix.opts || {}).name || "?") + ": " + e.message);
    }
  });
  print("  " + name + ": validator + " + made + "/" + indexes.length + " indexes");
}

// Two kinds of identifier, and they are NOT interchangeable.
//
// companyId and every user id come from the authentication service, which is
// PostgreSQL and issues UUIDs — 36 characters with dashes.
//
// Everything inside this database is keyed by MongoDB's own ObjectId, 24 hex
// characters, so a reference from one collection to another is that shape.
//
// Requiring a UUID for both was a real fault: every reference field — groupId,
// vehicleId, trackerId, categoryId — rejected the very ids this database
// issues, so no document could reference another and the validator refused
// every insert that tried.
const tenantId = { bsonType: "string", pattern: "^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$" };
const localRef = { bsonType: "string", pattern: "^[0-9a-fA-F]{24}$" };

// Kept as an alias so the tenant fields below read as what they are.
const uuid = tenantId;
const ts   = { bsonType: "date" };

// ===========================================================================
// Vehicles
// ===========================================================================

ensure("brands", {
  $jsonSchema: {
    bsonType: "object",
    required: ["name", "createdAt"],
    properties: {
      companyId: { bsonType: ["string", "null"] },
      name: { bsonType: "string", minLength: 1 },
      nameNormalised: { bsonType: "string", description: NORMALISE_NOTE },
      isActive: { bsonType: "bool" },
      deleted: { bsonType: "bool" },
      createdAt: ts, updatedAt: ts,
    },
  },
}, [
  { keys: { nameNormalised: 1 },
    opts: { unique: true, name: "uq_brand_global",
            partialFilterExpression: { companyId: null, deleted: false } } },
  { keys: { companyId: 1, nameNormalised: 1 },
    opts: { unique: true, name: "uq_brand_company",
            partialFilterExpression: { companyId: { $type: "string" }, deleted: false } } },
]);

ensure("truck_heads", {
  $jsonSchema: {
    bsonType: "object",
    required: ["name", "createdAt"],
    properties: {
      companyId: { bsonType: ["string", "null"] },
      name: { bsonType: "string", minLength: 1 },
      nameNormalised: { bsonType: "string" },
      code: { bsonType: ["string", "null"] },
      // What distinguishes one tractor unit from another.
      axles: { bsonType: ["int", "null"] },
      configuration: { bsonType: ["string", "null"], description: "4x2, 6x4" },
      // A picture of the tractor unit, shown when picking a type.
      imageKey: { bsonType: ["string", "null"] },
      isActive: { bsonType: "bool" }, deleted: { bsonType: "bool" },
      createdAt: ts, updatedAt: ts,
    },
  },
}, [
  { keys: { nameNormalised: 1 },
    opts: { unique: true, name: "uq_truck_head_global",
            partialFilterExpression: { companyId: null, deleted: false } } },
  { keys: { companyId: 1, nameNormalised: 1 },
    opts: { unique: true, name: "uq_truck_head_company",
            partialFilterExpression: { companyId: { $type: "string" }, deleted: false } } },
]);

ensure("truck_bodies", {
  $jsonSchema: {
    bsonType: "object",
    required: ["name", "createdAt"],
    properties: {
      companyId: { bsonType: ["string", "null"] },
      name: { bsonType: "string", minLength: 1 },
      nameNormalised: { bsonType: "string" },
      code: { bsonType: ["string", "null"] },

      // The SPEC for this kind of body: what a 40ft wingbox generally holds.
      // A vehicle overrides it only when its own figures differ — see the note
      // on vehicles.
      maxWeightKg: { bsonType: ["double", "int", "null"] },
      volumeM3: { bsonType: ["double", "int", "null"] },
      lengthM: { bsonType: ["double", "int", "null"] },
      widthM: { bsonType: ["double", "int", "null"] },
      heightM: { bsonType: ["double", "int", "null"] },

      // Which cargo this body suits. Ids into cargo_types, held as an array
      // rather than a join collection: it is a short list read with the body,
      // never queried on its own.
      cargoTypeIds: { bsonType: "array", items: { bsonType: "string" } },

      imageKey: { bsonType: ["string", "null"] },
      isActive: { bsonType: "bool" }, deleted: { bsonType: "bool" },
      createdAt: ts, updatedAt: ts,
    },
  },
}, [
  { keys: { nameNormalised: 1 },
    opts: { unique: true, name: "uq_truck_body_global",
            partialFilterExpression: { companyId: null, deleted: false } } },
  { keys: { companyId: 1, nameNormalised: 1 },
    opts: { unique: true, name: "uq_truck_body_company",
            partialFilterExpression: { companyId: { $type: "string" }, deleted: false } } },
]);

// ---------------------------------------------------------------------------
// Truck classes
// ---------------------------------------------------------------------------
// The SIZE axis: CDE, CDD, Tronton, Trailer 40FT.
//
// Its own list rather than a field on truck_bodies, because size and body are
// independent: a wingbox exists as a CDD and as a Tronton, and pricing an
// agreement means saying WHICH combinations are covered. One list of "CDD
// Wingbox" strings would multiply out to the product of the two and could not
// answer "every Tronton we carry" without parsing names.
//
// sortOrder exists because these have a natural order — smallest to largest —
// that alphabetical sorting destroys: CDD before CDE before Tronton is not how
// anybody thinks about trucks.
ensure("truck_classes", {
  $jsonSchema: {
    bsonType: "object",
    required: ["name", "createdAt"],
    properties: {
      companyId: { bsonType: ["string", "null"] },
      name: { bsonType: "string", minLength: 1 },
      nameNormalised: { bsonType: "string" },
      code: { bsonType: ["string", "null"] },

      // Indicative, not a rule. What a class of this size generally carries;
      // the BODY carries the figure that prices a load.
      typicalMaxWeightKg: { bsonType: ["double", "int", "null"] },
      axles: { bsonType: ["int", "null"] },

      sortOrder: { bsonType: ["int", "null"] },
      isActive: { bsonType: "bool" }, deleted: { bsonType: "bool" },
      createdAt: ts, updatedAt: ts,
    },
  },
}, [
  { keys: { nameNormalised: 1 },
    opts: { unique: true, name: "uq_truck_class_global",
            partialFilterExpression: { companyId: null, deleted: false } } },
  { keys: { companyId: 1, nameNormalised: 1 },
    opts: { unique: true, name: "uq_truck_class_company",
            partialFilterExpression: { companyId: { $type: "string" }, deleted: false } } },
  { keys: { sortOrder: 1 }, opts: { name: "ix_truck_class_order" } },
]);

// ---------------------------------------------------------------------------
// Vehicles
// ---------------------------------------------------------------------------
// The uniqueness that carries the whole design.
//
// plateNormalised is the field the unique index is on. The human `licensePlate`
// keeps whatever the operator typed; the application writes the normalised form
// beside it — upper-cased with punctuation stripped — so that "B 1234 XYZ",
// "b1234xyz" and "B1234XYZ" are one truck rather than three.
//
// This is the guarantee that moved out of the database. Postgres could index
// the expression directly; MongoDB has no functional indexes, so the value must
// be maintained by whatever writes the document. It belongs in ONE place in the
// code — a single model layer — because a second writer that forgets recreates
// exactly the duplication this schema exists to prevent.

ensure("vehicles", {
  $jsonSchema: {
    bsonType: "object",
    required: ["companyId", "licensePlate", "plateNormalised", "unitType", "createdAt"],
    properties: {
      companyId: uuid,
      licensePlate: { bsonType: "string", minLength: 1 },
      plateNormalised: { bsonType: "string", minLength: 1, description: NORMALISE_NOTE },

      // Identify the physical vehicle rather than its registration: a plate
      // can be reissued, these cannot.
      chassisNumber: { bsonType: ["string", "null"] },
      chassisNormalised: { bsonType: ["string", "null"] },
      engineNumber: { bsonType: ["string", "null"] },

      // rigid: engine and cargo bed in one unit
      // head:  a tractor unit; pulls a body and carries nothing itself
      // body:  a trailer or chassis; carries the load, has no engine
      unitType: { enum: ["rigid", "head", "body"] },

      // A head vehicle names a head type; a body or a rigid names a body type.
      // Both are here rather than one "type" field, because a rigid truck has a
      // body and no separate head, and forcing them through one field would
      // mean guessing which list an id belongs to.
      truckHeadId: { bsonType: ["string", "null"] },
      truckBodyId: { bsonType: ["string", "null"] },

      brandId: { bsonType: ["string", "null"] },

      // One vehicle group per unit (vehicle_groups._id as hex). Single-valued
      // so that TMS and FMS, whose fleet group is one per vehicle, agree.
      truckGroupId: { bsonType: ["string", "null"] },

      unitYear: { bsonType: ["int", "null"] },
      color: { bsonType: ["string", "null"] },

      // A copy of the live tracker_assignment, kept so the common question does
      // not need a query over history. NOTHING IN THE DATABASE KEEPS IT IN
      // STEP — see the note at the top of this file.
      trackerId: { bsonType: ["string", "null"] },
      // The master-data driver, and that driver's login when they have one.
      currentDriverId: { bsonType: ["string", "null"] },
      currentDriverUserId: { bsonType: ["string", "null"] },

      status: { bsonType: "string" },
      isAvailable: { bsonType: "bool" },

      odometerKm: { bsonType: ["double", "int", "null"] },
      hourmeterHours: { bsonType: ["double", "int", "null"] },
      fuelTankLiters: { bsonType: ["double", "int", "null"] },
      fuelRatioKmpl: { bsonType: ["double", "int", "null"] },

      // The cargo figures are NOT here.
      //
      // They belong to the body type — a body of a given kind holds what it
      // holds — and repeating them on the vehicle meant two places to look with
      // nothing saying which won. To find what a vehicle carries, read its
      // truckBodyId and take the figures from there.
      //
      // The case this gives up is a single unit that differs from its type: a
      // truck derated after damage. If that turns out to matter, the field
      // comes back here as an override, nullable, meaning "use the type's".

      notes: { bsonType: ["string", "null"] },
      attributes: { bsonType: "object" },
      deleted: { bsonType: "bool" },
      createdAt: ts, updatedAt: ts,
    },
  },
}, [
  // A plate is unique WITHIN a company: two companies may hold the same plate
  // string, because a plate is reissued when a vehicle is sold on.
  { keys: { companyId: 1, plateNormalised: 1 },
    opts: { unique: true, name: "uq_vehicle_plate",
            partialFilterExpression: { deleted: false } } },

  // A chassis number is unique EVERYWHERE: the same physical truck cannot be
  // registered by two companies at once.
  { keys: { chassisNormalised: 1 },
    opts: { unique: true, name: "uq_vehicle_chassis",
            partialFilterExpression: { chassisNormalised: { $type: "string" }, deleted: false } } },

  { keys: { companyId: 1, deleted: 1 }, opts: { name: "ix_vehicles_company" } },
  { keys: { companyId: 1, unitType: 1 }, opts: { name: "ix_vehicles_unit_type" } },
  { keys: { trackerId: 1 }, opts: { name: "ix_vehicles_tracker" } },
  { keys: { truckHeadId: 1 }, opts: { name: "ix_vehicles_head" } },
  { keys: { truckBodyId: 1 }, opts: { name: "ix_vehicles_body" } },
  { keys: { companyId: 1, truckGroupId: 1 }, opts: { name: "ix_vehicles_group" } },
  { keys: { currentDriverUserId: 1 }, opts: { name: "ix_vehicles_driver" } },
  // GetTrucksByDriver filters on the driver id; the pairing check on every
  // assignment goes through it.
  { keys: { currentDriverId: 1 }, opts: { name: "ix_vehicles_driver_id" } },
]);

ensure("vehicle_groups", {
  $jsonSchema: {
    bsonType: "object",
    required: ["companyId", "name", "createdAt"],
    properties: {
      companyId: uuid,
      name: { bsonType: "string", minLength: 1 },
      nameNormalised: { bsonType: "string" },
      description: { bsonType: ["string", "null"] },
      // Who to tell about this group. Auth user ids, so no cross-database
      // reference — the point of a group is usually that somebody is
      // responsible for it.
      picUserIds: { bsonType: "array", items: { bsonType: "string" } },
      deleted: { bsonType: "bool" },
      createdAt: ts, updatedAt: ts,
    },
  },
}, [
  { keys: { companyId: 1, nameNormalised: 1 },
    opts: { unique: true, name: "uq_vehicle_group",
            partialFilterExpression: { deleted: false } } },
]);

// A company's own consignee register: the end-recipients its orders deliver to.
//
// These are NOT platform users — a customer never signs in, and most have no
// account anywhere. They exist so an order can name who is receiving the goods
// and an invoice can be addressed, which is why the only required field beyond
// the owner is a name.
//
// companyId is required, with no shared variant: unlike a cargo type or a truck
// body, one company's customer list is its commercial relationships and must
// never appear in another company's pickers.
ensure("customers", {
  $jsonSchema: {
    bsonType: "object",
    required: ["companyId", "name", "createdAt"],
    properties: {
      companyId: uuid,
      name: { bsonType: "string", minLength: 1 },
      nameNormalised: { bsonType: "string" },
      // The customer's own reference for itself, printed on documents.
      code: { bsonType: ["string", "null"] },
      npwp: { bsonType: ["string", "null"] },
      address: { bsonType: ["string", "null"] },
      contactName: { bsonType: ["string", "null"] },
      contactPhone: { bsonType: ["string", "null"] },
      contactEmail: { bsonType: ["string", "null"] },
      attributes: { bsonType: ["object", "null"] },
      deleted: { bsonType: "bool" },
      createdAt: ts, updatedAt: ts,
    },
  },
}, [
  // Scoped to the company, so two companies may both have a "PT Maju Jaya".
  // Partial on deleted so a retired name becomes available again.
  { keys: { companyId: 1, nameNormalised: 1 },
    opts: { unique: true, name: "uq_customer_name",
            partialFilterExpression: { deleted: false } } },
]);

// Which vehicles are in which groups.
//
// A membership collection rather than a groupId on the vehicle, because a
// vehicle belongs to SEVERAL groups at once: "Jakarta fleet" and "reefers" and
// "leased units" are all true of one truck, and a single field would force a
// choice between them.
//
// It also means a group can be attached to a notification rule or a person
// without touching the vehicles themselves.
ensure("vehicle_group_members", {
  $jsonSchema: {
    bsonType: "object",
    required: ["companyId", "groupId", "vehicleId"],
    properties: {
      companyId: uuid,
      groupId: localRef,
      vehicleId: localRef,
      driverId: localRef,
      addedByUserId: { bsonType: ["string", "null"] },
      createdAt: ts,
    },
  },
}, [
  // One membership per pair. Without this the same vehicle could be added to a
  // group twice and every count would be wrong.
  { keys: { groupId: 1, vehicleId: 1 },
    opts: { unique: true, name: "uq_group_member" } },
  // "Which groups is this vehicle in" — asked when a vehicle raises an alert
  // and the rules that cover it have to be found.
  { keys: { vehicleId: 1 }, opts: { name: "ix_group_members_vehicle" } },
  { keys: { companyId: 1 }, opts: { name: "ix_group_members_company" } },
]);

// ---------------------------------------------------------------------------
// Trackers, their models and their sensors
// ---------------------------------------------------------------------------

// ===========================================================================
// Items
// ===========================================================================

ensure("cargo_types", {
  $jsonSchema: {
    bsonType: "object",
    required: ["name", "createdAt"],
    properties: {
      companyId: { bsonType: ["string", "null"] },
      name: { bsonType: "string", minLength: 1 },
      nameNormalised: { bsonType: "string" },
      description: { bsonType: ["string", "null"] },
      characteristics: { bsonType: "array", items: { bsonType: "string" } },
      isActive: { bsonType: "bool" }, deleted: { bsonType: "bool" },
      createdAt: ts, updatedAt: ts,
    },
  },
}, [
  { keys: { nameNormalised: 1 },
    opts: { unique: true, name: "uq_cargo_global",
            partialFilterExpression: { companyId: null, deleted: false } } },
  { keys: { companyId: 1, nameNormalised: 1 },
    opts: { unique: true, name: "uq_cargo_company",
            partialFilterExpression: { companyId: { $type: "string" }, deleted: false } } },
]);

// Item categories and sub-categories.
//
// Two collections rather than one self-referencing table. A sub-category is not
// a category with a parent — it is a different level with a different meaning,
// and separating them means a query for "the top-level list" is a collection
// rather than a filter that somebody can forget.
ensure("item_categories", {
  $jsonSchema: {
    bsonType: "object",
    required: ["name", "createdAt"],
    properties: {
      companyId: { bsonType: ["string", "null"] },
      name: { bsonType: "string", minLength: 1 },
      nameNormalised: { bsonType: "string" },
      description: { bsonType: ["string", "null"] },
      isActive: { bsonType: "bool" }, deleted: { bsonType: "bool" },
      createdAt: ts, updatedAt: ts,
    },
  },
}, [
  { keys: { nameNormalised: 1 },
    opts: { unique: true, name: "uq_item_category_global",
            partialFilterExpression: { companyId: null, deleted: false } } },
  { keys: { companyId: 1, nameNormalised: 1 },
    opts: { unique: true, name: "uq_item_category_company",
            partialFilterExpression: { companyId: { $type: "string" }, deleted: false } } },
]);

ensure("item_sub_categories", {
  $jsonSchema: {
    bsonType: "object",
    required: ["categoryId", "name", "createdAt"],
    properties: {
      companyId: { bsonType: ["string", "null"] },
      // The category this sits under. Required: a sub-category with no
      // category is not a sub-category.
      categoryId: localRef,
      name: { bsonType: "string", minLength: 1 },
      nameNormalised: { bsonType: "string" },
      description: { bsonType: ["string", "null"] },
      isActive: { bsonType: "bool" }, deleted: { bsonType: "bool" },
      createdAt: ts, updatedAt: ts,
    },
  },
}, [
  { keys: { categoryId: 1, nameNormalised: 1 },
    opts: { unique: true, name: "uq_item_sub_category",
            partialFilterExpression: { deleted: false } } },
  { keys: { categoryId: 1 }, opts: { name: "ix_item_sub_category_parent" } },
]);

ensure("items", {
  $jsonSchema: {
    bsonType: "object",
    required: ["companyId", "name", "createdAt"],
    properties: {
      companyId: uuid,
      categoryId: { bsonType: ["string", "null"] },
      subCategoryId: { bsonType: ["string", "null"] },
      cargoTypeId: { bsonType: ["string", "null"] },
      name: { bsonType: "string", minLength: 1 },
      code: { bsonType: ["string", "null"] },
      codeNormalised: { bsonType: ["string", "null"] },
      description: { bsonType: ["string", "null"] },
      unit: { bsonType: ["string", "null"] },
      // What one unit weighs, so an order can total a load against a vehicle.
      weightKg: { bsonType: ["double", "int", "null"] },
      volumeM3: { bsonType: ["double", "int", "null"] },
      isActive: { bsonType: "bool" }, deleted: { bsonType: "bool" },
      attributes: { bsonType: "object" },
      createdAt: ts, updatedAt: ts,
    },
  },
}, [
  { keys: { companyId: 1, deleted: 1 }, opts: { name: "ix_items_company" } },
  { keys: { categoryId: 1 }, opts: { name: "ix_items_category" } },
  { keys: { subCategoryId: 1 }, opts: { name: "ix_items_sub_category" } },
  { keys: { companyId: 1, codeNormalised: 1 },
    opts: { unique: true, name: "uq_item_code",
            partialFilterExpression: { codeNormalised: { $type: "string" }, deleted: false } } },
]);

// ---------------------------------------------------------------------------
// Truck heads and truck bodies
// ---------------------------------------------------------------------------
// Two lists, deliberately separate. A head type and a body type describe
// different things and carry different fields: a head has axles and a tractor
// configuration and carries nothing, a body has dimensions and a weight limit
// and has no engine.
//
// They were one `vehicle_types` collection with a `unitType` discriminator,
// which meant every field had to be nullable because half of them applied to
// half the rows. Separately, each can require what it actually needs.
//
// Both link straight to a vehicle. There is no head-by-body matrix.

// ===========================================================================
// Trackers
// ===========================================================================

ensure("tracker_models", {
  $jsonSchema: {
    bsonType: "object",
    required: ["vendor", "model", "createdAt"],
    properties: {
      // Which register entries of this model belong to. gps unless said
      // otherwise, so every existing row keeps meaning what it did.
      kind: { enum: ["gps", "dashcam"] },
      vendor: { bsonType: "string", minLength: 1 },
      model: { bsonType: "string", minLength: 1 },
      // How it speaks: codec8, gt06. RECORDED, not acted on — the telemetry
      // service decodes the feed and this service never sees it. It is here so
      // somebody diagnosing a device can see what it should be sending.
      protocol: { bsonType: ["string", "null"] },
      attributes: { bsonType: "object" },
      isActive: { bsonType: "bool" },
      createdAt: ts, updatedAt: ts,
    },
  },
}, [
  { keys: { vendor: 1, model: 1 }, opts: { unique: true, name: "uq_tracker_model" } },
]);

ensure("trackers", {
  $jsonSchema: {
    bsonType: "object",
    required: ["kind", "deviceId", "owner", "createdAt"],
    properties: {
      // What the device is. One register for GPS trackers and dashcams,
      // because both are a physical device fitted to a vehicle with a fitting
      // history, and two registers is how one vehicle ends up with a device
      // in each that nobody can see together.
      kind: { enum: ["gps", "dashcam"] },

      // The device's identity within its kind: the IMEI for gps, the
      // vendor's device id for a dashcam. Unique per kind.
      deviceId: { bsonType: "string", minLength: 1 },

      // Where the device is DEPLOYED. null while it sits in stock, which is why
      // this is optional when nearly every other companyId is not.
      companyId: { bsonType: ["string", "null"] },

      // Who OWNS it, which is a different question and differs constantly:
      // Karlo lends devices, and a customer may bring their own.
      owner: { enum: ["karlo", "customer", "vendor"] },
      ownerName: { bsonType: ["string", "null"] },

      // Kept for gps devices, equal to deviceId, because every telemetry
      // reading and assignment is keyed on it. Absent for a dashcam.
      // What was fitted, copied from the device so the history survives the
      // device document being retired. kind + deviceId always; imei for gps.
      kind: { enum: ["gps", "dashcam"] },
      deviceId: { bsonType: "string", minLength: 1 },
      imei: { bsonType: "string", minLength: 1 },

      // The SIM card itself. Survives the number changing, which a phone
      // number does not.
      iccid: { bsonType: ["string", "null"] },
      simProvider: { bsonType: ["string", "null"] },

      // The model, always by reference. A free-text modelName sat here for
      // devices not yet catalogued, and it was the wrong answer: two spellings
      // of one model cannot be compared, so "every device of this model" — the
      // question asked when a firmware fault appears — would miss half of them.
      // Add the model to tracker_models first.
      modelId: { bsonType: ["string", "null"] },

      status: { bsonType: "string" },
      attributes: { bsonType: "object" },
      deleted: { bsonType: "bool" },
      createdAt: ts, updatedAt: ts,
    },
  },
}, [
  { keys: { kind: 1, deviceId: 1 },
    opts: { unique: true, name: "uq_tracker_device",
            partialFilterExpression: { deleted: false } } },
  { keys: { imei: 1 },
    opts: { unique: true, name: "uq_tracker_imei_gps",
            partialFilterExpression: { imei: { $type: "string" }, deleted: false } } },
  { keys: { iccid: 1 },
    opts: { unique: true, name: "uq_tracker_iccid",
            partialFilterExpression: { iccid: { $type: "string" }, deleted: false } } },
  { keys: { companyId: 1, deleted: 1 }, opts: { name: "ix_trackers_company" } },
  { keys: { owner: 1 }, opts: { name: "ix_trackers_owner" } },
  { keys: { modelId: 1 }, opts: { name: "ix_trackers_model" } },
]);

// The kinds of PHYSICAL sensor that can be fitted to a device.
//
// Worth being clear about the scope: many trackers report fuel or ignition from
// the vehicle's own bus with no separate hardware at all. This collection and
// tracker_sensors describe what was PHYSICALLY INSTALLED — a probe in a tank, a
// door contact, a temperature sensor in a reefer — so that an engineer can see
// what is on a vehicle and a reading can be traced to the thing producing it.
//
// A list rather than free text, so "fuel" and "Fuel Level" cannot become two
// things nothing can compare.
ensure("sensor_types", {
  $jsonSchema: {
    bsonType: "object",
    required: ["code", "name"],
    properties: {
      code: { bsonType: "string", minLength: 1 },
      name: { bsonType: "string", minLength: 1 },
      unit: { bsonType: ["string", "null"] },
      // Decides how a reading is stored and charted. A door is not a quantity.
      valueKind: { enum: ["number", "boolean"] },
      description: { bsonType: ["string", "null"] },
      isActive: { bsonType: "bool" },
      createdAt: ts,
    },
  },
}, [
  { keys: { code: 1 }, opts: { unique: true, name: "uq_sensor_code" } },
]);

// Which sensors are fitted to ONE DEVICE, on which input, with what
// calibration. Per device rather than per model: two units of the same model
// can be wired differently, and it is the individual device that is connected
// to a fuel probe.
ensure("tracker_sensors", {
  $jsonSchema: {
    bsonType: "object",
    required: ["trackerId", "sensorTypeId", "fittedAt"],
    properties: {
      trackerId: localRef,
      sensorTypeId: localRef,
      // Which input it is wired to. Two fuel probes on one tracker is normal on
      // a truck with two tanks, which is why the uniqueness includes it.
      channel: { bsonType: ["string", "null"] },
      channelKey: { bsonType: "string", description: "channel or empty string; see the index" },
      // The numbers that turn a raw reading into litres or degrees. Per
      // fitting, because the same probe in a different tank calibrates
      // differently.
      calibration: { bsonType: "object" },
      fittedAt: ts,
      removedAt: { bsonType: ["date", "null"] },
      createdAt: ts,
    },
  },
}, [
  // channelKey rather than channel: a null in a compound unique index does not
  // behave as "the same missing value", so an unchannelled sensor could
  // otherwise be fitted twice.
  { keys: { trackerId: 1, sensorTypeId: 1, channelKey: 1 },
    opts: { unique: true, name: "uq_tracker_sensor_live",
            partialFilterExpression: { removedAt: null } } },
  { keys: { trackerId: 1 }, opts: { name: "ix_tracker_sensors_tracker" } },
]);

// Which device was on which vehicle, and when.
//
// Without this history, telemetry recorded last month is attributed to whatever
// vehicle the device is on today — so a trip report for one truck silently
// includes another's journeys.
ensure("tracker_assignments", {
  $jsonSchema: {
    bsonType: "object",
    required: ["companyId", "vehicleId", "kind", "deviceId", "fittedAt"],
    properties: {
      companyId: uuid,
      // Null once the device document is deleted. The copied imei below is what
      // still identifies it — which is the whole reason imei is duplicated here.
      trackerId: { bsonType: ["string", "null"] },
      vehicleId: localRef,
      imei: { bsonType: "string", minLength: 1 },
      fittedAt: ts,
      unfittedAt: { bsonType: ["date", "null"] },

      // A photograph of the finished installation, and who did it.
      //
      // Fitting happens in a yard, often by a subcontractor, and "was this
      // actually installed" is asked when a device never reports. A picture
      // taken at the time answers it; the object-store key, never a URL, since
      // a signed URL expires and a stored one rots.
      installPhotoKey: { bsonType: ["string", "null"] },
      installedByUserId: { bsonType: ["string", "null"] },
      installNotes: { bsonType: ["string", "null"] },

      createdAt: ts,
    },
  },
}, [
  // One device is fitted to one vehicle at a time. This is what stops the same
  // device reporting for two vehicles at once.
  { keys: { trackerId: 1 },
    opts: { unique: true, name: "uq_assignment_live",
            partialFilterExpression: { unfittedAt: null, trackerId: { $type: "string" } } } },
  { keys: { vehicleId: 1 }, opts: { name: "ix_assignments_vehicle" } },
  { keys: { imei: 1, fittedAt: -1 }, opts: { name: "ix_assignments_imei" } },
  { keys: { companyId: 1, fittedAt: -1 }, opts: { name: "ix_assignments_company" } },
]);

// ---------------------------------------------------------------------------
// Sites and documents
// ---------------------------------------------------------------------------

// ===========================================================================
// Sites
// ===========================================================================

ensure("sites", {
  $jsonSchema: {
    bsonType: "object",
    required: ["companyId", "siteType", "name", "createdAt"],
    properties: {
      companyId: uuid,
      // warehouse: a loading or unloading point on an order
      // depot:     where a transporter keeps its fleet (an FMS hangar)
      // port:      a sea or dry port
      siteType: { enum: ["warehouse", "depot", "port", "customer", "other"] },
      name: { bsonType: "string", minLength: 1 },
      nameNormalised: { bsonType: "string" },

      // The address as text. City and province are NAMES, not ids into a
      // region table: there is no region table, and an id referencing nothing
      // is worse than the name it stands for — it cannot be read, printed or
      // searched without a lookup that does not exist.
      address: { bsonType: ["string", "null"] },
      street: { bsonType: ["string", "null"] },
      district: { bsonType: ["string", "null"] },
      city: { bsonType: ["string", "null"] },
      province: { bsonType: ["string", "null"] },
      postcode: { bsonType: ["string", "null"] },

      // GeoJSON, so a 2dsphere index can answer "which site is this truck at".
      location: {
        bsonType: ["object", "null"],
        properties: {
          type: { enum: ["Point"] },
          coordinates: { bsonType: "array", items: { bsonType: ["double", "int"] } },
        },
      },
      geofenceRadiusM: { bsonType: ["int", "null"] },

      // Who to call at the gate. Named for what it is: not the company's
      // switchboard but the person in charge of this site.
      sitePicPhone: { bsonType: ["string", "null"] },
      notes: { bsonType: ["string", "null"] },
      picUserIds: { bsonType: "array", items: { bsonType: "string" } },

      isActive: { bsonType: "bool" },
      attributes: { bsonType: "object" },
      deleted: { bsonType: "bool" },
      createdAt: ts, updatedAt: ts,
    },
  },
}, [
  // Unique per company AND type: a depot and a warehouse may share a name,
  // because the yard and the warehouse next to it are named after the place.
  { keys: { companyId: 1, siteType: 1, nameNormalised: 1 },
    opts: { unique: true, name: "uq_site_name",
            partialFilterExpression: { deleted: false } } },
  { keys: { companyId: 1, deleted: 1 }, opts: { name: "ix_sites_company" } },
  // The reason `location` is GeoJSON rather than two numbers: this index
  // answers proximity, which two columns cannot.
  { keys: { location: "2dsphere" }, opts: { name: "ix_sites_geo" } },
]);

// Which companies may see a site, and in what capacity.
//
// A shipper's warehouse is where a TRANSPORTER loads. Both need it, and with a
// single owner the transporter creates their own copy — two records of one
// gate, two geofences that drift apart, and a truck inside one and outside the
// other.
ensure("site_links", {
  $jsonSchema: {
    bsonType: "object",
    required: ["siteId", "companyId", "relation"],
    properties: {
      siteId: localRef,
      companyId: uuid,
      // owner may edit it; operator runs it day to day; user loads here and may
      // read it and nothing more. This is what lets a transporter see a
      // shipper's warehouse without being able to move its geofence.
      relation: { enum: ["owner", "operator", "user"] },
      linkedByUserId: { bsonType: ["string", "null"] },
      createdAt: ts,
    },
  },
}, [
  { keys: { siteId: 1, companyId: 1 }, opts: { unique: true, name: "uq_site_link" } },
  { keys: { companyId: 1, relation: 1 }, opts: { name: "ix_site_links_company" } },
]);

// A vehicle's paperwork: STNK, KIR, insurance.
//
// Its own collection rather than an array on the vehicle, because EXPIRY is the
// point: "what expires in the next thirty days" has to be answerable across
// every vehicle at once, and an array inside each vehicle cannot be indexed for
// that across vehicles.
ensure("documents", {
  $jsonSchema: {
    bsonType: "object",
    // Exactly one owner: vehicleId or driverId. The validator cannot say
    // "one of"; the service enforces it, and the two indexes below make
    // each side answerable.
    required: ["companyId", "docType", "createdAt"],
    properties: {
      companyId: uuid,
      // VEHICLES ONLY. A person's documents live in the authentication
      // service's user_documents, beside the account they belong to; a
      // polymorphic owner split them across two services and bought nothing.
      vehicleId: localRef,
      docType: { bsonType: "string", minLength: 1 },
      number: { bsonType: ["string", "null"] },
      issuedOn: { bsonType: ["date", "null"] },
      expiresOn: { bsonType: ["date", "null"] },
      // The object-store key, never a URL: signed URLs expire, so a stored one
      // rots. The key is resolved to a fresh URL when someone asks to see it.
      fileKey: { bsonType: ["string", "null"] },
      isVerified: { bsonType: "bool" },
      verifiedByUserId: { bsonType: ["string", "null"] },
      notes: { bsonType: ["string", "null"] },
      deleted: { bsonType: "bool" },
      createdAt: ts, updatedAt: ts,
    },
  },
}, [
  { keys: { vehicleId: 1 }, opts: { name: "ix_documents_vehicle" } },
  { keys: { driverId: 1 }, opts: { name: "ix_documents_driver",
            partialFilterExpression: { driverId: { $type: "string" } } } },
  // The reminder query: everything expiring soon, across the whole company.
  { keys: { companyId: 1, expiresOn: 1 },
    opts: { name: "ix_documents_expiry",
            partialFilterExpression: { expiresOn: { $type: "date" }, deleted: false } } },
]);

// The standard sensors, so a deployment starts with a usable list.

// Drivers: the person register.
//
// A driver is a person who drives, as master data — a name, a phone number,
// a licence. Not an identity: most drivers never sign in. When one does, the
// auth user is recorded on userId, so the TMS driver app and the FMS
// scorecard describe the same person. FMS's employees and TMS's drivers are
// one register here, and a vehicle's currentDriverId points at it.
ensure("drivers", {
  $jsonSchema: {
    bsonType: "object",
    required: ["companyId", "fullName", "nameNormalised", "status", "createdAt"],
    properties: {
      companyId: tenantId,
      fullName: { bsonType: "string", minLength: 1 },
      nameNormalised: { bsonType: "string" },
      phone: { bsonType: ["string", "null"] },
      phoneNormalised: { bsonType: ["string", "null"] },
      employeeNo: { bsonType: ["string", "null"] },
      licenseNo: { bsonType: ["string", "null"] },
      licenseClass: { bsonType: ["string", "null"] },
      // A date. Stored at UTC midnight; no time is meaningful.
      licenseExpiry: { bsonType: ["date", "null"] },
      status: { enum: ["active", "inactive"] },
      // The auth user, when this person has a login. ABSENT rather than
      // null when nobody has looked: an import cannot assert there is no
      // account, only that it did not link one.
      userId: uuid,
      notes: { bsonType: ["string", "null"] },
      attributes: { bsonType: "object" },
      deleted: { bsonType: "bool" },
      createdAt: ts, updatedAt: ts,
    },
  },
}, [
  { keys: { companyId: 1, phoneNormalised: 1 },
    opts: { unique: true, name: "uq_driver_phone",
            partialFilterExpression: { phoneNormalised: { $type: "string" }, deleted: false } } },
  { keys: { companyId: 1, employeeNo: 1 },
    opts: { unique: true, name: "uq_driver_employee_no",
            partialFilterExpression: { employeeNo: { $type: "string" }, deleted: false } } },
  { keys: { companyId: 1, licenseNo: 1 },
    opts: { unique: true, name: "uq_driver_license",
            partialFilterExpression: { licenseNo: { $type: "string" }, deleted: false } } },
  { keys: { userId: 1 },
    opts: { unique: true, name: "uq_driver_user",
            partialFilterExpression: { userId: { $type: "string" }, deleted: false } } },
  { keys: { companyId: 1, status: 1, nameNormalised: 1 }, opts: { name: "ix_drivers_company_name" } },
  { keys: { companyId: 1, licenseExpiry: 1 },
    opts: { name: "ix_drivers_license_expiry",
            partialFilterExpression: { licenseExpiry: { $type: "date" } } } },
]);

// One-off backfills for rows written before a field existed. Idempotent.
//
// Trackers registered before `kind` existed are GPS devices identified by
// IMEI; dashcams did not exist here. Run BEFORE the trackers validator
// above is enforced against them by any write.
const assignmentsBackfilled = db.tracker_assignments.updateMany(
  { kind: { $exists: false } },
  [{ $set: { kind: "gps", deviceId: "$imei" } }]);
if (assignmentsBackfilled.modifiedCount) print("  tracker_assignments: " + assignmentsBackfilled.modifiedCount + " rows given kind=gps, deviceId=imei");
const backfilled = db.trackers.updateMany(
  { kind: { $exists: false } },
  [{ $set: { kind: "gps", deviceId: "$imei" } }]);
if (backfilled.modifiedCount) print("  trackers: " + backfilled.modifiedCount + " rows given kind=gps, deviceId=imei");
const modelsBackfilled = db.tracker_models.updateMany({ kind: { $exists: false } }, { $set: { kind: "gps" } });
if (modelsBackfilled.modifiedCount) print("  tracker_models: " + modelsBackfilled.modifiedCount + " rows given kind=gps");

// The dashcam vendor in use. A model per device type is added as devices are
// registered; the vendor row exists so a dashcam can be catalogued at all.
db.tracker_models.updateOne({ vendor: "Howen", model: "MDVR", kind: "dashcam" },
  { $setOnInsert: { vendor: "Howen", model: "MDVR", kind: "dashcam", isActive: true, createdAt: new Date(), updatedAt: new Date() } },
  { upsert: true });

const SENSORS = [
  { code: "fuel",        name: "Fuel level",   unit: "L",   valueKind: "number",  description: "Tank level from a probe or the CAN bus" },
  { code: "temperature", name: "Temperature",  unit: "C",   valueKind: "number",  description: "Reefer or cargo temperature" },
  { code: "door",        name: "Door",         unit: null,  valueKind: "boolean", description: "Open or closed" },
  { code: "ignition",    name: "Ignition",     unit: null,  valueKind: "boolean", description: "Engine on or off" },
  { code: "odometer",    name: "Odometer",     unit: "km",  valueKind: "number",  description: "Distance" },
  { code: "rpm",         name: "Engine speed", unit: "rpm", valueKind: "number",  description: "From the CAN bus" },
  { code: "weight",      name: "Axle load",    unit: "kg",  valueKind: "number",  description: "From a load sensor" },
  { code: "panic",       name: "Panic button", unit: null,  valueKind: "boolean", description: "Driver alarm" },
];
SENSORS.forEach(function (s) {
  s.isActive = true;
  db.sensor_types.updateOne({ code: s.code },
    { $setOnInsert: Object.assign({ createdAt: new Date() }, s) }, { upsert: true });
});
print("  sensor_types seeded: " + db.sensor_types.countDocuments({}));
const collections = db.getCollectionNames().filter(function (c) { return true; });
print("\n" + collections.length + " master data collections.");
if (FAILURES > 0) {
  print("FAILED: " + FAILURES + " index(es) could not be created. The schema is INCOMPLETE.");
  quit(1);
}
print("Done — every declared index exists.");
