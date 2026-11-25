package naming

// EntityTypeLocalSuffix is what a generated frontend file appends to an
// entity's message name to bind its type: `import type { Link as LinkEntity }`.
//
// An entity is named by the user, and a generated page also imports names the
// user never sees: `Link` from next/link or @tanstack/react-router, `Image`,
// `Head`, `Script`, `Form`, React hooks, the component library's `Button` and
// `Input`. A page that bound the entity type under its own name declared that
// identifier twice the moment an entity shared one (`import type { Link }`
// beside `import Link from "next/link"`), and the frontend no longer
// type-checked. Binding it under a suffixed local name makes the collision
// impossible whatever the entity is called, without forge keeping a list of
// reserved names that would be wrong the day a template imports something new.
//
// The upgrade codemod `entity-type-alias` (internal/codemod) rewrites pages
// scaffolded before the alias to the same name, so it reads this too.
const EntityTypeLocalSuffix = "Entity"

// EntityTypeLocal is the local name a generated frontend file binds entity's
// message type to: "Link" → "LinkEntity".
func EntityTypeLocal(entity string) string { return entity + EntityTypeLocalSuffix }
