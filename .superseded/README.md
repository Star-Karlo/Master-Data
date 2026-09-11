# Superseded: the service and handler layer

These files were written against the PREVIOUS master data model — `md_trucks`,
`md_catalog_items`, `md_warehouses`, `md_customers` — none of which exist any
more. They are kept here rather than deleted because they carry the request
validation, pagination and error mapping that the replacements will need, and
rewriting that from nothing would lose work that was already correct.

They do NOT compile against the current models, and are excluded from the build
by living outside `internal/`.

## What replaced what

| Was | Now |
|---|---|
| `md_catalog_items` with a `kind` column | `md_brands`, `md_cargo_types`, `md_item_categories`, `md_item_sub_categories`, `md_truck_heads`, `md_truck_bodies` — a collection each |
| `md_trucks` | `md_vehicles`, with `unitType` distinguishing rigid, head and body |
| `md_warehouses` | `md_sites`, with `siteType` covering warehouses, depots and ports |
| `md_customers` | Gone. Customers are companies in the authentication service. |
| Bespoke repository per collection | One generic `repository.Store[T]`, which is where `BeforeWrite` is called |

## The rule any replacement must follow

Every write goes through `repository.Store`. It calls `BeforeWrite`, which fills
the normalised fields that every unique index is built on. A handler that writes
to a collection directly produces a document with those fields missing — the
partial index ignores it, and a duplicate is created with no error at all.
