# Karlo Masterdata Service

**HTTP** `:5002` · **gRPC** `:6002` · **Store** MongoDB

Reference data, in two families with different access rules.

**Catalogues** carry a `companyId`. Empty means a platform-global entry — truck
types, provinces, currencies — shared by every company. A value means a
company-private one: that company's own item types, body types and naming. A
company reads the globals plus its own and nothing of anybody else's, decided in
the query rather than checked afterwards, and uniqueness is
`(companyId, kind, code)` so two companies may each define a body type coded
`BOX`.

Only the ten kinds in `models.CompanyScopedKinds` may be extended per company.
The rest are platform-wide: a company does not get its own list of Indonesian
provinces, and one defining its own currency codes would break every integration
that reads them.

Ownership of a written entry is decided from the token — platform staff write
globals, a company writes its own — never from a `companyId` in the body. Note
that `PUT /catalog/:kind` is still restricted to `superadmin`/`admin`, so the
per-company write path has no caller yet.

**Company data** — trucks, warehouses, customers, truck groups, points, saved
routes, trackers — belongs to one company outright, scoped by the company id on
the token and never by a request parameter.

## Getting started

```bash
cp .env.example .env          # set SERVICE_TOKEN, ACCEPTED_SERVICE_TOKENS
make run
make seed-dry                 # preview the catalogue load; writes nothing
make seed                     # load the global catalogues
```

For a full local stack — accounts, companies, orders and this catalogue in one
step — run `./seed/seed.sh` from the workspace instead; it calls the seeder
above and then verifies the counts. Note that the compose MongoDB is published
on host port **27018**, not 27017, so that a MongoDB already installed on the
machine cannot silently receive the writes.

`JWT_PUBLIC_KEY_FILE` must point at the authentication service's public key.

## The legacy database is unreachable by construction

This service **cannot connect to the monolith's MongoDB**, by two independent
defences that no environment variable can disable
([`internal/config/guard.go`](internal/config/guard.go)):

1. **Target guard.** The connection is refused *before the driver is built* if the URI names the legacy Atlas cluster, or the database is `prod`, `test` or `local` — whether named in `MONGO_DATABASE` or in the URI path.
2. **Collection prefix.** Everything this service writes carries `md_`. The legacy Mongoose models own the unprefixed `trucks`, `warehouses`, `customers` and `points`, so even a target that slipped past the first check could not overwrite them.

The seeder adds a third: it is a dry run by default, writes only with
`-confirm`, upserts on `(companyId, kind, code)` so re-running is a no-op and a
company's own entry of the same code is never overwritten, and never deletes.


## Telematics devices live here, and why

The link between a police number and a device's IMEI is a fact about the
physical world that **both Karlo products need and neither owns**. TMS knows a
vehicle by its plate; the tracking service knows it only by IMEI. Holding the
link in shared master data means no product owns a mapping belonging to
neither, and TMS never has to handle a device identifier it has no legitimate
way to resolve.

`md_trackers` is the device register and `md_tracker_assignments` records which
vehicle carried which device, and when. Four decisions worth knowing before
reading the code:

**IMEI is unique globally, not per company.** Every other uniqueness rule in
this service is scoped by `companyId`, because two companies may legitimately
hold the same plate or the same catalogue code. A tracker is not like that: it
is one physical object, in one company's hands. The duplicate error says only
*"that IMEI is already registered"* and deliberately never names the company
holding it, because doing so would confirm another tenant's inventory to
whoever asked. `TestIMEIIsGloballyUnique` asserts the message stays that
unhelpful.

**Assignments are time-bounded because a device outlives a posting.** One
tracker produces one continuous trail of readings across every vehicle it is
ever fitted to. Read that trail against whichever truck holds the device
*today* and everything an earlier truck drove is credited to the current one —
silently, corrupting distance, fuel-per-kilometre and driver scoring rather
than failing. `VehicleAt(imei, at)` resolves the assignment that was open at
that instant. The Karlo fleet product has confirmed this is a live defect on
its side today, which is why the history exists from day one.

