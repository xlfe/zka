package zka

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Kitty's session parser (kitty/session.py parse_session) splits a line once on
// whitespace and hands the raw remainder to the directive. Only `launch` is
// shlex-split, and only `launch` and `set_layout_state` skip expandvars.
//
// sessionWriter encodes that contract with one method per directive and no
// generic token writer, so a caller cannot reach for the wrong escaping. The
// bug this replaces was a caller choosing shell quoting for `new_tab`, whose
// value Kitty takes verbatim -- the quote characters became part of the tab
// name and the topology could never converge again.
type sessionWriter struct {
	buf bytes.Buffer
}

func (w *sessionWriter) String() string { return w.buf.String() }

// writeVerbatim emits a directive whose value Kitty consumes as the raw rest of
// the line and then expands. Canonicalising here is what keeps the desired
// state closed under Kitty's round trip.
func (w *sessionWriter) writeVerbatim(directive, value string) {
	value = canonicalStrippedValue(value)
	if value == "" {
		w.writeBare(directive)
		return
	}
	w.buf.WriteString(directive)
	w.buf.WriteByte(' ')
	w.buf.WriteString(escapeExpandVars(value))
	w.buf.WriteByte('\n')
}

func (w *sessionWriter) writeBare(directive string) {
	w.buf.WriteString(directive)
	w.buf.WriteByte('\n')
}

func (w *sessionWriter) NewOSWindow()           { w.writeBare("new_os_window") }
func (w *sessionWriter) NewTab(title string)    { w.writeVerbatim("new_tab", title) }
func (w *sessionWriter) Title(title string)     { w.writeVerbatim("title", title) }
func (w *sessionWriter) CD(dir string)          { w.writeVerbatim("cd", dir) }
func (w *sessionWriter) OSWindowState(s string) { w.writeVerbatim("os_window_state", s) }
func (w *sessionWriter) OSWindowClass(s string) { w.writeVerbatim("os_window_class", s) }
func (w *sessionWriter) OSWindowName(s string)  { w.writeVerbatim("os_window_name", s) }
func (w *sessionWriter) OSWindowTitle(s string) { w.writeVerbatim("os_window_title", s) }
func (w *sessionWriter) FocusMatching(s string) { w.writeVerbatim("focus_matching_window", s) }
func (w *sessionWriter) Focus()                 { w.writeBare("focus") }
func (w *sessionWriter) FocusTab(index int)     { w.writeBare("focus_tab " + strconv.Itoa(index)) }
func (w *sessionWriter) ResizeWindow(a []string) {
	w.writeVerbatim("resize_window", strings.Join(a, " "))
}
func (w *sessionWriter) OSWindowSize(x, y string) { w.writeVerbatim("os_window_size", x+" "+y) }

// FocusOSWindow takes no argument by construction: session.py:274 discards
// whatever follows the directive and merely sets a boolean, so an index here
// would silently do nothing.
func (w *sessionWriter) FocusOSWindow() { w.writeBare("focus_os_window") }

// Layout must not be quoted: set_layout raises ValueError on an unknown name,
// which aborts the entire session load rather than just the tab.
func (w *sessionWriter) Layout(name string) {
	if name = canonicalStrippedValue(name); name != "" {
		w.writeVerbatim("layout", name)
	}
}

// EnabledLayouts is comma-joined and each entry is stripped by Kitty. An
// unknown name raises, so entries are canonicalised individually.
func (w *sessionWriter) EnabledLayouts(names []string) {
	clean := make([]string, 0, len(names))
	for _, name := range names {
		if name = canonicalStrippedValue(name); name != "" {
			clean = append(clean, name)
		}
	}
	if len(clean) != 0 {
		w.writeVerbatim("enabled_layouts", strings.Join(clean, ","))
	}
}

// LayoutState is the one verbatim directive Kitty does not expand
// (session.py:258 excludes it), so it must not be "$"-escaped.
func (w *sessionWriter) LayoutState(state json.RawMessage) {
	if len(state) == 0 {
		return
	}
	w.buf.WriteString("set_layout_state ")
	w.buf.WriteString(canonicalLineValue(string(state)))
	w.buf.WriteByte('\n')
}

func (w *sessionWriter) Launch(line LaunchLine) {
	w.buf.WriteString(line.SessionLine())
	w.buf.WriteByte('\n')
}

// Verbatim re-emits an already-encoded line, used when passing a user template
// or a Kitty-authored directive through untouched.
func (w *sessionWriter) Verbatim(line sessionLine) {
	if line.Directive == "" {
		return
	}
	if line.Rest == "" {
		w.writeBare(line.Directive)
		return
	}
	w.buf.WriteString(line.Directive)
	w.buf.WriteByte(' ')
	w.buf.WriteString(canonicalLineValue(line.Rest))
	w.buf.WriteByte('\n')
}

