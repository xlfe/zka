package zka

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"
)

type SessionTemplate struct {
	Lines    []templateLine
	Launches int
}

type templateLine struct {
	Directive string
	Tokens    []string
	Launch    bool
	Raw       string
}

var safeSessionDirectives = map[string]bool{
	"new_os_window": true, "os_window_state": true, "os_window_class": true,
	"os_window_name": true, "os_window_title": true, "os_window_size": true,
	"new_tab": true, "enabled_layouts": true,
	"layout": true, "set_layout_state": true, "focus": true,
	"focus_tab": true, "focus_os_window": true, "resize_window": true,
	"focus_matching_window": true, "cd": true, "title": true,
}

var launchValueOptions = map[string]bool{
	"--source-window": true, "--window-title": true, "--title": true,
	"--tab-title": true, "--cwd": true, "--add-to-session": true,
	"--location": true, "--next-to": true, "--bias": true, "--var": true,
	"--env": true, "--type": true, "--stdin-source": true,
	"--spacing": true, "--logo": true, "--logo-position": true,
	"--logo-alpha": true, "--color": true, "--watcher": true,
	"-w": true, "--marker": true, "--remote-control-password": true,
	"--os-window-class": true, "--os-window-name": true,
	"--os-window-title": true, "--os-window-state": true,
	"--os-window-position": true, "--os-panel": true,
}

var launchFlagOptions = map[string]bool{
	"--hold": true, "--keep-focus": true, "--dont-take-focus": true,
	"--copy-colors": true, "--copy-env": true, "--copy-cmdline": true,
	"--allow-remote-control": true, "--stdin-add-formatting": true,
	"--stdin-add-line-wrap-markers": true, "--hold-after-ssh": true,
}

var safeTopologyValueOptions = map[string]bool{
	"--window-title": true, "--title": true, "--tab-title": true,
	"--cwd": true, "--location": true, "--next-to": true, "--bias": true,
	"--var": true, "--os-window-class": true, "--os-window-name": true,
	"--os-window-title": true, "--os-window-state": true,
	"--os-window-position": true,
}

var safeTopologyFlagOptions = map[string]bool{
	"--keep-focus": true, "--dont-take-focus": true,
}

func DefaultSessionTemplate() SessionTemplate {
	return SessionTemplate{Lines: []templateLine{{Directive: "launch", Tokens: []string{"launch"}, Launch: true}}, Launches: 1}
}

func ParseSessionTemplate(content string) (SessionTemplate, error) {
	var template SessionTemplate
	for lineNumber, raw := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		tokens, err := splitSessionWords(trimmed)
		if err != nil {
			return SessionTemplate{}, fmt.Errorf("template line %d: %w", lineNumber+1, err)
		}
		if len(tokens) == 0 {
			continue
		}
		directive := tokens[0]
		line := templateLine{Directive: directive, Tokens: tokens, Raw: trimmed}
		if directive == "launch" {
			options, command, err := parseLaunch(tokens[1:])
			if err != nil {
				return SessionTemplate{}, fmt.Errorf("template line %d: %w", lineNumber+1, err)
			}
			if len(command) != 0 {
				return SessionTemplate{}, fmt.Errorf("template line %d: launch must not contain a program", lineNumber+1)
			}
			if err := validateTemplateOptions(options); err != nil {
				return SessionTemplate{}, fmt.Errorf("template line %d: %w", lineNumber+1, err)
			}
			line.Launch = true
			template.Launches++
		} else if !safeSessionDirectives[directive] {
			return SessionTemplate{}, fmt.Errorf("template line %d: directive %q is not topology-safe", lineNumber+1, directive)
		}
		template.Lines = append(template.Lines, line)
	}
	if template.Launches == 0 {
		return SessionTemplate{}, fmt.Errorf("template must contain at least one bare launch")
	}
	return template, nil
}

