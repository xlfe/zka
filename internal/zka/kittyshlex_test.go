package zka

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func FuzzShlexRoundTrip(f *testing.F) {
	for _, seed := range []string{
		"", "plain", "a b", "it's", `a"b`, `back\slash`, "$HOME", "#hash",
		"tab\there", "uni✓", "trailing\\", "-leading", "a=b",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		if !utf8.ValidString(value) {
			t.Skip()
		}
		tokens, err := shlexSplit(shlexQuote(value))
		if err != nil {
			t.Fatalf("shlexSplit(shlexQuote(%q)) failed: %v", value, err)
		}
		// A value that is entirely whitespace splits to nothing, which is what
		// a shell does too.
		if strings.TrimFunc(value, pythonIsSpace) == "" && value != "" {
			if len(tokens) == 1 && tokens[0] == value {
				return
			}
		}
		if len(tokens) != 1 || tokens[0] != value {
			t.Fatalf("round trip of %q produced %#v", value, tokens)
		}
	})
}

func FuzzExpandVarsRoundTrip(f *testing.F) {
	for _, seed := range []string{"", "$", "$$", "$HOME", "${X}", "cost $5", "a$$b"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		if !utf8.ValidString(value) {
			t.Skip()
		}
		if got := unescapeExpandVars(escapeExpandVars(value)); got != value {
			t.Fatalf("expandvars round trip of %q produced %q", value, got)
		}
	})
}

func FuzzLaunchLineRoundTrip(f *testing.F) {
	f.Add("--cwd", "/work", "zka")
	f.Add("--var", "note=cost $5", "zka pane")
	f.Add("--title", `it's "mine"`, "prog")
	f.Fuzz(func(t *testing.T, name, value, arg string) {
		if !utf8.ValidString(name) || !utf8.ValidString(value) || !utf8.ValidString(arg) {
			t.Skip()
		}
		// Option names are structural; only well-formed ones are meaningful.
		if !strings.HasPrefix(name, "--") || strings.ContainsAny(name, "= \t\n") || len(name) < 3 {
			t.Skip()
		}
		value, arg = canonicalLineValue(value), canonicalLineValue(arg)
		if strings.TrimFunc(arg, pythonIsSpace) == "" || strings.HasPrefix(arg, "-") {
			t.Skip()
		}
		line := LaunchLine{Options: launchOptions{{Name: name, Value: value}}, Args: []string{arg}}
		parsed, err := parseZkaLaunchLine(line.SessionLine())
		if err != nil {
			t.Fatalf("parse %q: %v", line.SessionLine(), err)
		}
		option, ok := parsed.Options.Get(name)
		if !ok || option.Value != value {
			t.Fatalf("option round trip: sent %q=%q, got %#v", name, value, parsed.Options)
		}
		if len(parsed.Args) != 1 || parsed.Args[0] != arg {
			t.Fatalf("argument round trip: sent %q, got %#v", arg, parsed.Args)
		}
	})
}

func FuzzCanonicalValueIsIdempotent(f *testing.F) {
	for _, seed := range []string{"", " x ", "a\u2028b", "a\x1cb", "\v\f", "plain"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		if !utf8.ValidString(value) {
			t.Skip()
		}
		once := canonicalStrippedValue(value)
		if twice := canonicalStrippedValue(once); twice != once {
			t.Fatalf("canonicalStrippedValue not idempotent: %q -> %q -> %q", value, once, twice)
		}
		if strings.IndexFunc(once, pythonIsLineBoundary) >= 0 {
			t.Fatalf("canonical value %q still contains a Kitty line boundary", once)
		}
	})
}

// A tab name must survive being written to a session file and parsed back by
// Kitty. This is the property whose absence caused the outage.
func FuzzTabNameSurvivesSessionRoundTrip(f *testing.F) {
	for _, seed := range []string{
		"Recovered 98b08d66", "plain", `has "quotes"`, "it's", "$HOME", "#hash",
		`back\slash`, "-leading", "  padded  ", "uni✓",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, title string) {
		if !utf8.ValidString(title) {
			t.Skip()
		}
		canonical := canonicalStrippedValue(title)
		var out sessionWriter
		out.NewTab(canonical)
		lines := parseSessionLines(out.String())
		if canonical == "" {
			if len(lines) != 1 || lines[0].Rest != "" {
				t.Fatalf("empty title produced %#v", lines)
			}
			return
		}
		if len(lines) != 1 || lines[0].Directive != "new_tab" {
			t.Fatalf("title %q produced %#v", canonical, lines)
		}
		if got := lines[0].verbatimValue(); got != canonical {
			t.Fatalf("tab name %q came back as %q", canonical, got)
		}
	})
}

func TestAnsiCEscapeSurvivesKittenExpansion(t *testing.T) {
	// kitten expands ANSI-C escapes in set-tab-title / set-window-title
	// arguments, so any backslash must be doubled on the way out.
	for _, value := range []string{`C:\Users\name`, `a\nb`, `\x41`, `\\`, "plain"} {
		escaped := ansiCEscape(value)
		if got := expandAnsiCModel(escaped); got != value {
			t.Fatalf("ansiCEscape(%q) = %q expanded to %q", value, escaped, got)
		}
	}
}

// expandAnsiCModel is a minimal model of kitten's escape expansion, enough to
// prove backslash doubling is the right inverse.
func expandAnsiCModel(value string) string {
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' || i+1 >= len(value) {
			out.WriteByte(value[i])
			continue
		}
		i++
		switch value[i] {
		case 'n':
			out.WriteByte('\n')
		case 't':
			out.WriteByte('\t')
		case '\\':
			out.WriteByte('\\')
		default:
			// Unknown escapes consume the backslash, which is exactly how an
			// unescaped Windows path loses characters.
			out.WriteByte(value[i])
		}
	}
	return out.String()
}
