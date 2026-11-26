package mlcollect

// Argument redaction: what a snapshot keeps of a container's command and
// args (and of an exec probe's command, and of a string list in a custom
// object, an operator's `args`).
//
// Discovery reads arguments — a vLLM `--model` or `serve <id>`, an
// `s3://`/`hf://` fetch, an exporter's `-f <counters file>`, a collector's
// `--config <path>`, `--tracing-config-file`, `run <alloy config>` — so they
// keep their shape. What goes is every credential an argument can carry:
//
//   - the value of a flag named like a credential, attached or as the next
//     argument (`--token=v`, `--api-key v`, `-passin v`), including when the
//     flag ends a container's command and its value starts its args: the
//     kubelet runs command then args as one argv, and so they are redacted
//     as one (RedactArgv);
//   - a user:password value (`curl -u u:p`, `-uu:p`, `--user=u:p`), an HTTP
//     header (`-H "X-Key: v"`), a short password flag's value (`-p v`, unless
//     it is a port or a path; `mysql -pv`, `redis-cli -a v`, `openssl -k v`);
//   - `NAME=v` for a variable named like a credential, a sensitive header
//     written into a command (`Authorization: Bearer v`), a URL's userinfo
//     password, a presigned URL's signature, and any token of a well-known
//     vendor format (sk-…, hf_…, ghp_…, AKIA…, a JWT).
//
// A shell script in one argument (`sh -c "…"`) is split into words and
// commands and redacted by the same rules.

import (
	"path"
	"regexp"
	"strings"
)

// RedactArgv redacts a container's command and args as the one argv the
// kubelet runs (args after command), then splits it back.
func RedactArgv(command, args []string) ([]string, []string) {
	if command == nil && args == nil {
		return nil, nil
	}
	red := RedactArgs(append(append([]string{}, command...), args...))
	var c, a []string
	if command != nil {
		c = red[:len(command)]
	}
	if args != nil {
		a = red[len(command):]
	}
	return c, a
}

// RedactArgs is one argv with every credential in it replaced by Redacted.
func RedactArgs(argv []string) []string {
	if argv == nil {
		return nil
	}
	toks := make([]argTok, len(argv))
	for i, a := range argv {
		toks[i] = argTok{val: a}
	}
	markCredentialValues(toks)
	out := make([]string, len(argv))
	for i, t := range toks {
		switch {
		case t.redact:
			out[i] = Redacted
		case isScript(t.val):
			out[i] = redactScript(t.val)
		default:
			out[i] = redactWord(t.val, t.program)
		}
	}
	return out
}

// RedactArgText is one string — an argument, a script, a URL — with its
// credentials replaced by the rules RedactArgs applies to one argument.
func RedactArgText(a string) string {
	if isScript(a) {
		return redactScript(a)
	}
	return redactWord(a, "")
}

// argTok is one word of an argv or a script.
type argTok struct {
	val     string // the word, unquoted
	sep     bool   // a command separator (; | & newline): the next word is a program
	program string // the base name of the command the word belongs to
	redact  bool   // the word is a credential flag's value
}

// wrappers run the command after them; the program is the word they wrap.
var wrappers = map[string]bool{"sudo": true, "env": true, "exec": true, "nohup": true, "timeout": true, "time": true, "command": true, "xargs": true}

var numberRE = regexp.MustCompile(`^[0-9.]+[smhd]?$`)

// markCredentialValues marks every word that is the value of the credential
// flag before it, and records each word's program.
func markCredentialValues(toks []argTok) {
	program, wantProgram := "", true
	for i := range toks {
		t := &toks[i]
		if t.sep {
			wantProgram = true
			continue
		}
		if wantProgram && !strings.HasPrefix(t.val, "-") && !isAssignment(t.val) &&
			!wrappers[path.Base(t.val)] && !numberRE.MatchString(t.val) {
			program, wantProgram = path.Base(t.val), false
		}
		t.program = program
		if i > 0 && !toks[i-1].sep && !toks[i-1].redact && takesCredential(toks[i-1].val, program, t.val) {
			t.redact = true
		}
	}
}

