package migratekit

import (
	"strings"
)

// A MIGRATION IS RETRIED ONLY WHEN IT IS PROVEN ALL-OR-NOTHING.
//
// golang-migrate sends a file to postgres as one query, which postgres runs as
// one implicit transaction: a failure rolls the whole file back, and running it
// again is a retry. That stops being true the moment the file ends the
// transaction itself (`BEGIN; … COMMIT; ALTER TABLE …` commits the first half
// before the second can time out), and a retry would then repeat work that is
// already committed. Whether a file does that cannot be read off the text with
// a regular expression: `COMMIT` can follow a `;` on the same line, sit behind
// a comment, or appear inside a string or a function body, where it means
// nothing. So the file is tokenized the way postgres's lexer reads it —
// comments, string constants, quoted identifiers and dollar-quoted bodies are
// skipped — and its statements are classified from what is left.
//
// When the classification cannot prove a file retryable, it is not retried:
// the first lock timeout fails the run and leaves the schema dirty at the
// file's version, as any partly applied migration does.

type tokKind byte

const (
	tokWord   tokKind = 'w' // an unquoted identifier or keyword, lower-cased
	tokQuoted tokKind = 'q' // a "quoted identifier", exactly as written
	tokOther  tokKind = 'o' // a string constant or dollar-quoted body (content dropped)
	tokPunct  tokKind = 'p' // one punctuation character
)

type sqlTok struct {
	kind tokKind
	text string
}

func (t sqlTok) isWord(w string) bool { return t.kind == tokWord && t.text == w }

func isIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func isIdentByte(c byte) bool { return isIdentStart(c) || c >= '0' && c <= '9' || c == '$' }

// splitStatements tokenizes src and splits it at the `;` postgres would end a
// statement at: one outside comments, strings, quoted identifiers and
// dollar-quoted bodies. Empty statements are dropped.
func splitStatements(src string) [][]sqlTok {
	var (
		out [][]sqlTok
		cur []sqlTok
	)
	end := func() {
		if len(cur) > 0 {
			out = append(out, cur)
			cur = nil
		}
	}
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == ';':
			end()
			i++
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			i++
		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			if j := strings.IndexByte(src[i:], '\n'); j >= 0 {
				i += j
			} else {
				i = len(src)
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			i = blockCommentEnd(src, i)
		case c == '\'':
			i = quotedEnd(src, i, '\'', false)
			cur = append(cur, sqlTok{kind: tokOther})
		case c == '"':
			j := quotedEnd(src, i, '"', false)
			cur = append(cur, sqlTok{kind: tokQuoted, text: strings.ReplaceAll(src[i+1:max(i+1, j-1)], `""`, `"`)})
			i = j
		case c == '$':
			if j, ok := dollarQuoteEnd(src, i); ok {
				cur = append(cur, sqlTok{kind: tokOther})
				i = j
			} else {
				cur = append(cur, sqlTok{kind: tokPunct, text: "$"})
				i++
			}
		case isIdentStart(c):
			j := i + 1
			for j < len(src) && isIdentByte(src[j]) {
				j++
			}
			word := src[i:j]
			if (word == "e" || word == "E") && j < len(src) && src[j] == '\'' {
				i = quotedEnd(src, j, '\'', true)
				cur = append(cur, sqlTok{kind: tokOther})
				continue
			}
			cur = append(cur, sqlTok{kind: tokWord, text: strings.ToLower(word)})
			i = j
		default:
			cur = append(cur, sqlTok{kind: tokPunct, text: string(c)})
			i++
		}
	}
	end()
	return out
}

// blockCommentEnd returns the index after the (nested) block comment starting
// at src[i:], or len(src) when it is unterminated.
func blockCommentEnd(src string, i int) int {
	depth := 0
	for i < len(src) {
		switch {
		case strings.HasPrefix(src[i:], "/*"):
			depth++
			i += 2
		case strings.HasPrefix(src[i:], "*/"):
			depth--
			i += 2
			if depth == 0 {
				return i
			}
		default:
			i++
		}
	}
	return len(src)
}

// quotedEnd returns the index after the quoted run starting at src[i] (the
// opening quote), where a doubled quote is an escaped one and, when
// backslashes is set (an E'…' string), `\` escapes the next byte. An
// unterminated run extends to the end.
func quotedEnd(src string, i int, q byte, backslashes bool) int {
	for j := i + 1; j < len(src); j++ {
		switch {
		case backslashes && src[j] == '\\':
			j++
		case src[j] == q:
			if j+1 < len(src) && src[j+1] == q {
				j++
				continue
			}
			return j + 1
		}
	}
	return len(src)
}

