---
name: seeding
description: Development seed data — the runtime seeder derived from the applied schema, `forge db seed` commands, domain vocabulary in db/seeds/vocab.yaml, and hand-authored demo data in db/seeds/custom/.
---

# Seeding Development Data

Seed data is not a file in your project — it materializes at runtime,
deterministically, from the applied schema: FK-coherent, ~20 rows/table,
idempotent.

## Do this first: fill in `db/seeds/vocab.yaml`

**Write the vocabulary BEFORE the first seed, not after you dislike the rows.**
It takes about two minutes, it is the step that decides whether seeded data
reads like your product, and it is the step every hurried run skips — then
spends longer working around. The full reference is below; the shape is:

```yaml
pools:
  product_names: [Chemex Pour-Over, Aeropress Go, Hario Skerton Grinder]
columns:
  products.name: {pool: product_names}
  products.currency: [USD]                              # see the traps below
  products.price_cents: {min: 1200, max: 24900, step: 100}
```

Two defaults produce schema-valid data that still breaks a page, and both are
invisible until you look at the UI:

- **A 3-char `currency` column synthesizes as `"sa5"`** — right length, and a
  code no currency table recognises. `Intl.NumberFormat` *throws* on it, and
  the generated product page dies through its error boundary. Pin it:
  `products.currency: [USD]`.
- **An undescribed numeric column seeds as the ROW INDEX** (`1, 2, 3 …`), so
  money renders as fractions of a cent and two numeric columns on one table
  come out perfectly correlated. Give it a range.

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
columns:                            # "table.column": inline list, {pool: name},
                                    # {type: name}, or {min: n, max: n}
  warehouses.city: {pool: city_names}
  suppliers.city: {pool: city_names}
  carriers.name: [Northwind Freight, Cordon Logistics, Alto Shipping]
  shipments.weight_grams: {min: 100, max: 25000, step: 50}
  carriers.rating: {min: 1.0, max: 5.0, step: 0.5, decimals: 1}
