package codegen

// Flattening composed config blocks into one KCL schema.
//
// A config block is its own namespace in proto: `StaticSiteConfig.base_domain`
// and `SimpleBackendConfig.base_domain` are two different fields, bound to two
// different env vars, and the Go loader resolves each through its own message.
// Nothing about declaring both is a proto error.
//
// The KCL projection does not have that namespace. It flattens every block's
// leaves into ONE `schema AppConfig`, because an env's config.k authors values
// as a flat instance (`log_level = "debug"`), and nested schema instantiation
// is not projected. So two blocks declaring the same leaf name arrive at the
// emitter as two declarations of one field.
//
// CheckDuplicateConfigFields then refuses the schema, correctly — KCL keeps the
// LAST declaration of a repeated field silently, which once shipped an empty
// APP_URL to three prod workloads. But the refusal lands on the whole AppConfig,
// and `forge generate` downgrades a config-generation failure to a warning, so
// the previous config_gen.k stays on disk. The author does not see an error
// about their field; they see a generated file in which EVERY leaf of BOTH
// blocks is silently absent, indistinguishable from fields that were never
// declared. In control-plane that hid the entire on-switch for two deploy
// tiers, which is how a tier that was fully implemented could not be turned on.
//
// The fix is to give the flattened names the namespace the projection lost, and
// to do it ONLY where a name is actually contested. Qualifying every block leaf
// unconditionally is the cleaner rule and is the wrong one here: every existing
// per-env config.k authors leaves by bare name, and a blanket rename would
// invalidate all of them at once, in files forge does not own.

// FlattenBlockLeaves projects rootMessage's fields into the flat field list the
// KCL emitters consume: its own scalar leaves, plus one level of the leaves of
// every config block it composes.
//
// A leaf claimed by two different blocks is emitted under `<block>_<leaf>`
// (`simple_backend_base_domain`); a leaf claimed by only one keeps its bare
// name. Qualification is decided across the whole message set before any field
// is emitted, so it does not depend on declaration order — both sides of a
// collision are qualified, never just the second one to be visited, which would
// make which name an author writes depend on proto field order.
//
// Only Name is rewritten. EnvVar, defaults and sensitivity are untouched: the
// env var is the actual contract with the running binary, and it was never
// ambiguous — SIMPLE_BACKEND_BASE_DOMAIN and STATIC_SITE_BASE_DOMAIN already
// differ. This function changes what an operator TYPES in config.k, and nothing
// about what the process reads.
func FlattenBlockLeaves(messages []ConfigMessage, rootMessage string) []ConfigField {
	byName := make(map[string]*ConfigMessage, len(messages))
	for i := range messages {
		byName[messages[i].Name] = &messages[i]
	}

	var root *ConfigMessage
	for i := range messages {
		if messages[i].Name == rootMessage {
			root = &messages[i]
			break
		}
	}
	if root == nil {
		return nil
	}

	return flattenWithBlocks(root.Fields, byName)
}

// FlattenFieldsWithBlocks is FlattenBlockLeaves for a caller that already holds
// the field list — the per-binary and multi-root-message emitters, which select
// their fields by a rule of their own before flattening.
func FlattenFieldsWithBlocks(fields []ConfigField, messages []ConfigMessage) []ConfigField {
	byName := make(map[string]*ConfigMessage, len(messages))
	for i := range messages {
		byName[messages[i].Name] = &messages[i]
	}
	return flattenWithBlocks(fields, byName)
}

// flattenWithBlocks is the shared core: it walks one field list, expands each
// composed block one level, and qualifies leaf names that more than one block
// claims. Exported callers differ only in how they obtain the field list.
func flattenWithBlocks(fields []ConfigField, byName map[string]*ConfigMessage) []ConfigField {
	// Pass one: count which blocks claim each leaf name. Counting BLOCKS
	// rather than occurrences matters — a single block declaring a name twice
	// is a real duplicate that must still be refused downstream, not
	// something to paper over by qualifying it.
	claims := map[string]map[string]bool{}
	noteClaim := func(leaf, owner string) {
		if claims[leaf] == nil {
			claims[leaf] = map[string]bool{}
		}
		claims[leaf][owner] = true
	}
	for _, f := range fields {
		if f.MessageType != "" {
			if bm, known := byName[f.MessageType]; known {
				for _, bf := range bm.Fields {
					if bf.MessageType == "" {
						noteClaim(bf.Name, f.Name)
					}
				}
			}
			continue
		}
		noteClaim(f.Name, "")
	}

	// Pass two: emit, qualifying only contested names.
	var out []ConfigField
	for _, f := range fields {
		if f.MessageType != "" {
			bm, known := byName[f.MessageType]
			if !known {
				continue
			}
			for _, bf := range bm.Fields {
				if bf.MessageType != "" {
					continue // one nesting level, as elsewhere
				}
				if len(claims[bf.Name]) > 1 {
					bf.Name = f.Name + "_" + bf.Name
				}
				out = append(out, bf)
			}
			continue
		}
		out = append(out, f)
	}
	return out
}
