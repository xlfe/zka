package zka

import (
	"fmt"
	"strings"
	"unicode"
)

// This file encodes Kitty's own string handling. Every function here has a
// named counterpart in the Kitty source so the two can be kept in step:
//
//	shlexQuote/shlexSplit    kitty/utils.py shlex_split -> Shlex (POSIX-like)
//	escapeExpandVars         kitty/utils.py expandvars ("$$" is the literal "$")
//	pythonIsSpace            CPython str.isspace, used by Kitty's .strip() calls
//	pythonIsLineBoundary     CPython str.splitlines, used by parse_session
//	ansiCEscape              kitten @ expand_ansi_c_escapes_in_args
//
// Getting any of these wrong writes a value Kitty cannot hand back unchanged,
// which the reconciler then reads as permanent divergence.

// shlexSafe is Python shlex.quote's unquoted set: re.compile(r'[^\w@%+=:,./-]',
// re.ASCII). Anything outside it, including every non-ASCII rune, is quoted.
func shlexSafe(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("_@%+=:,./-", r)
}

// shlexQuote returns value quoted so Kitty's Shlex yields exactly value. It is
// byte-compatible with Python shlex.quote, which is what Kitty itself writes
// when it serializes a launch line (kitty/tabs.py uses shlex.join).
func shlexQuote(value string) string {
	if value == "" {
		return "''"
	}
	safe := true
	for _, r := range value {
		if !shlexSafe(r) {
			safe = false
			break
		}
	}
	if safe {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func shlexJoin(tokens []string) string {
	quoted := make([]string, 0, len(tokens))
	for _, token := range tokens {
		quoted = append(quoted, shlexQuote(token))
	}
	return strings.Join(quoted, " ")
}

// shlexSplit is the exact inverse of shlexJoin and matches Kitty's Shlex:
// single quotes are literal, a backslash always escapes the next rune outside
// single quotes (including inside double quotes), and unterminated quotes or a
// trailing backslash are errors.
func shlexSplit(input string) ([]string, error) {
	var result []string
	var word strings.Builder
	inWord := false
	var quote rune
	escaped := false
	flush := func() {
		if inWord {
			result = append(result, word.String())
			word.Reset()
			inWord = false
		}
	}
	for _, r := range input {
		if escaped {
			word.WriteRune(r)
			inWord = true
			escaped = false
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			inWord = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			inWord = true
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			inWord = true
			continue
		}
		if pythonIsSpace(r) {
			flush()
			continue
		}
		word.WriteRune(r)
		inWord = true
	}
	if escaped {
		return nil, fmt.Errorf("trailing backslash at end of input data")
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated string at the end of input")
	}
	flush()
	return result, nil
}

// escapeExpandVars doubles every "$" so Kitty's expandvars returns value
// unchanged. Kitty applies expandvars to every session directive except launch
// and set_layout_state.
func escapeExpandVars(value string) string {
	return strings.ReplaceAll(value, "$", "$$")
}

// unescapeExpandVars reverses escapeExpandVars. It is expandvars with an empty
// environment and no OS fallback, which reduces to a left-to-right "$$" -> "$"
// pass because unset variables are left as written. Use it only on text zka
// itself rendered; Kitty writes its own session output unescaped.
func unescapeExpandVars(value string) string {
	if !strings.Contains(value, "$$") {
		return value
	}
	var out strings.Builder
	out.Grow(len(value))
	for i := 0; i < len(value); i++ {
		if value[i] == '$' && i+1 < len(value) && value[i+1] == '$' {
			out.WriteByte('$')
			i++
			continue
		}
		out.WriteByte(value[i])
	}
	return out.String()
}

// ansiCEscape makes value survive the ANSI-C escape expansion that kitten
// applies to the positional arguments of set-tab-title and set-window-title
// (both declare special_parse='expand_ansi_c_escapes_in_args(args...)').
// Doubling backslashes is exactly sufficient: expansion only ever consumes a
// backslash-led sequence.
func ansiCEscape(value string) string {
	return strings.ReplaceAll(value, `\`, `\\`)
}

// pythonIsSpace mirrors CPython str.isspace, which drives every .strip() Kitty
// performs on a directive value. Go's unicode.IsSpace omits the C1-adjacent
// separators, so they are added explicitly.
func pythonIsSpace(r rune) bool {
	switch r {
	case 0x1c, 0x1d, 0x1e, 0x1f:
		return true
	}
	return unicode.IsSpace(r)
}

// pythonIsLineBoundary mirrors CPython str.splitlines, which is how Kitty
// divides a session file. It is a strictly wider set than "\r" and "\n".
func pythonIsLineBoundary(r rune) bool {
	switch r {
	case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
		return true
	}
	return false
}

// canonicalLineValue makes value safe to place on a session-file line: no rune
// that Kitty would treat as a line break, and no NUL. Every value entering the
// desired state must already satisfy it.
func canonicalLineValue(value string) string {
	return strings.Map(func(r rune) rune {
		if r == 0 {
			return -1
		}
		if pythonIsLineBoundary(r) {
			return ' '
		}
		return r
	}, value)
}

// canonicalStrippedValue additionally trims Python whitespace, for the
// directives Kitty strips: new_tab, title, layout, enabled_layouts entries,
// os_window_*, and cd. Values that do not survive this are unobservable.
func canonicalStrippedValue(value string) string {
	return strings.TrimFunc(canonicalLineValue(value), pythonIsSpace)
}