// dollarQuoteEnd returns the index after the dollar-quoted body whose opening
// tag starts at src[i], and ok=false when src[i] does not open one (`$1`).
func dollarQuoteEnd(src string, i int) (int, bool) {
	if i > 0 && isIdentByte(src[i-1]) {
		return 0, false
	}
	j := i + 1
	if j < len(src) && isIdentStart(src[j]) {
		for j < len(src) && isIdentByte(src[j]) && src[j] != '$' {
			j++
		}
	}
	if j >= len(src) || src[j] != '$' {
		return 0, false
	}
	tag := src[i : j+1]
	if k := strings.Index(src[j+1:], tag); k >= 0 {
		return j + 1 + k + len(tag), true
	}
	return len(src), true
}

// A createIndexTarget is the index a CREATE INDEX CONCURRENTLY statement
// builds: its name, and the table it is on exactly as written (qualified or
// not), rendered so that to_regclass resolves it under postgres's own
// search-path rules. An index lives in its table's schema, so the table
// decides where the index is.
type createIndexTarget struct {
	index string
	table string
}

// filePlan is what a migration file is, from its statements.
type filePlan struct {
	// endsTransaction is true when a statement controls the transaction
	// itself: after one, a failure no longer rolls the whole file back.
	endsTransaction bool
	// runsProcedural is true when a top-level CALL or DO runs code: a
	// procedure or DO block may COMMIT on its own outside a transaction
	// block, and what it commits is invisible to this scan (the body is
	// quoted), so a failure after it is not proven to have rolled back.
	runsProcedural bool
	// unretryableConcurrently is true when the file holds a CONCURRENTLY
	// statement other than CREATE INDEX or DROP INDEX: REINDEX leaves
	// `_ccnew` indexes behind and DETACH PARTITION leaves the partition in a
	// state only FINALIZE resolves, so neither is run twice blindly.
	unretryableConcurrently bool
	// creates are the CREATE INDEX CONCURRENTLY statements; unnamed is true
	// when one names no index (postgres picks the name), so there is nothing
	// to clean up by name.
	creates []createIndexTarget
	unnamed bool
}

// retryable reports whether a failed attempt at the file can be run again.
func (p filePlan) retryable() bool {
	return !p.endsTransaction && !p.runsProcedural && !p.unretryableConcurrently && !p.unnamed
}

// transactionControl are the statements that start, end or divide a
// transaction, as the first word of a statement.
var transactionControl = map[string]bool{
	"begin": true, "start": true, "commit": true, "end": true, "abort": true,
	"rollback": true, "savepoint": true, "release": true,
}

func planFile(body []byte) filePlan {
	var p filePlan
	for _, s := range splitStatements(string(body)) {
		first := s[0]
		if first.kind == tokWord && (transactionControl[first.text] ||
			first.text == "prepare" && len(s) > 1 && s[1].isWord("transaction")) {
			p.endsTransaction = true
		}
		if first.kind == tokWord && (first.text == "call" || first.text == "do") {
			p.runsProcedural = true
		}
		concurrently := false
		for _, t := range s {
			if t.isWord("concurrently") {
				concurrently = true
				break
			}
		}
		if !concurrently {
			continue
		}
		switch {
		case createsIndex(s):
			if t, ok := createIndexOf(s); ok {
				p.creates = append(p.creates, t)
			} else {
				p.unnamed = true
			}
		case first.isWord("drop") && len(s) > 2 && s[1].isWord("index"):
			// DROP INDEX CONCURRENTLY is safe to run again: a failed one
			// leaves the index invalid and the next attempt finishes it.
		default:
			p.unretryableConcurrently = true
		}
	}
	return p
}

func createsIndex(s []sqlTok) bool {
	i := 1
	if len(s) > 1 && s[1].isWord("unique") {
		i = 2
	}
	return s[0].isWord("create") && len(s) > i && s[i].isWord("index")
}

// createIndexOf reads `CREATE [UNIQUE] INDEX CONCURRENTLY [IF NOT EXISTS]
// name ON [ONLY] [schema.]table …`.
func createIndexOf(s []sqlTok) (createIndexTarget, bool) {
	i := 2
	if s[1].isWord("unique") {
		i = 3
	}
	skip := func(words ...string) {
		for _, w := range words {
			if i < len(s) && s[i].isWord(w) {
				i++
			} else {
				return
			}
		}
	}
	skip("concurrently")
	if i+2 < len(s) && s[i].isWord("if") && s[i+1].isWord("not") && s[i+2].isWord("exists") {
		i += 3
	}
	if i >= len(s) || !identTok(s[i]) || s[i].isWord("on") {
		return createIndexTarget{}, false
	}
	name := s[i].text
	i++
	if i >= len(s) || !s[i].isWord("on") {
		return createIndexTarget{}, false
	}
	i++
	skip("only")
	var table []string
	for i < len(s) && identTok(s[i]) {
		table = append(table, quoteIdent(s[i].text))
		i++
		if i < len(s) && s[i].kind == tokPunct && s[i].text == "." {
			i++
			continue
		}
		break
	}
	if len(table) == 0 || len(table) > 2 {
		return createIndexTarget{}, false
	}
	return createIndexTarget{index: name, table: strings.Join(table, ".")}, true
}

func identTok(t sqlTok) bool { return t.kind == tokWord || t.kind == tokQuoted }