```

### Numeric columns take a range

`{min, max}` (optionally `step`, and `decimals` for a fractional column)
expands to a numeric pool and is drawn from exactly like an authored list.
**Describe every numeric column that means something.** A numeric column with
no CHECK and no vocabulary synthesizes as the row index — `1, 2, 3 …` — which
satisfies the schema, is never warned about, and is wrong in two ways that
only show up in the UI: a money column renders as a fraction of a cent, and
any two numeric columns on the same table are perfectly correlated (row 7 is
`price_cents = 7` *and* `stock_quantity = 7`).

A numeric column that already carries a CHECK — a range (`price_cents >= 0`)
or an IN-list (`level IN (10, 20, 30)`) — is read off the schema and needs no
entry here; the range is for the unconstrained ones.

Matched columns draw from your pools (deterministically, like everything
else); unmatched columns keep built-in synthesis. forge validates every value
**you supply here** against the applied schema — CHECK vocabularies,
varchar/char_length caps, numeric ranges, and the canonical spelling of a
`uuid` column — skipping invalid ones with a warning and falling back to
built-in synthesis for that column. Generated CRUD lifecycle-test fixtures
reuse the same seed plan, so test parent rows carry the same vocabulary.

### Every column is drawn INDEPENDENTLY — rows will not be coherent

This is the thing that surprises people, and it is visible the moment the data
reaches a UI. Each column is a separate deterministic pick, so filling three
pools on one table gives you three unrelated draws per row, not a sensible
row:

```
name: "Bluetooth Turntable"   category: PRODUCT_CATEGORY_HOME   emoji: 🧦
```

Every value is schema-valid and no warning fires — it is a sock-emoji
turntable filed under home goods. Nothing in vocab.yaml can tie those columns
together; there is no archetype or row-template concept, by design (the
column-local hash is what makes a draw stable when you edit a different
column's pool).

That is fine for what seeds are FOR — FK-coherent volume to exercise CRUD,
pagination, and constraints, where nobody reads the rows. It is not fine for a
screenshot, a sales demo, or a dogfooding pass, where incoherent rows read as
an unfinished product.

**When rows have to make sense, hand-write them in `db/seeds/custom/`** — it is
applied after the generated dataset and is exactly the seam for this. A dozen
coherent rows on top of the synthesized volume gets you both.

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

A **cross-column ordering** CHECK (`expires_at > issued_at`) is placed: the pass
ranks the columns and assigns each a value above its lower bound, so the rows
satisfy it by arithmetic rather than by luck.

A **discriminated union** — a top-level `OR` of `AND`-groups, each pinning a
discriminator column and constraining its siblings from there — is placed too,
by picking one branch per row and satisfying it whole:

```sql
CHECK (
    (kind = 'wallet_credit'    AND amount_cents    IS NOT NULL AND amount_cents > 0
                               AND compute_minutes IS NULL)
 OR (kind = 'compute_minutes'  AND compute_minutes IS NOT NULL AND compute_minutes > 0
                               AND amount_cents    IS NULL)
);
-- seeds  row 0: kind='wallet_credit',   amount_cents=1,    compute_minutes=NULL
--        row 1: kind='compute_minutes', amount_cents=NULL, compute_minutes=2  …
```

Branches are taken **round-robin by row**, so every arm of the union appears in
the dataset — a table with only `wallet_credit` rows cannot exercise the other
redemption path at all. The matcher is narrow on purpose: the terms it reads are
`col = <literal>`, `col IS [NOT] NULL` and `col <op> <number>`. A nested `OR`, a
`NOT`, a function call, or a comparison between two columns is refused by name,
as is a union whose columns another mechanism owns (a key, a foreign key, a
UNIQUE column) or that a second multi-column constraint also spans.

**A `BYTEA` column varies per row** (`sample_<column>_<row>`, hex-encoded), so a
`key_hash BYTEA NOT NULL UNIQUE` carries the full row target instead of capping
its table at one row. A `length(col) = N` CHECK is honored, and the row
discriminator survives the truncation.

**A partial unique index is read as the weaker statement it is.**
`UNIQUE (user_id) WHERE revoked_at IS NULL` means one *active* key per user, not
one key per user, so it does not cap the table — as long as forge can show the
predicate false of every row it writes. A predicate it cannot read keeps the
strict reading (the column is treated as plain `UNIQUE`), which is the safe
direction.

A **derivation** (`total_cents = subtotal_cents + tax_cents`) is not placeable
and the plan says so by name, because three independently synthesized values
make an equality true only by coincidence. It is not a seeding problem to work
around — the schema is asserting a rule it never implements. Declare it instead
(`GENERATED ALWAYS AS (…) STORED`, see the `db` skill) and the column stops
being seeded at all, because postgres owns it. **Do not pin the columns to
`{min: 0, max: 0}` in `vocab.yaml` to force the CHECK to hold** — that trades a
warning for a demo database where every money column reads zero.

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

**`authoritative` is usually the right answer.** The record states its own
subject; the longer route is a back-reference. Deriving the other way can
disagree with a column that already scopes the row — a `company_id` stamped
from the caller, say — and then the derived value points at a parent outside
that scope. The rows still satisfy every foreign key, so nothing rejects them:
that class of bug is found by auditing the data, not by a failing test.

**One shape is not a decision, and forge no longer asks about it:** a NOT NULL
direct column whose indirect route leaves by a NULLABLE one. A row must always
name its parent directly and may carry nothing at all on the other route, so
the optional edge cannot be what determines a column that is never absent —
`derived-from` would have to invent a parent for every row whose route is NULL.
Forge resolves those as `authoritative` and stays quiet. What it still refuses
is the symmetric case, where **both** routes are required and either could
honestly be the truth. On a wide schema that is the difference between a page
of refusals and the handful that are real questions.

It lives on the constraint, not in a forge file, because it is a **schema-level
domain fact** — validation and your own business rules need the same sentence —
and because plain SQL lands in the postgres catalog where forge, `psql \d+` and
anything else can read it. Prose in the same comment is fine; only the
`forge:ref ` clause is read.

### Sweep the schema for diamonds yourself

Declare a diamond when you add the second reference rather than waiting for a
refusal. The seeder only reports a pair whose two routes ACTUALLY disagree in
the rows it just planned, so a domain with several diamonds surfaces them a few
at a time over successive runs. This query reports every one at once:

```sql
-- every child that reaches one parent BOTH directly and through a sibling FK,
-- with each route's nullability and whatever is already declared on it
WITH fks AS (
  SELECT c.conname, c.conrelid::regclass::text AS child,
         a.attname AS col, c.confrelid::regclass::text AS parent,
         a.attnotnull AS required
  FROM pg_constraint c
  JOIN unnest(c.conkey) AS k(attnum) ON true
  JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum
  WHERE c.contype = 'f'
)
SELECT d.child, d.col AS direct_col, d.parent,
       s.col AS via_col, s.parent AS via_table,
       d.required AS direct_required, s.required AS via_required,
       COALESCE(obj_description(pc.oid, 'pg_constraint'), '(undeclared)') AS declaration
FROM fks d
JOIN fks s ON s.child = d.child AND s.col <> d.col
JOIN fks t ON t.child = s.parent AND t.parent = d.parent
JOIN pg_constraint pc ON pc.conname = d.conname AND pc.conrelid = d.child::regclass
ORDER BY 1, 2;
```

`forge env config <env>` prints the DSN to run it against (see `dev`).

One rule usually settles every row it returns: when the intermediate link is
REQUIRED and determines the parent (a payment's payer follows from its
invoice), the parent is `derived-from` that link; when the link is OPTIONAL
provenance (an estimate records the lead it came from), the row's own column is
`authoritative`.

## Hand-authored demo data (`db/seeds/custom/`)

Domain-flavored demo rows that the generic synthesizer can't invent go in
`db/seeds/custom/`. These are applied **after** the runtime dataset, so
`ON CONFLICT` overrides of seeded rows are sanctioned there.

See also: `db` for the schema/ORM half.