**`Fit` keeps three records in agreement**: `tracker.vehicleId`,
`truck.trackerId`, and exactly one open assignment row. It closes any open
assignment on either side, detaches whatever the device or the vehicle was
previously paired with, then writes all three — and refuses a vehicle belonging
to another company, since otherwise a caller could fit their own device to
someone else's truck and read its position. The three writes are **not** in a
MongoDB session; that needs a replica set, and whether to require one is an
open decision rather than an oversight.

**IMEI normalisation strips every non-digit and then requires exactly 15.**
These arrive from spreadsheets, where a leading apostrophe, a non-breaking
space or a hyphen is routine, and where a serial or SIM number is easily pasted
into the wrong column. A value that is not 15 digits will never match a
telemetry reading, so the vehicle simply reports nothing and nothing anywhere
explains why. Entry is the only place the mistake is still cheap.

The routes reuse `truck.read`, `truck.create` and `truck.update` rather than
carrying keys of their own: a device is part of the vehicle record, not a
separate thing a company buys.

## Catalogues are read through Redis

Every order form loads half a dozen catalogues, and they change perhaps monthly.
Entries and listings are cached for 15 minutes and invalidated **explicitly** on
upsert, because a catalogue edit is usually made by someone who then reloads the
page to check it.

The cache key carries the company scope. Without it, one company's request would
populate an entry another then reads — which no amount of correct database
scoping prevents, because the second request never reaches the database. Editing
a *global* entry sweeps every scope, since a global entry appears in every
company's listing.

This replaced an in-process map, which meant N Fargate tasks with N independent
TTLs and an edit that had to expire out of each separately. With `REDIS_ADDR`
unset the service runs correctly against MongoDB alone. See `../docs/CACHING.md`.

## Layout

```
cmd/server/           entrypoint
internal/
  config/             environment loading; no defaults for security controls
  models/             domain types
  repository/         the only code that talks to the database
  services/           business rules
  handlers/           HTTP
  grpcserver/         gRPC contract implementation
  clients/            outbound gRPC to other services
  routes/             the HTTP surface, one handler per path
  platform/           shared plumbing, vendored (see below)
proto/                gRPC contracts
docs/                 generated OpenAPI document
tests/unit/           no I/O; run always
tests/integration/    real database; build-tagged
```

## Platform documentation

The cross-service documentation — data ownership, business flows, testing and
observability — is **not in this repository**. It describes all four services,
so it lives once in the workspace that holds them side by side, at
`../docs/`, rather than in four drifting copies.

This README covers what is specific to this service.

## About `internal/platform`

This directory is **vendored, not authored here**. It holds the plumbing every
Karlo service shares: RS256 token verification, the structured logger, the
gRPC server and client setup, the query allowlist, and the HTTP response
envelope.

Each service repo carries its own copy so it is fully standalone. The cost is
that a change to shared plumbing — a fix to token verification, say — has to be
applied to all four repos. **`internal/platform/authctx` is security-critical:
a change there must land everywhere.**

The same applies to `proto/`. The contracts are duplicated by design; when one
changes, copy the updated `.proto` into every repo that speaks it and run
`make proto` there. CI fails if the committed bindings do not match the
contracts in the repo, which catches a forgotten regeneration but not a
forgotten copy.

## Commands

| | |
|---|---|
| `make tools` | install buf, the protoc plugins, swag and golangci-lint |
| `make run` | run the service |
| `make test` | unit tests |
| `make test-integration` | integration tests (needs a database; see above) |
| `make lint` | golangci-lint |
| `make proto` | regenerate the gRPC bindings |
| `make swagger` | regenerate the OpenAPI document |
| `make docker` | build the container image |

## API documentation

`make run`, then open **http://localhost:5002/swagger/index.html**.

The browser is served only outside production: the document describes every
endpoint and response shape, which is the reconnaissance an attacker would
otherwise have to guess at. A test asserts it 404s when `ENVIRONMENT=production`.
