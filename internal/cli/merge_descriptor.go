package cli

// Merging gen/forge_descriptor.json.
//
// The descriptor is a committed, derived file, and it carries a stamp of the
// protos it was extracted from (codegen.ForgeDescriptor.Sources: one sha256
// per proto). Two branches that edit DIFFERENT protos each rewrite the stamp,
// and every merge, rebase and cherry-pick of one onto the other has to settle
// it.
//
// It used to be a single digest of every proto, which no driver can settle:
// the merged value is a function of the merged protos, the driver runs for
// gen/ before git has written proto/ into the work tree, and so the best it
// could do was keep one side's digest, stale for the other side's edit. Every
// such rebase then needed a `forge generate` and an amend before CI's
// verify-generated would pass.
//
// Per-proto fingerprints are a map, and a map merges key by key: the side
// that changed a proto's fingerprint wins it, a proto only one side added or
// removed is added or removed. The merged stamp is then exactly what a
// generate over the merged protos writes, with no generate in between. A
// proto both sides edited differently has no right answer here (the merged
// content is a third thing), so ours' fingerprint stays, still stale, which
// loadDescriptor refuses until `forge generate` refreshes it — and in that
// case git has the proto itself in conflict, so a person is in the file.

import (
	"bytes"
	"context"
	"encoding/json"
	"path"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
)

const (
	descriptorFileName = "forge_descriptor.json"
	sourcesKeyLine     = `  "sources": `
	// sourcesPlaceholder stands in for the sources block while the rest of
	// the descriptor merges as text.
	sourcesPlaceholder = "__FORGE_MERGE_DESCRIPTOR_SOURCES__"
)

// isDescriptorPath reports whether relPath is the descriptor.
func isDescriptorPath(relPath string) bool {
	return relPath != "" && path.Base(strings.ReplaceAll(relPath, `\`, "/")) == descriptorFileName
}

// splitSources lifts the sources block out of a descriptor, leaving a
// placeholder line where it was. ok is false when the file has no block (an
// unstamped descriptor, or one from a forge that predates the stamp).
func splitSources(text []byte) (rest []byte, sources map[string]string, ok bool) {
	lines := strings.SplitAfter(string(text), "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimRight(line, "\r\n") == sourcesKeyLine+"{" {
			start = i
			break
		}
	}
	if start < 0 {
		return text, nil, false
	}
	end := -1
	for j := start + 1; j < len(lines); j++ {
		if strings.TrimRight(lines[j], ",\r\n") == "  }" {
			end = j
			break
		}
	}
	if end < 0 {
		return text, nil, false
	}
	block := strings.TrimSuffix(strings.TrimRight(strings.Join(lines[start:end+1], ""), "\r\n"), ",")
	block = strings.TrimPrefix(strings.TrimSpace(block), `"sources":`)
	if json.Unmarshal([]byte(block), &sources) != nil {
		return text, nil, false
	}
	comma := ""
	if strings.HasSuffix(strings.TrimRight(lines[end], "\r\n"), ",") {
		comma = ","
	}
	nl := "\n"
	if !strings.HasSuffix(lines[end], "\n") {
		nl = ""
	}
	out := append([]string{}, lines[:start]...)
	out = append(out, sourcesKeyLine+sourcesPlaceholder+comma+nl)
	out = append(out, lines[end+1:]...)
	return []byte(strings.Join(out, "")), sources, true
}

// renderSources writes the block the way forge's writer does: encoding/json
// sorts map keys, and the descriptor nests the object one level in.
func renderSources(sources map[string]string) string {
	if len(sources) == 0 {
		return "{}"
	}
	data, err := json.MarshalIndent(sources, "  ", "  ")
	if err != nil {
		return "{}"
	}
	return string(data)
}

// joinSources puts merged sources into text in place of the placeholder.
func joinSources(text []byte, sources map[string]string) []byte {
	return bytes.Replace(text, []byte(sourcesPlaceholder), []byte(renderSources(sources)), 1)
}

// mergeDescriptor three-way merges the descriptor: its sources by key, the
// rest as text. clean is false when the rest conflicts; merged then carries
// git's conflict markers (the caller decides what a descriptor conflict
// means).
func mergeDescriptor(ctx context.Context, base, ours, theirs []byte, markerSize int) (merged []byte, clean, ok bool, err error) {
	nBase, bSrc, bOK := splitSources(base)
	nOurs, oSrc, oOK := splitSources(ours)
	nTheirs, tSrc, tOK := splitSources(theirs)
	if !bOK || !oOK || !tOK {
		// An add/add merge, or a side from before the stamp existed: the
		// text merge below has no common block to merge, so the caller
		// falls back to treating the descriptor as plain text.
		return nil, false, false, nil
	}
	merged, clean, err = mergeGenerated(ctx, nBase, nOurs, nTheirs, markerSize)
	if err != nil {
		return nil, false, false, err
	}
	if !bytes.Contains(merged, []byte(sourcesPlaceholder)) {
		return merged, clean, true, nil
	}
	return joinSources(merged, checksums.MergeStringMap(bSrc, oSrc, tSrc)), clean, true, nil
}

// oursWithMergedSources is the answer for a descriptor whose body conflicts:
// the descriptor is rewritten whole on every generate and certified by
// nothing, so this side stands, with the sources merged so the stamp stays
// as right as it can be.
func oursWithMergedSources(base, ours, theirs []byte) []byte {
	_, bSrc, _ := splitSources(base)
	nOurs, oSrc, oOK := splitSources(ours)
	_, tSrc, _ := splitSources(theirs)
	if !oOK {
		return ours
	}
	return joinSources(nOurs, checksums.MergeStringMap(bSrc, oSrc, tSrc))
}