// sessionLine is one parsed session directive. Rest is exactly the rest-of-line
// Kitty sees, still encoded; it is never tokenized here because Kitty does not
// tokenize it either.
type sessionLine struct {
	Directive string
	Rest      string
}

// verbatimValue decodes Rest for a directive Kitty expands. Use it only on text
// zka rendered; Kitty writes its own session output unescaped.
func (l sessionLine) verbatimValue() string { return unescapeExpandVars(l.Rest) }

// parseSessionLines mirrors Kitty exactly: splitlines, strip, drop blanks and
// comments, split once on whitespace, strip both halves. Applying a shell
// tokenizer here is what used to make a tab title containing an apostrophe --
// which Kitty writes unquoted -- fail capture permanently.
func parseSessionLines(content string) []sessionLine {
	var lines []sessionLine
	for _, raw := range splitPythonLines(content) {
		trimmed := strings.TrimFunc(raw, pythonIsSpace)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		directive, rest := trimmed, ""
		if at := strings.IndexFunc(trimmed, pythonIsSpace); at >= 0 {
			directive = trimmed[:at]
			rest = strings.TrimFunc(trimmed[at:], pythonIsSpace)
		}
		lines = append(lines, sessionLine{Directive: directive, Rest: rest})
	}
	return lines
}

func splitPythonLines(content string) []string {
	var lines []string
	start := 0
	runes := []rune(content)
	for i := 0; i < len(runes); i++ {
		if !pythonIsLineBoundary(runes[i]) {
			continue
		}
		lines = append(lines, string(runes[start:i]))
		if runes[i] == '\r' && i+1 < len(runes) && runes[i+1] == '\n' {
			i++
		}
		start = i + 1
	}
	if start < len(runes) {
		lines = append(lines, string(runes[start:]))
	}
	return lines
}

// LaunchOption is a single kitty launch option. Value is always the decoded
// string -- exactly what Kitty ends up with -- so encoding happens only when
// rendering and decoding only when parsing. That is what makes the round trip
// idempotent instead of doubling every "$" on each capture.
type LaunchOption struct {
	Name  string `json:"name"`
	Value string `json:"value,omitempty"`
	Flag  bool   `json:"flag,omitempty"`
}

// token renders the option in inline form. Inline is required, not cosmetic:
// Kitty stops expanding options at the first token that equals the program name
// (session.py:140), so with separated form a pane titled "zka" would silently
// truncate the expansion window.
func (o LaunchOption) token() string {
	if o.Flag {
		return o.Name
	}
	// Only the value is canonicalised. Folding the name into the same pass
	// could rewrite the option's identity rather than just its payload.
	return o.Name + "=" + canonicalLineValue(o.Value)
}

type launchOptions []LaunchOption

func (o launchOptions) Get(name string) (LaunchOption, bool) {
	for _, option := range o {
		if option.Name == name {
			return option, true
		}
	}
	return LaunchOption{}, false
}

func (o launchOptions) Has(name string) bool {
	_, ok := o.Get(name)
	return ok
}

func (o launchOptions) Drop(names ...string) launchOptions {
	unwanted := make(map[string]bool, len(names))
	for _, name := range names {
		unwanted[name] = true
	}
	clean := make(launchOptions, 0, len(o))
	for _, option := range o {
		if !unwanted[option.Name] {
			clean = append(clean, option)
		}
	}
	return clean
}

// VarValue returns the value of a "--var key=value" option.
func (o launchOptions) VarValue(name, key string) string {
	for _, option := range o {
		if option.Name != name || option.Flag {
			continue
		}
		if at := strings.IndexByte(option.Value, '='); at >= 0 && option.Value[:at] == key {
			return option.Value[at+1:]
		}
	}
	return ""
}

func (o launchOptions) clone() launchOptions {
	if o == nil {
		return nil
	}
	return append(launchOptions(nil), o...)
}

// UnmarshalJSON accepts both the typed form and the pre-v6 flat token list.
// Legacy tokens were stored still carrying the "$" doubling that rendering
// applied and parsing never undid, so they are decoded exactly once here --
// which is also what stops that doubling from compounding further.
func (o *launchOptions) UnmarshalJSON(data []byte) error {
	var typed []LaunchOption
	if err := json.Unmarshal(data, &typed); err == nil {
		*o = typed
		return nil
	}
	var tokens []string
	if err := json.Unmarshal(data, &tokens); err != nil {
		return err
	}
	decoded := make([]string, 0, len(tokens))
	for _, token := range tokens {
		decoded = append(decoded, unescapeExpandVars(token))
	}
	options, _, err := splitLaunchTokens(decoded)
	if err != nil {
		return err
	}
	*o = options
	return nil
}

// LaunchLine is a whole `launch` directive. Rendering it is the only way zka
// produces one, for a session file and for `kitten @ launch` alike.
type LaunchLine struct {
	SerializedWindowID int64
	Options            launchOptions
	Args               []string
}

