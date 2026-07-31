---
name: seeding
description: Development seed data — the runtime seeder derived from the applied schema, `forge db seed` commands, domain vocabulary in db/seeds/vocab.yaml, and hand-authored demo data in db/seeds/custom/.
---

# Seeding Development Data

Seed data is not a file in your project — it materializes at runtime,
deterministically, from the applied schema: FK-coherent, ~20 rows/table,
idempotent.

```
forge db seed apply    # introspect db/migrations, INSERT deterministic rows (dev only)
forge db seed status   # per-table seeded-row counts vs the seed model
forge db seed reset    # wipe seeded tables and re-seed (dev only)
```

`forge run` auto-seeds a fresh dev database on first boot (all tables empty).
`apply`/`reset` refuse any non-dev environment, and the applier is never
compiled into your server binary.

Keep seed data out of migrations: migrations define schema, seeds populate it.
Mixing them makes both harder to reason about.

## Domain vocabulary (`db/seeds/vocab.yaml`)

**This file is the only place your domain's words come from.** Nothing else
supplies them, and forge does not invent them.

forge derives everything your schema *declares* — a column's type, its enum or
CHECK vocabulary, a regex `CHECK (col ~ '...')`, `char_length` / `varchar`
caps, numeric ranges, NOT NULL, UNIQUE, every foreign key. It does not derive
what a column *means*, because that is nowhere in the schema: nothing in
`price_cents BIGINT` says money, and nothing in `name TEXT` says whether the
row is a person or a company. forge will not read it off the column's name
either — a name guess is right for the vocabulary it was written against and
silently wrong everywhere else.

So a column nothing describes seeds an obviously-invented value:
`sample_<column>_<row>`, type-correct and inside every constraint. **A database
full of `sample_*` is not a bug — it is forge reporting that this file is
empty.**

`db/seeds/vocab.yaml` — yours, scaffolded once — teaches it your domain's
vocabulary. **Write it right after authoring migrations, while the domain
context is fresh:**

```yaml
pools:                              # shared pools, referenced by name
  city_names: [Lisbon, Osaka, Nairobi, Montreal, Reykjavik]
columns:                            # "table.column": inline list or {pool: name}
  warehouses.city: {pool: city_names}
  suppliers.city: {pool: city_names}
  carriers.name: [Northwind Freight, Cordon Logistics, Alto Shipping]
```

Matched columns draw from your pools (deterministically, like everything
else); unmatched columns keep built-in synthesis. forge validates every value
**you supply here** against the applied schema — CHECK vocabularies,
varchar/char_length caps, numeric ranges, and the canonical spelling of a
`uuid` column — skipping invalid ones with a warning and falling back to
built-in synthesis for that column. Generated CRUD lifecycle-test fixtures
reuse the same seed plan, so test parent rows carry the same vocabulary.

### Pinning an exact value

A pool of **one** is a pin — the way to state a value forge could not invent:

```yaml
columns:
  users.email: ["dev@example.com"]                              # every row
  orders.currency: ["USD"]                                      # which currency is YOUR decision
  users.external_id: ["3f2504e0-4f89-41d3-9a0c-0305e82c3301"]   # uuid, shape-checked
```

**Keys are not pinnable.** An entry on a primary-key or foreign-key column is
refused with a warning: referential coherence is what the seeder guarantees by
construction, and a hand-written key is a reference it cannot prove resolves.
So you cannot declare the id of a particular row, and you cannot write "this
order belongs to that user" here — both halves are keys.

What you get instead is the **row-0 spine**: row 0 of every table references
row 0 of every table it points at, so one fully-connected dataset always
exists. Ids come from forge's key function and are stable for a given salt, so
querying the database once tells you an id that will still be there next seed.
If a value must exist *before* the seed runs — a principal id baked into a
token your issuer mints — write that row yourself in `db/seeds/custom/`, which
is applied after the generated dataset and is where you own the INSERT and
therefore the key.

## Constraints

Values **forge** synthesizes are satisfied by construction, not validated after
the fact — forge derived the CHECK from your proto field rules, so it generates
inside it. Synthesized strings are fitted to `char_length` / `varchar(n)` caps,
numbers to range CHECKs, enum columns draw from the CHECK vocabulary, and a
single-column UNIQUE column draws **without replacement** so it never collides
(a deeper value pool does not fix a colliding draw — drawing without
replacement does).

A column with a **regex** CHECK — the one a `string.email` or a `string.pattern`
field rule projects into — is seeded from the pattern itself, so the value
matches whatever you declared and does so whatever the column is called:

```sql
CREATE TABLE accounts (
    contact TEXT NOT NULL CHECK (contact ~ '^[^@\s]+@[^@\s]+\.[^@\s]+$'),
    reference TEXT NOT NULL CHECK (reference ~ '^SKU-[0-9]{4}$')
);
-- seeds  contact = '1@1.1', '2@2.2', …   reference = 'SKU-0001', 'SKU-0002', …
```

Anything genuinely unsatisfiable is resolved at **plan** time, before a single
INSERT: a UNIQUE column backed by a 5-value CHECK vocabulary can never carry 20
rows, so its table caps at 5 and the plan says so — the same cap a 1-1 foreign
key already gets.

`apply` runs as one transaction: it seeds every table or leaves the database
untouched, so a failed run is always safe to retry and never leaves a
half-populated dev database behind.

## Two paths to one parent (`forge:ref`)

When an entity reaches the same parent **two ways** — `orders.patient_id` and
`orders.prescription_id → prescriptions.patient_id` — every foreign key is
individually valid and the two routes still name different rows. Those rows are
fixtures a correct implementation of the rule must *reject*, so **`forge db
seed` refuses to write them** and prints the relationship, the disagreeing
rows, and the statements to choose between.

Which route is authoritative is a fact about your domain, not something forge
can read off the schema, so you declare it — as a `COMMENT` on the foreign-key
constraint, in a migration:

```sql
COMMENT ON CONSTRAINT orders_patient_id_fkey ON orders IS
    'forge:ref derived-from=prescription_id';
```

| declaration | meaning |
| --- | --- |
| `forge:ref derived-from=<column>` | the route leaving by `<column>` decides; this column is seeded from it |
| `forge:ref authoritative` | this column is the truth; the other edge is narrowed to agree with it |
| `forge:ref independent` | genuinely unrelated facts (an order shipped to someone else's address) |

It lives on the constraint, not in a forge file, because it is a **schema-level
domain fact** — validation and your own business rules need the same sentence —
and because plain SQL lands in the postgres catalog where forge, `psql \d+` and
anything else can read it. Prose in the same comment is fine; only the
`forge:ref ` clause is read.

## Hand-authored demo data (`db/seeds/custom/`)

Domain-flavored demo rows that the generic synthesizer can't invent go in
`db/seeds/custom/`. These are applied **after** the runtime dataset, so
`ON CONFLICT` overrides of seeded rows are sanctioned there.

See also: `db` for the schema/ORM half.