func isAssignment(w string) bool {
	i := strings.IndexByte(w, '=')
	return i > 0 && !strings.HasPrefix(w, "-")
}

// credentialName matches a flag name (dashes trimmed) whose value is a
// credential.
var credentialName = regexp.MustCompile(`(?i)(token|password|passwd|passphrase|pwd|secret|key|auth|credential|bearer|cookie|signature|header)`)

// credentialShortNames are single-dash flags spelt as words that take a
// credential (openssl's -passin pass:…), beyond credentialName.
var credentialShortNames = map[string]bool{"pass": true, "passin": true, "passout": true, "pw": true, "psk": true, "pin": true}

// programCredentialFlags are short flags that take a credential in one
// program only.
var programCredentialFlags = map[string]map[string]bool{
	"redis-cli": {"-a": true},
	"openssl":   {"-k": true},
	"smbclient": {"-U": true},
}

// mysqlLike take a password attached to -p (`mysql -pS3cret`).
var mysqlLike = map[string]bool{"mysql": true, "mysqladmin": true, "mysqldump": true, "mariadb": true, "mariadb-dump": true, "mysqlsh": true}

// takesCredential reports whether value is the credential flag's value.
func takesCredential(flag, program, value string) bool {
	if !strings.HasPrefix(flag, "-") || flag == "-" || flag == "--" || strings.Contains(flag, "=") {
		return false
	}
	if strings.HasPrefix(value, "--") {
		return false // the next flag, not a value
	}
	name := strings.TrimLeft(flag, "-")
	switch {
	case flag == "-H" || strings.EqualFold(name, "header"):
		return strings.Contains(value, ":")
	case flag == "-u" || flag == "-U" || name == "user" || name == "proxy-user":
		return strings.ContainsAny(value, ":%")
	case programCredentialFlags[program][flag]:
		return true
	case flag == "-p" || flag == "-P":
		return !strings.HasPrefix(value, "-") && !keepableValue(value)
	case len(name) > 1 && (credentialName.MatchString(name) || credentialShortNames[strings.ToLower(name)]):
		return true
	}
	return false
}

// keepableValue is a value a short password flag's heuristic leaves alone:
// a port or port mapping, a number, a path, a URI.
var keepableValue = regexp.MustCompile(`^(?:[0-9]+(?:[:.\-][0-9]+)*(?:/(?:tcp|udp|sctp))?|[.~]{0,2}/.*|[a-z][a-z0-9+.-]*://.*)$`).MatchString

var (
	// --token=v, -passin=v
	wordFlagAssign = regexp.MustCompile(`^(-{1,2}([A-Za-z0-9_.-]+)=)(.+)$`)
	// -uuser:pass, -Uuser%pass
	wordUserAttached = regexp.MustCompile(`^(-[uU])(\S*[:%]\S*)$`)
)

// redactWord is one word with its attached credentials replaced: a
// credential flag's `=value`, an attached user:password or mysql password,
// then the substring rules of argText.
func redactWord(w, program string) string {
	if m := wordFlagAssign.FindStringSubmatch(w); m != nil {
		name := m[2]
		if credentialName.MatchString(name) || credentialShortNames[strings.ToLower(name)] ||
			((name == "user" || name == "proxy-user") && strings.ContainsAny(m[3], ":%")) {
			return m[1] + Redacted
		}
	}
	if m := wordUserAttached.FindStringSubmatch(w); m != nil {
		return m[1] + Redacted
	}
	if mysqlLike[program] && len(w) > 2 && strings.HasPrefix(w, "-p") {
		return "-p" + Redacted
	}
	return argText(w)
}

// sensitiveArg names what an argument must never carry a value for:
// SensitiveKey's words plus a presigned URL's signature.
const sensitiveArg = `(?:token|password|passwd|pwd|secret|key|auth|authorization|header|credential|bearer|cookie|signature)`