func validateTemplateOptions(options []string) error {
	for i := 0; i < len(options); i++ {
		name, value, hasValue := optionParts(options[i])
		if !hasValue && launchValueOptions[name] && i+1 < len(options) {
			value, hasValue = options[i+1], true
			i++
		}
		if !safeTopologyValueOptions[name] && !safeTopologyFlagOptions[name] {
			return fmt.Errorf("launch option %q is not topology-safe", name)
		}
		if (name == "--var" || name == "--env") && hasValue {
			key := value
			if at := strings.IndexByte(key, '='); at >= 0 {
				key = key[:at]
			}
			if isManagedPaneVariable(key) {
				return fmt.Errorf("reserved variable %q is managed by zka", key)
			}
		}
	}
	return nil
}

func optionParts(token string) (name, value string, hasValue bool) {
	if at := strings.IndexByte(token, '='); at >= 0 {
		return token[:at], token[at+1:], true
	}
	return token, "", false
}

func parseLaunch(tokens []string) (options, command []string, err error) {
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		if token == "--" {
			return options, append([]string(nil), tokens[i+1:]...), nil
		}
		if !strings.HasPrefix(token, "-") {
			return options, append([]string(nil), tokens[i:]...), nil
		}
		name, _, inline := optionParts(token)
		if inline && !launchValueOptions[name] && !launchFlagOptions[name] {
			return nil, nil, fmt.Errorf("unsupported launch option %q", name)
		}
		if launchFlagOptions[name] || inline {
			options = append(options, token)
			continue
		}
		if !launchValueOptions[name] {
			return nil, nil, fmt.Errorf("unsupported launch option %q", name)
		}
		if i+1 >= len(tokens) {
			return nil, nil, fmt.Errorf("launch option %q requires a value", name)
		}
		options = append(options, token, tokens[i+1])
		i++
	}
	return options, nil, nil
}

func GenerateManagedSession(template SessionTemplate, workspace *Workspace) (string, error) {
	panes := workspace.SortedPanes()
	if len(panes) != template.Launches {
		return "", fmt.Errorf("template has %d launches but workspace has %d panes", template.Launches, len(panes))
	}
	var out bytes.Buffer
	paneIndex := 0
	for _, line := range template.Lines {
		if !line.Launch {
			writeRawDirective(&out, line.Raw)
			continue
		}
		options, _, err := parseLaunch(line.Tokens[1:])
		if err != nil {
			return "", err
		}
		writeCanonicalLaunch(&out, options, workspace, panes[paneIndex], Transport{Kind: "local"}, "")
		paneIndex++
	}
	return out.String(), nil
}

func writeCanonicalLaunch(out *bytes.Buffer, options []string, workspace *Workspace, pane *Pane, transport Transport, attachmentID string) {
	clean := stripManagedOptions(options)
	if transport.Kind == "ssh" {
		clean = dropLaunchOption(clean, "--cwd")
	}
	tokens := []string{"launch"}
	tokens = append(tokens, clean...)
	if transport.Kind != "ssh" && !hasLaunchOption(clean, "--cwd") && pane.CWD != "" {
		tokens = append(tokens, "--cwd", pane.CWD)
	}
	if !hasLaunchOption(clean, "--title") && !hasLaunchOption(clean, "--window-title") && pane.Title != "" {
		tokens = append(tokens, "--title", pane.Title)
	}
	tokens = append(tokens,
		"--var", "zka_workspace="+workspace.ID,
		"--var", "zka_pane="+pane.ID,
		"--var", "zka_state="+string(pane.State),
		"--var", "zka_ready=0",
		"--env", "ZKA_WORKSPACE_ID="+workspace.ID,
		"--env", "ZKA_PANE_ID="+pane.ID,
		"--",
	)
	if transport.Kind == "ssh" {
		tokens = append(tokens, "zka", "remote-pane", "--origin", transport.Host,
			"--workspace", workspace.ID, "--pane", pane.ID, "--attachment", attachmentID)
	} else {
		tokens = append(tokens, "zka", "pane", "--workspace", workspace.ID, "--pane", pane.ID)
	}
	writeSessionTokens(out, tokens)
}

