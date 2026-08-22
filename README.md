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
routes — belongs to one company outright, scoped by the company id on the token
and never by a request parameter.

## Getting started

```bash
cp .env.example .env          # set SERVICE_TOKEN, ACCEPTED_SERVICE_TOKENS
make run
make seed-dry                 # preview the catalogue load; writes nothing
make seed                     # load the global catalogues
```

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