// argValue is a value as a shell or a URL delimits it.
const argValue = `("[^"]*"|'[^']*'|[^\s"'&;]+)`

var (
	// --token=v, HF_TOKEN=v, ?X-Amz-Signature=v
	argAssign = regexp.MustCompile(`(?i)((?:^|[\s"'?&;(])-{0,2}[A-Za-z0-9_.-]*` + sensitiveArg + `[A-Za-z0-9_.-]*=)` + argValue)
	// Authorization: Bearer v, X-Api-Key: v
	argHeader = regexp.MustCompile(`(?i)([A-Za-z0-9_-]*` + sensitiveArg + `[A-Za-z0-9_-]*\s*:\s*)[^"'\n;]+`)
	// scheme://user:password@host
	argUserinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://[^/\s:@"']*:)[^/\s@"']+@`)
	// tokens of a well-known vendor format, wherever they appear
	argKnownSecret = regexp.MustCompile(`\b(?:sk-(?:proj-|ant-)?[A-Za-z0-9_-]{16,}|hf_[A-Za-z0-9]{20,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{20,}|xox[abprs]-[A-Za-z0-9-]{10,}|(?:AKIA|ASIA)[0-9A-Z]{16}|nvapi-[A-Za-z0-9_-]{20,}|eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,})`)
)

// argText applies the substring rules: a URL's userinfo password, a
// sensitive header, a credential-named assignment, a vendor-format token.
func argText(a string) string {
	a = argUserinfo.ReplaceAllString(a, "${1}"+Redacted+"@")
	a = argHeader.ReplaceAllString(a, "${1}"+Redacted)
	a = argAssign.ReplaceAllString(a, "${1}"+Redacted)
	return argKnownSecret.ReplaceAllString(a, Redacted)
}

func isScript(s string) bool { return strings.ContainsAny(s, " \t\n") }

// redactScript redacts a shell script: split into words and commands, each
// word redacted as an argv's would be, then the substring rules over the
// whole text for what spans words (an unquoted `Authorization: Bearer v`).
func redactScript(s string) string {
	spans := scriptWords(s)
	toks := make([]argTok, len(spans))
	for i, sp := range spans {
		toks[i] = argTok{val: unquote(s[sp.start:sp.end]), sep: sp.sep}
	}
	markCredentialValues(toks)
	var b strings.Builder
	last := 0
	for i, sp := range spans {
		b.WriteString(s[last:sp.start])
		raw := s[sp.start:sp.end]
		switch {
		case sp.sep:
			b.WriteString(raw)
		case toks[i].redact:
			b.WriteString(Redacted)
		default:
			b.WriteString(redactWord(raw, toks[i].program))
		}
		last = sp.end
	}
	b.WriteString(s[last:])
	return argText(b.String())
}

type wordSpan struct {
	start, end int
	sep        bool
}

// scriptWords splits a shell script into words (quotes and backslash
// escapes kept inside a word) and command separators (; | & and newline).
func scriptWords(s string) []wordSpan {
	isSep := func(c byte) bool { return c == ';' || c == '|' || c == '&' }
	var out []wordSpan
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\n':
			out = append(out, wordSpan{i, i + 1, true})
			i++
			continue
		case c == ' ' || c == '\t' || c == '\r':
			i++
			continue
		case isSep(c):
			j := i
			for j < len(s) && isSep(s[j]) {
				j++
			}
			out = append(out, wordSpan{i, j, true})
			i = j
			continue
		}
		j := i
		var quote byte
	word:
		for j < len(s) {
			ch := s[j]
			switch {
			case quote != 0:
				if ch == quote {
					quote = 0
				}
			case ch == '"' || ch == '\'':
				quote = ch
			case ch == '\\' && j+1 < len(s):
				j++
			case ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n' || isSep(ch):
				break word
			}
			j++
		}
		out = append(out, wordSpan{i, j, false})
		i = j
	}
	return out
}

var unquoter = strings.NewReplacer(`"`, "", `'`, "", `\`, "")

func unquote(w string) string { return unquoter.Replace(w) }