func dropLaunchOption(options []string, unwanted string) []string {
	clean := make([]string, 0, len(options))
	for i := 0; i < len(options); i++ {
		name, _, inline := optionParts(options[i])
		consumed := 1
		if !inline && launchValueOptions[name] && i+1 < len(options) {
			consumed = 2
		}
		if name != unwanted {
			clean = append(clean, options[i:i+consumed]...)
		}
		i += consumed - 1
	}
	return clean
}

func stripManagedOptions(options []string) []string {
	var clean []string
	for i := 0; i < len(options); i++ {
		token := options[i]
		name, value, inline := optionParts(token)
		consumed := 1
		if !inline && launchValueOptions[name] && i+1 < len(options) {
			value = options[i+1]
			consumed = 2
		}
		managed := !safeTopologyValueOptions[name] && !safeTopologyFlagOptions[name]
		if name == "--var" || name == "--env" {
			key := value
			if at := strings.IndexByte(key, '='); at >= 0 {
				key = key[:at]
			}
			managed = managed || isManagedPaneVariable(key)
		}
		if !managed {
			if name == "--title" || name == "--window-title" {
				value = stripStateMarker(value)
				if inline {
					clean = append(clean, name+"="+value)
				} else {
					clean = append(clean, name, value)
				}
			} else {
				clean = append(clean, options[i:i+consumed]...)
			}
		}
		i += consumed - 1
	}
	return clean
}

func isManagedPaneVariable(key string) bool {
	return key == "zka_workspace" || key == "zka_pane" || key == "zka_state" || key == "zka_ready" || strings.HasPrefix(key, "ZKA_")
}

func hasLaunchOption(options []string, wanted string) bool {
	for _, token := range options {
		name, _, _ := optionParts(token)
		if name == wanted {
			return true
		}
	}
	return false
}

func CanonicalizeKittySession(content string, workspace *Workspace) (string, error) {
	var out bytes.Buffer
	seen := map[string]bool{}
	for lineNumber, raw := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		tokens, err := splitSessionWords(trimmed)
		if err != nil {
			return "", fmt.Errorf("kitty session line %d: %w", lineNumber+1, err)
		}
		if len(tokens) == 0 {
			continue
		}
		if tokens[0] != "launch" {
			if !safeSessionDirectives[tokens[0]] {
				return "", fmt.Errorf("kitty session line %d: unsafe directive %q", lineNumber+1, tokens[0])
			}
			if workspace.RemoteHost != "" && tokens[0] == "cd" {
				continue
			}
			writeRawDirective(&out, trimmed)
			continue
		}
		options, _, err := parseLaunch(tokens[1:])
		if err != nil {
			return "", fmt.Errorf("kitty session line %d: %w", lineNumber+1, err)
		}
		workspaceID := launchOptionValue(options, "--var", "zka_workspace")
		paneID := launchOptionValue(options, "--var", "zka_pane")
		if workspaceID != workspace.ID {
			return "", fmt.Errorf("kitty session line %d: launch is not tagged for workspace %s", lineNumber+1, workspace.ID)
		}
		pane := workspace.Panes[paneID]
		if pane == nil {
			return "", fmt.Errorf("kitty session line %d: unknown pane %q", lineNumber+1, paneID)
		}
		if seen[paneID] {
			return "", fmt.Errorf("kitty session line %d: pane %s is duplicated", lineNumber+1, paneID)
		}
		seen[paneID] = true
		if workspace.RemoteHost != "" {
			options = dropLaunchOption(options, "--cwd")
		}
		writeCanonicalLaunch(&out, options, workspace, pane, Transport{Kind: "local"}, "")
	}
	if len(seen) == 0 {
		return "", fmt.Errorf("kitty session contains no managed panes")
	}
	return out.String(), nil
}

func launchOptionValue(options []string, option, key string) string {
	for i := 0; i < len(options); i++ {
		name, value, inline := optionParts(options[i])
		if !inline && launchValueOptions[name] && i+1 < len(options) {
			value = options[i+1]
			i++
		}
		if name != option {
			continue
		}
		parts := strings.SplitN(value, "=", 2)
		if len(parts) == 2 && parts[0] == key {
			return parts[1]
		}
	}
	return ""
}

