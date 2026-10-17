package cmd

import (
	"os"
	"strings"
)

// A project's .env is read twice over: by docker compose, which parses it as
// a dotenv file (quotes, `export`, inline comments), and by the CLI, which
// used to take everything after KEY= as the value. Where the CLI decides
// what compose will do with a value (the profiles, the project's name, the
// compose files, the release), it reads it as compose does (dotenvValue);
// readEnvVar stays the raw text, for the values the CLI writes and carries
// forward as they are (the generated secrets).

// parseDotenvLine reads one line of a dotenv file: its key and the value
// compose takes from it. ok is false for a blank line, a comment and a line
// with no KEY=.
func parseDotenvLine(line string) (key, value string, ok bool) {
	s := strings.TrimLeft(strings.TrimRight(line, "\r"), " \t")
	if s == "" || s[0] == '#' {
		return "", "", false
	}
	if rest, found := strings.CutPrefix(s, "export"); found && rest != "" && (rest[0] == ' ' || rest[0] == '\t') {
		s = strings.TrimLeft(rest, " \t")
	}
	eq := strings.IndexByte(s, '=')
	if eq <= 0 {
		return "", "", false
	}
	key = strings.TrimRight(s[:eq], " \t")
	if key == "" || strings.ContainsAny(key, " \t") {
		return "", "", false
	}
	return key, parseDotenvValue(s[eq+1:]), true
}

// parseDotenvValue is the value compose takes from the text after KEY=:
// a single-quoted value is literal, a double-quoted one has its backslash
// escapes resolved, and an unquoted one ends at an inline comment (a # after
// white space) and loses the white space around it. Text after a closing
// quote (a comment) is dropped. A value with an opening quote and no closing
// one on its line is taken as written, to the end of the line.
func parseDotenvValue(raw string) string {
	v := strings.TrimLeft(raw, " \t")
	if v == "" {
		return ""
	}
	switch v[0] {
	case '\'':
		if end := strings.IndexByte(v[1:], '\''); end >= 0 {
			return v[1 : 1+end]
		}
	case '"':
		for i := 1; i < len(v); i++ {
			switch v[i] {
			case '\\':
				i++
			case '"':
				return unescapeDotenv(v[1:i])
			}
		}
	}
	// Unquoted: a # that follows white space starts a comment (also the one
	// right after "KEY=" and the white space that follows it).
	for i := 0; i < len(raw); i++ {
		if raw[i] == '#' && i > 0 && (raw[i-1] == ' ' || raw[i-1] == '\t') {
			raw = raw[:i]
			break
		}
	}
	return strings.TrimSpace(raw)
}

// unescapeDotenv resolves the backslash escapes of a double-quoted value:
// \n and \r are the control characters, any other \X is X.
func unescapeDotenv(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i == len(s)-1 {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case '"', '\\', '$', '\'':
			b.WriteByte(s[i])
		default:
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// dotenvValue is the value compose takes for KEY from the dotenv file at
// path: the last KEY= line's, with its quotes, escapes and inline comment
// handled (parseDotenvValue); "" when there is none or the file cannot be
// read. (A ${VAR} in it is not expanded.)
func dotenvValue(path, key string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	val := ""
	for _, l := range strings.Split(string(data), "\n") {
		if k, v, ok := parseDotenvLine(l); ok && k == key {
			val = v
		}
	}
	return val
}