// SessionLine encodes for a session file. Options are expanded by Kitty so they
// are "$"-escaped; program arguments are not expanded (sessions.rst.txt:207)
// so they must not be; and kitty-unserialize-data is removed before the
// expansion pass (session.py:130) so it is not escaped either.
func (l LaunchLine) SessionLine() string {
	tokens := []string{"launch"}
	if l.SerializedWindowID > 0 {
		tokens = append(tokens, shlexQuote(serializedWindowToken(l.SerializedWindowID)))
	}
	for _, option := range l.Options {
		tokens = append(tokens, shlexQuote(escapeExpandVars(option.token())))
	}
	if len(l.Args) != 0 {
		tokens = append(tokens, "--")
		for _, arg := range l.Args {
			tokens = append(tokens, shlexQuote(canonicalLineValue(arg)))
		}
	}
	return strings.Join(tokens, " ")
}

// RCArgs renders argv for `kitten @ launch`. Nothing is escaped: these go
// straight to exec, which passes bytes.
func (l LaunchLine) RCArgs(launchType string, nextTo int64, dontTakeFocus bool) []string {
	if launchType == "" {
		launchType = "window"
	}
	args := []string{"launch", "--type=" + launchType}
	if dontTakeFocus {
		args = append(args, "--dont-take-focus")
	}
	for _, option := range l.Options {
		args = append(args, option.token())
	}
	if nextTo > 0 {
		args = append(args, "--next-to=id:"+strconv.FormatInt(nextTo, 10))
	}
	if len(l.Args) != 0 {
		args = append(args, "--")
		args = append(args, l.Args...)
	}
	return args
}

func serializedWindowToken(id int64) string {
	serialized, _ := json.Marshal(map[string]int64{"id": id})
	return "kitty-unserialize-data=" + string(serialized)
}

// parseZkaLaunchLine parses a launch line zka rendered, undoing the "$"
// escaping applied to options.
func parseZkaLaunchLine(rest string) (LaunchLine, error) { return parseLaunchLine(rest, true) }

// parseKittyLaunchLine parses a launch line Kitty wrote. Kitty does not escape
// its own output, so nothing is undone.
func parseKittyLaunchLine(rest string) (LaunchLine, error) { return parseLaunchLine(rest, false) }

func parseLaunchLine(rest string, zkaEncoded bool) (LaunchLine, error) {
	tokens, err := shlexSplit(rest)
	if err != nil {
		return LaunchLine{}, err
	}
	if len(tokens) != 0 && tokens[0] == "launch" {
		tokens = tokens[1:]
	}
	var line LaunchLine
	if len(tokens) != 0 && strings.HasPrefix(tokens[0], "kitty-unserialize-data=") {
		var payload struct {
			ID int64 `json:"id"`
		}
		_ = json.Unmarshal([]byte(strings.TrimPrefix(tokens[0], "kitty-unserialize-data=")), &payload)
		line.SerializedWindowID = payload.ID
		tokens = tokens[1:]
	}
	options, args, err := splitLaunchTokens(tokens)
	if err != nil {
		return LaunchLine{}, err
	}
	if zkaEncoded {
		// Only options were "$"-escaped on the way out, because Kitty expands
		// options and never program arguments. Decoding args too would eat a
		// literal "$$" that the user typed.
		for index := range options {
			options[index].Name = unescapeExpandVars(options[index].Name)
			options[index].Value = unescapeExpandVars(options[index].Value)
		}
	}
	line.Options, line.Args = options, args
	return line, nil
}

// splitLaunchTokens is total and forward compatible. An unknown inline
// --name=value is always parseable; an unknown bare --name consumes the next
// token only when that token cannot itself be an option. Failing capture
// because Kitty gained a launch option zka has never heard of is not an
// acceptable outcome.
func splitLaunchTokens(tokens []string) (launchOptions, []string, error) {
	var options launchOptions
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		if token == "--" {
			return options, append([]string(nil), tokens[i+1:]...), nil
		}
		if !strings.HasPrefix(token, "-") {
			return options, append([]string(nil), tokens[i:]...), nil
		}
		name, value, inline := optionParts(token)
		switch {
		case inline:
			options = append(options, LaunchOption{Name: name, Value: value})
		case launchFlagOptions[name]:
			options = append(options, LaunchOption{Name: name, Flag: true})
		case launchValueOptions[name]:
			if i+1 >= len(tokens) {
				return nil, nil, fmt.Errorf("launch option %q requires a value", name)
			}
			options = append(options, LaunchOption{Name: name, Value: tokens[i+1]})
			i++
		case i+1 < len(tokens) && tokens[i+1] != "--" && !strings.HasPrefix(tokens[i+1], "-"):
			options = append(options, LaunchOption{Name: name, Value: tokens[i+1]})
			i++
		default:
			options = append(options, LaunchOption{Name: name, Flag: true})
		}
	}
	return options, nil, nil
}