func RenderAttachmentSession(workspace *Workspace, transport Transport, attachmentID string) (string, error) {
	if strings.TrimSpace(workspace.Manifest.Session) == "" {
		return "", fmt.Errorf("workspace %s has no captured manifest", workspace.Name)
	}
	var out bytes.Buffer
	seen := map[string]bool{}
	for _, raw := range strings.Split(workspace.Manifest.Session, "\n") {
		trimmed := strings.TrimSpace(raw)
		tokens, err := splitSessionWords(trimmed)
		if err != nil {
			return "", err
		}
		if len(tokens) == 0 {
			continue
		}
		if tokens[0] != "launch" {
			if transport.Kind == "ssh" && tokens[0] == "cd" {
				continue
			}
			writeRawDirective(&out, trimmed)
			continue
		}
		options, _, err := parseLaunch(tokens[1:])
		if err != nil {
			return "", err
		}
		paneID := launchOptionValue(options, "--var", "zka_pane")
		pane := workspace.Panes[paneID]
		if pane == nil {
			return "", fmt.Errorf("manifest references unknown pane %s", paneID)
		}
		if seen[paneID] {
			return "", fmt.Errorf("manifest duplicates pane %s", paneID)
		}
		seen[paneID] = true
		writeCanonicalLaunch(&out, options, workspace, pane, transport, attachmentID)
	}
	return out.String(), nil
}

func CaptureManifest(ctx context.Context, kitty KittyClient, endpoint string, workspace *Workspace) (Manifest, map[string]RuntimeView, error) {
	tree, err := kitty.List(ctx, endpoint)
	if err != nil {
		return Manifest{}, nil, err
	}
	views, untagged := findWorkspaceViews(tree, workspace.ID)
	if len(untagged) > 0 {
		return Manifest{}, nil, fmt.Errorf("kitty has untagged windows: %v", untagged)
	}
	topology, err := topologyFromKitty(tree, workspace.ID)
	if err != nil {
		return Manifest{}, nil, err
	}
	native, err := kitty.NativeSession(ctx, endpoint, workspace.ID)
	if err != nil {
		return Manifest{}, nil, fmt.Errorf("capture kitty session: %w", err)
	}
	canonical, err := CanonicalizeKittySession(native, workspace)
	if err != nil {
		return Manifest{}, nil, err
	}
	manifest := Manifest{KittyVersion: kitty.Version(ctx), CapturedAt: timeNowUTC(), Session: canonical, Topology: topology}
	if err := validateManifest(workspace, manifest); err != nil {
		return Manifest{}, nil, err
	}
	return manifest, views, nil
}

var timeNowUTC = func() time.Time { return time.Now().UTC() }

func writeSessionTokens(out *bytes.Buffer, tokens []string) {
	for i, token := range tokens {
		if i > 0 {
			out.WriteByte(' ')
		}
		out.WriteString(quoteSessionToken(token))
	}
	out.WriteByte('\n')
}

func writeRawDirective(out *bytes.Buffer, line string) {
	line = strings.NewReplacer("\r", " ", "\n", " ").Replace(strings.TrimSpace(line))
	if line == "" {
		return
	}
	parts := strings.SplitN(line, " ", 2)
	if len(parts) == 2 && (parts[0] == "new_tab" || parts[0] == "title") {
		line = parts[0] + " " + stripStateMarker(strings.TrimSpace(parts[1]))
	}
	out.WriteString(line)
	out.WriteByte('\n')
}

func quoteSessionToken(token string) string {
	if token != "" && strings.IndexFunc(token, func(r rune) bool {
		return unicode.IsSpace(r) || r == '\'' || r == '"' || r == '\\' || r == '$' || r == '#' || unicode.IsControl(r)
	}) == -1 {
		return token
	}
	return quoteKitty(token)
}

func splitSessionWords(input string) ([]string, error) {
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
		if unicode.IsSpace(r) {
			flush()
			continue
		}
		word.WriteRune(r)
		inWord = true
	}
	if escaped {
		return nil, fmt.Errorf("trailing escape")
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote")
	}
	flush()
	return result, nil
}
