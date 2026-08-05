package zka

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	assuanLineMax          = 16 << 10
	assuanOperationMax     = 1 << 20
	credentialNoticeWindow = 2 * time.Second
)

const (
	assuanForbidden   = "ERR 67108963 Forbidden <GPG Agent>"
	assuanTimeout     = "ERR 67108926 Timeout <GPG Agent>"
	assuanNoPinentry  = "ERR 67108949 No Pinentry <GPG Agent>"
	assuanNoDevice    = "ERR 100696144 No such device <SCD>"
	assuanNoSecretKey = "ERR 67108881 No secret key <GPG Agent>"
)

type openPGPFilter struct {
	daemon           *Daemon
	host             string
	hello            credentialStreamHello
	downstream       net.Conn
	upstream         net.Conn
	reader           *bufio.Reader
	downstreamReader *bufio.Reader
	allowed          map[string]string
	extraSocket      string

	selectedGrip string
	selectedFor  string
	hashLine     string
	description  string
	options      []string
	optionBytes  int
	interactive  func() bool
	dial         func() (net.Conn, string, error)
}

type credentialNoticeState struct {
	At    time.Time
	Count int
}

func (d *Daemon) filterOpenPGPStream(ctx context.Context, host string, hello credentialStreamHello, manifest *credentialOpenPGPManifest, downstream net.Conn) error {
	if manifest == nil || len(manifest.AllowedKeygrips) == 0 {
		return fmt.Errorf("OpenPGP provider manifest is unavailable")
	}
	extraSocket, _, err := d.runner.Run(ctx, d.config.Credentials.GnuPG.GPGConfCommand, "--list-dirs", "agent-extra-socket")
	if err != nil {
		return err
	}
	filter := &openPGPFilter{
		daemon: d, host: host, hello: hello, downstream: downstream,
		allowed: manifest.AllowedKeygrips, extraSocket: strings.TrimSpace(extraSocket), interactive: interactiveCredentialSessionAvailable,
	}
	if d.credentialInteractive != nil {
		filter.interactive = d.credentialInteractive
	}
	upstream, greeting, err := filter.dialUpstream()
	if err != nil {
		return err
	}
	filter.upstream = upstream
	filter.reader = bufio.NewReaderSize(upstream, assuanLineMax)
	defer func() {
		if filter.upstream != nil {
			_ = filter.upstream.Close()
		}
	}()
	if err := writeAssuanLine(downstream, greeting); err != nil {
		return err
	}
	return filter.serve(ctx)
}

func (f *openPGPFilter) dialUpstream() (net.Conn, string, error) {
	if f.dial != nil {
		return f.dial()
	}
	if f.extraSocket == "" {
		return nil, "", fmt.Errorf("restricted GnuPG agent extra socket is unavailable")
	}
	conn, err := net.DialTimeout("unix", f.extraSocket, 500*time.Millisecond)
	if err != nil {
		return nil, "", err
	}
	greeting, err := readAssuanLine(bufio.NewReaderSize(conn, assuanLineMax))
	if err != nil {
		_ = conn.Close()
		return nil, "", err
	}
	if !strings.HasPrefix(greeting, "OK") {
		_ = conn.Close()
		return nil, "", fmt.Errorf("GnuPG agent rejected restricted connection: %s", greeting)
	}
	return conn, greeting, nil
}

func (f *openPGPFilter) serve(ctx context.Context) error {
	f.downstreamReader = bufio.NewReaderSize(f.downstream, assuanLineMax)
	for {
		line, err := readAssuanLine(f.downstreamReader)
		if err != nil {
			return err
		}
		command, argument := splitAssuanCommand(line)
		switch command {
		case "RESET":
			f.clearPrivateKeyState()
			if err := f.forwardSimple(line); err != nil {
				return err
			}
		case "NOP", "END":
			if err := f.forwardSimple(line); err != nil {
				return err
			}
		case "BYE":
			_ = f.forwardSimple(line)
			return nil
		case "GETINFO":
			if argument != "version" {
				if err := writeAssuanLine(f.downstream, assuanForbidden); err != nil {
					return err
				}
				continue
			}
			if err := f.forwardSimple(line); err != nil {
				return err
			}
		case "OPTION":
			if err := f.handleOption(argument); err != nil {
				return err
			}
		case "SCD":
			if argument == "SERIALNO" {
				if err := writeAssuanLine(f.downstream, assuanNoDevice); err != nil {
					return err
				}
				continue
			}
			if err := writeAssuanLine(f.downstream, assuanForbidden); err != nil {
				return err
			}
		case "HAVEKEY":
			if err := f.handleHaveKey(argument); err != nil {
				return err
			}
		case "KEYINFO":
			if err := f.handleKeyInfo(argument); err != nil {
				return err
			}
		case "SIGKEY", "SETKEY":
			grip := strings.ToUpper(strings.TrimSpace(argument))
			if _, ok := f.allowed[grip]; !ok || len(grip) != 40 {
				f.clearPrivateKeyState()
				if err := writeAssuanLine(f.downstream, assuanForbidden); err != nil {
					return err
				}
				continue
			}
			f.selectedGrip = grip
			f.selectedFor = map[string]string{"SIGKEY": "sign", "SETKEY": "decrypt"}[command]
			f.hashLine, f.description = "", ""
			if err := f.forwardSimple(command + " " + grip); err != nil {
				return err
			}
		case "SETKEYDESC":
			if f.selectedGrip == "" {
				if err := writeAssuanLine(f.downstream, assuanForbidden); err != nil {
					return err
				}
				continue
			}
			f.description = "Remote OpenPGP request from " + f.daemon.credentialNoticeContext(f.host, f.hello, f.allowed[f.selectedGrip], f.selectedFor)
			if err := f.forwardSimple("SETKEYDESC " + assuanEscapeDescription(f.description)); err != nil {
				return err
			}
		case "SETHASH":
			if f.selectedGrip == "" || f.selectedFor != "sign" || !validSetHash(argument) {
				if err := writeAssuanLine(f.downstream, assuanForbidden); err != nil {
					return err
				}
				continue
			}
			f.hashLine = "SETHASH " + argument
			if err := f.forwardSimple(f.hashLine); err != nil {
				return err
			}
		case "PKSIGN":
			if f.selectedGrip == "" || f.selectedFor != "sign" || f.hashLine == "" {
				if err := writeAssuanLine(f.downstream, assuanForbidden); err != nil {
					return err
				}
				continue
			}
			if err := f.sign(ctx, line); err != nil {
				return err
			}
		case "PKDECRYPT":
			if f.selectedGrip == "" || f.selectedFor != "decrypt" {
				if err := writeAssuanLine(f.downstream, assuanForbidden); err != nil {
					return err
				}
				continue
			}
			if err := f.privateKeyOperation(ctx, line, "decrypt"); err != nil {
				return err
			}
		default:
			if err := writeAssuanLine(f.downstream, assuanForbidden); err != nil {
				return err
			}
		}
	}
}

func (f *openPGPFilter) handleOption(argument string) error {
	name := strings.ToLower(argument)
	if index := strings.IndexByte(name, '='); index >= 0 {
		name = name[:index]
	}
	switch name {
	case "display", "ttyname", "ttytype", "xauthority", "putenv", "lc-ctype", "lc-messages", "pinentry-mode":
		return writeAssuanLine(f.downstream, "OK")
	case "agent-awareness", "allow-pinentry-notify":
		line := "OPTION " + argument
		if f.optionBytes+len(line)+1 > assuanOperationMax {
			return writeAssuanLine(f.downstream, assuanForbidden)
		}
		if err := f.forwardSimple(line); err != nil {
			return err
		}
		f.options = append(f.options, line)
		f.optionBytes += len(line) + 1
		return nil
	default:
		return writeAssuanLine(f.downstream, assuanForbidden)
	}
}

func (f *openPGPFilter) handleHaveKey(argument string) error {
	if strings.HasPrefix(argument, "--list") {
		limit := 1000
		if _, value, ok := strings.Cut(argument, "="); ok {
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 0 || parsed > assuanOperationMax {
				return writeAssuanLine(f.downstream, assuanForbidden)
			}
			limit = parsed
		}
		grips := make([]string, 0, len(f.allowed))
		for grip := range f.allowed {
			grips = append(grips, grip)
		}
		sort.Strings(grips)
		data := make([]byte, 0, len(grips)*20)
		for _, grip := range grips {
			raw, _ := hex.DecodeString(grip)
			if len(data)+len(raw) > limit {
				break
			}
			data = append(data, raw...)
		}
		if len(data) != 0 {
			if err := writeAssuanLine(f.downstream, "D "+assuanEscapeData(data)); err != nil {
				return err
			}
		}
		return writeAssuanLine(f.downstream, "OK")
	}
	allowed := make([]string, 0)
	for _, grip := range strings.Fields(argument) {
		grip = strings.ToUpper(grip)
		if _, ok := f.allowed[grip]; ok {
			allowed = append(allowed, grip)
		}
	}
	if len(allowed) == 0 {
		return writeAssuanLine(f.downstream, assuanNoSecretKey)
	}
	return f.forwardSimple("HAVEKEY " + strings.Join(allowed, " "))
}

func (f *openPGPFilter) handleKeyInfo(argument string) error {
	if argument == "--list" {
		grips := make([]string, 0, len(f.allowed))
		for grip := range f.allowed {
			grips = append(grips, grip)
		}
		sort.Strings(grips)
		for _, grip := range grips {
			lines, err := f.exchange("KEYINFO " + grip)
			if err != nil {
				return err
			}
			for _, line := range lines {
				if strings.HasPrefix(line, "S KEYINFO ") {
					if err := writeAssuanLine(f.downstream, line); err != nil {
						return err
					}
				}
			}
		}
		return writeAssuanLine(f.downstream, "OK")
	}
	fields := strings.Fields(argument)
	if len(fields) == 0 || len(fields[0]) != 40 {
		return writeAssuanLine(f.downstream, assuanForbidden)
	}
	if _, ok := f.allowed[strings.ToUpper(fields[0])]; !ok {
		return writeAssuanLine(f.downstream, assuanNoSecretKey)
	}
	return f.forwardSimple("KEYINFO " + argument)
}

func (f *openPGPFilter) forwardSimple(line string) error {
	lines, err := f.exchange(line)
	if err != nil {
		return err
	}
	for _, response := range lines {
		if err := writeAssuanLine(f.downstream, response); err != nil {
			return err
		}
	}
	return nil
}

func (f *openPGPFilter) exchange(line string) ([]string, error) {
	if err := writeAssuanLine(f.upstream, line); err != nil {
		return nil, err
	}
	return f.readResponse()
}

func (f *openPGPFilter) readResponse() ([]string, error) {
	var lines []string
	total := 0
	for {
		line, err := readAssuanLine(f.reader)
		if err != nil {
			return nil, err
		}
		total += len(line) + 1
		if total > assuanOperationMax {
			return nil, fmt.Errorf("Assuan response exceeds %d bytes", assuanOperationMax)
		}
		if strings.HasPrefix(line, "INQUIRE PINENTRY_LAUNCHED") {
			if err := writeAssuanLine(f.upstream, "END"); err != nil {
				return nil, err
			}
			continue
		}
		if strings.HasPrefix(line, "INQUIRE ") {
			if f.downstreamReader == nil {
				if err := writeAssuanLine(f.upstream, "CAN"); err != nil {
					return nil, err
				}
				continue
			}
			if err := writeAssuanLine(f.downstream, line); err != nil {
				return nil, err
			}
			for {
				answer, answerErr := readAssuanLine(f.downstreamReader)
				if answerErr != nil {
					return nil, answerErr
				}
				total += len(answer) + 1
				command, _, _ := strings.Cut(answer, " ")
				command = strings.ToUpper(command)
				if total > assuanOperationMax || command != "D" && command != "END" && command != "CAN" {
					_ = writeAssuanLine(f.upstream, "CAN")
					return nil, fmt.Errorf("invalid Assuan inquiry response")
				}
				if err := writeAssuanLine(f.upstream, answer); err != nil {
					return nil, err
				}
				if command == "END" || command == "CAN" {
					break
				}
			}
			continue
		}
		lines = append(lines, line)
		if strings.HasPrefix(line, "OK") || strings.HasPrefix(line, "ERR") {
			return lines, nil
		}
	}
}

func (f *openPGPFilter) sign(ctx context.Context, command string) error {
	return f.privateKeyOperation(ctx, command, "sign")
}

func (f *openPGPFilter) privateKeyOperation(ctx context.Context, command, operation string) error {
	interactive := f.interactive
	if interactive == nil {
		interactive = interactiveCredentialSessionAvailable
	}
	if !interactive() {
		f.daemon.notifyCredentialFailure(ctx, f.host, f.hello, f.allowed[f.selectedGrip], operation, "no interactive session")
		return writeAssuanLine(f.downstream, assuanNoPinentry)
	}
	timeout, _ := time.ParseDuration(f.daemon.config.Credentials.GnuPG.OperationTimeout)
	deadline := time.Now().Add(timeout)
	finish := f.daemon.beginCredentialOperation(f.hello.Workspace, credentialCapabilityOpenPGP, operation)
	defer finish()
	if err := f.daemon.notifyCredentialOperation(ctx, f.host, f.hello, f.allowed[f.selectedGrip], operation); err != nil {
		f.daemon.notifyCredentialFailure(ctx, f.host, f.hello, f.allowed[f.selectedGrip], operation, "security notification delivery failed")
		return writeAssuanLine(f.downstream, assuanNoPinentry)
	}

	lines, err := f.exchangeWithDeadline(command, deadline)
	if err != nil {
		_ = f.upstream.Close()
		f.daemon.notifyCredentialFailure(ctx, f.host, f.hello, f.allowed[f.selectedGrip], operation, operation+" timed out")
		return writeAssuanLine(f.downstream, assuanTimeout)
	}
	if assuanScdaemonDied(lines) {
		if retryErr := f.reconnectAndReplay(deadline); retryErr == nil {
			lines, err = f.exchangeWithDeadline(command, deadline)
			if err != nil || assuanScdaemonDied(lines) {
				f.clearPrivateKeyState()
			}
			if err != nil {
				_ = f.upstream.Close()
			}
		}
	}
	if err != nil {
		return writeAssuanLine(f.downstream, assuanTimeout)
	}
	for _, line := range lines {
		if err := writeAssuanLine(f.downstream, line); err != nil {
			return err
		}
	}
	return nil
}

func (f *openPGPFilter) exchangeWithDeadline(line string, deadline time.Time) ([]string, error) {
	_ = f.upstream.SetDeadline(deadline)
	_ = f.downstream.SetDeadline(deadline)
	defer f.upstream.SetDeadline(time.Time{})
	defer f.downstream.SetDeadline(time.Time{})
	return f.exchange(line)
}

func (f *openPGPFilter) reconnectAndReplay(deadline time.Time) error {
	succeeded := false
	defer func() {
		if !succeeded {
			if f.upstream != nil {
				_ = f.upstream.Close()
			}
			f.clearPrivateKeyState()
		}
	}()
	_ = f.upstream.Close()
	upstream, _, err := f.dialUpstream()
	if err != nil {
		return err
	}
	f.upstream = upstream
	f.reader = bufio.NewReaderSize(upstream, assuanLineMax)
	_ = upstream.SetDeadline(deadline)
	lines := append([]string{"RESET"}, f.options...)
	keyCommand := map[string]string{"sign": "SIGKEY", "decrypt": "SETKEY"}[f.selectedFor]
	if keyCommand == "" || f.selectedGrip == "" {
		return fmt.Errorf("replay filtered GnuPG session without a selected key")
	}
	lines = append(lines, keyCommand+" "+f.selectedGrip)
	if f.description != "" {
		lines = append(lines, "SETKEYDESC "+assuanEscapeDescription(f.description))
	}
	if f.selectedFor == "sign" {
		if f.hashLine == "" {
			return fmt.Errorf("replay filtered signing session without a hash")
		}
		lines = append(lines, f.hashLine)
	}
	for _, line := range lines {
		responses, err := f.exchange(line)
		if err != nil || len(responses) == 0 || !strings.HasPrefix(responses[len(responses)-1], "OK") {
			return fmt.Errorf("replay filtered GnuPG session")
		}
	}
	succeeded = true
	return nil
}

func (f *openPGPFilter) clearPrivateKeyState() {
	f.selectedGrip, f.selectedFor, f.hashLine, f.description = "", "", "", ""
}

func readAssuanResponse(reader *bufio.Reader, upstream net.Conn) ([]string, error) {
	var lines []string
	total := 0
	for {
		line, err := readAssuanLine(reader)
		if err != nil {
			return nil, err
		}
		total += len(line) + 1
		if total > assuanOperationMax {
			return nil, fmt.Errorf("Assuan response exceeds %d bytes", assuanOperationMax)
		}
		if strings.HasPrefix(line, "INQUIRE PINENTRY_LAUNCHED") {
			if err := writeAssuanLine(upstream, "END"); err != nil {
				return nil, err
			}
			continue
		}
		if strings.HasPrefix(line, "INQUIRE ") {
			if err := writeAssuanLine(upstream, "CAN"); err != nil {
				return nil, err
			}
			continue
		}
		lines = append(lines, line)
		if strings.HasPrefix(line, "OK") || strings.HasPrefix(line, "ERR") {
			return lines, nil
		}
	}
}

func readAssuanLine(reader *bufio.Reader) (string, error) {
	var line []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		line = append(line, fragment...)
		if len(line) > assuanLineMax {
			return "", fmt.Errorf("Assuan line exceeds %d bytes", assuanLineMax)
		}
		if err == nil {
			return strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r"), nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return "", err
		}
	}
}

func writeAssuanLine(writer io.Writer, line string) error {
	_, err := io.WriteString(writer, line+"\n")
	return err
}

func splitAssuanCommand(line string) (string, string) {
	command, argument, _ := strings.Cut(strings.TrimSpace(line), " ")
	return strings.ToUpper(command), strings.TrimSpace(argument)
}

func validSetHash(argument string) bool {
	fields := strings.Fields(argument)
	if len(fields) != 2 {
		return false
	}
	digestLengths := map[int]int{
		1: 16, 2: 20, 3: 20,
		8: 32, 9: 48, 10: 64, 11: 28,
		12: 28, 13: 32, 14: 48, 15: 64,
	}
	expected := 0
	if name, found := strings.CutPrefix(strings.ToLower(fields[0]), "--hash="); found {
		expected = map[string]int{
			"md5": 16, "sha1": 20, "ripemd160": 20, "rmd160": 20,
			"sha256": 32, "sha384": 48, "sha512": 64, "sha224": 28,
			"sha3-224": 28, "sha3-256": 32, "sha3-384": 48, "sha3-512": 64,
			"tls-md5sha1": 36,
		}[name]
	} else {
		parsed, err := strconv.Atoi(fields[0])
		if err != nil {
			return false
		}
		expected = digestLengths[parsed]
	}
	if expected == 0 {
		return false
	}
	digest, err := hex.DecodeString(fields[1])
	return err == nil && len(digest) == expected
}

func assuanEscapeDescription(value string) string {
	var result strings.Builder
	for _, b := range []byte(value) {
		switch {
		case b == ' ':
			result.WriteByte('+')
		case b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || strings.ContainsRune("-._/:·", rune(b)):
			result.WriteByte(b)
		default:
			fmt.Fprintf(&result, "%%%02X", b)
		}
	}
	return result.String()
}

func assuanEscapeData(data []byte) string {
	var result strings.Builder
	for _, b := range data {
		if b >= 0x21 && b <= 0x7e && b != '%' {
			result.WriteByte(b)
		} else {
			fmt.Fprintf(&result, "%%%02X", b)
		}
	}
	return result.String()
}

func assuanScdaemonDied(lines []string) bool {
	if len(lines) == 0 || !strings.HasPrefix(lines[len(lines)-1], "ERR") {
		return false
	}
	detail := strings.ToLower(lines[len(lines)-1])
	return strings.Contains(detail, "<scd>") && (strings.Contains(detail, "daemon") || strings.Contains(detail, "card error") || strings.Contains(detail, "broken pipe"))
}

type credentialLockState struct {
	known  bool
	locked bool
}

func interactiveCredentialSessionAvailable() bool {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return false
	}
	_ = conn.Close()
	screenCtx, screenCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	screenState := credentialScreenSaverLockState(screenCtx)
	screenCancel()
	loginCtx, loginCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	loginState := credentialLogindLockState(loginCtx)
	loginCancel()
	return credentialSessionUnlocked(true, screenState, loginState)
}

func credentialSessionUnlocked(sessionBusAvailable bool, states ...credentialLockState) bool {
	if !sessionBusAvailable {
		return false
	}
	for _, state := range states {
		if state.known && state.locked {
			return false
		}
	}
	return true
}

func credentialScreenSaverLockState(ctx context.Context) credentialLockState {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return credentialLockState{}
	}
	defer conn.Close()
	for _, name := range []string{"org.freedesktop.ScreenSaver", "org.gnome.ScreenSaver"} {
		var owned bool
		if err := conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.NameHasOwner", 0, name).Store(&owned); err != nil || !owned {
			continue
		}
		var active bool
		path := dbus.ObjectPath("/org/freedesktop/ScreenSaver")
		if name == "org.gnome.ScreenSaver" {
			path = dbus.ObjectPath("/org/gnome/ScreenSaver")
		}
		if err := conn.Object(name, path).CallWithContext(ctx, name+".GetActive", 0).Store(&active); err == nil {
			return credentialLockState{known: true, locked: active}
		}
	}
	return credentialLockState{}
}

func credentialLogindLockState(ctx context.Context) credentialLockState {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return credentialLockState{}
	}
	defer conn.Close()
	var userPath dbus.ObjectPath
	if err := conn.Object("org.freedesktop.login1", dbus.ObjectPath("/org/freedesktop/login1")).
		CallWithContext(ctx, "org.freedesktop.login1.Manager.GetUser", 0, uint32(os.Getuid())).Store(&userPath); err != nil {
		return credentialLockState{}
	}
	type displaySession struct {
		Name string
		Path dbus.ObjectPath
	}
	var display displaySession
	user := conn.Object("org.freedesktop.login1", userPath)
	displayProperty, displayErr := credentialDBusProperty(ctx, user, "org.freedesktop.login1.User", "Display")
	if displayErr == nil {
		_ = displayProperty.Store(&display)
	}
	sessions := make([]displaySession, 0, 4)
	if display.Path.IsValid() && display.Path != "/" {
		sessions = append(sessions, display)
	}
	if sessionsProperty, sessionsErr := credentialDBusProperty(ctx, user, "org.freedesktop.login1.User", "Sessions"); sessionsErr == nil {
		var listed []displaySession
		if sessionsProperty.Store(&listed) == nil {
			sessions = append(sessions, listed...)
		}
	}
	known := false
	seen := map[dbus.ObjectPath]bool{}
	for _, session := range sessions {
		if !session.Path.IsValid() || session.Path == "/" || seen[session.Path] {
			continue
		}
		seen[session.Path] = true
		locked, propertyErr := credentialDBusProperty(ctx, conn.Object("org.freedesktop.login1", session.Path), "org.freedesktop.login1.Session", "LockedHint")
		if propertyErr != nil {
			continue
		}
		var value bool
		if locked.Store(&value) != nil {
			continue
		}
		known = true
		if value {
			return credentialLockState{known: true, locked: true}
		}
	}
	return credentialLockState{known: known}
}

func credentialDBusProperty(ctx context.Context, object dbus.BusObject, iface, name string) (dbus.Variant, error) {
	var value dbus.Variant
	err := object.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, iface, name).Store(&value)
	return value, err
}

func openPGPKeygripCardBacked(socket, grip string) bool {
	conn, err := net.DialTimeout("unix", socket, 500*time.Millisecond)
	if err != nil {
		return false
	}
	defer conn.Close()
	reader := bufio.NewReaderSize(conn, assuanLineMax)
	if greeting, err := readAssuanLine(reader); err != nil || !strings.HasPrefix(greeting, "OK") {
		return false
	}
	if err := writeAssuanLine(conn, "KEYINFO "+grip); err != nil {
		return false
	}
	lines, err := readAssuanResponse(reader, conn)
	if err != nil {
		return false
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "S KEYINFO ") {
			continue
		}
		fields := strings.Fields(line)
		// KEYINFO <grip> <type> <serialno> ...; a non-empty serial marks a
		// token-backed shadow key. Software keys use '-'.
		return len(fields) > 4 && fields[4] != "-" && fields[4] != ""
	}
	return false
}

func (d *Daemon) credentialNoticeContext(host string, hello credentialStreamHello, fingerprint, operation string) string {
	workspaceName := shortID(hello.Workspace)
	d.mu.Lock()
	if remote := d.state.Remotes[host]; remote != nil {
		if workspace := remote.Workspaces[hello.Workspace]; workspace != nil && workspace.Name != "" {
			workspaceName = workspace.Name
		}
	}
	nodeName := d.state.Node.Name
	d.mu.Unlock()
	if fingerprint != "" {
		fingerprint = shortFingerprint(fingerprint)
	}
	return strings.Join([]string{nodeName, workspaceName, hello.Bundle, "OpenPGP", operation, fingerprint}, " · ")
}

func shortFingerprint(fingerprint string) string {
	if len(fingerprint) <= 16 {
		return fingerprint
	}
	return fingerprint[:8] + "…" + fingerprint[len(fingerprint)-8:]
}

func (d *Daemon) notifyCredentialOperation(ctx context.Context, host string, hello credentialStreamHello, fingerprint, operation string) error {
	key := hello.Workspace + "\x00" + credentialCapabilityOpenPGP
	now := time.Now()
	d.credentialMu.Lock()
	notice := d.credentialNotices[key]
	if now.Sub(notice.At) <= credentialNoticeWindow {
		notice.Count++
	} else {
		notice.Count = 1
	}
	notice.At = now
	d.credentialNotices[key] = notice
	d.credentialMu.Unlock()
	body := d.credentialNoticeContext(host, hello, fingerprint, operation)
	if notice.Count > 1 {
		body += fmt.Sprintf(" · %d requests", notice.Count)
	}
	var desktopErr error
	if d.config.Notifications.DesktopEnabled && d.desktop != nil {
		notifyCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		desktopErr = d.desktop.Notify(notifyCtx, DesktopNotification{
			WorkspaceID: hello.Workspace, PaneID: "credential-openpgp", Summary: "Credential request",
			Body: body, Urgency: urgencyCritical, Icon: "dialog-password", ActionLabel: "",
		})
		cancel()
		if desktopErr == nil {
			return nil
		}
	}
	// Private-key use is special: unlike ordinary attention notifications, it
	// cannot proceed without a user-visible signal. The ntfy fallback therefore
	// ignores notifications.ntfy_enabled, which controls only best-effort notices.
	notifyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, _, ntfyErr := d.runner.Run(notifyCtx, d.config.Notifications.NtfyCommand,
		"-T", "Credential request", "-p", "5", "-g", "key", body)
	if ntfyErr != nil {
		if desktopErr != nil {
			return fmt.Errorf("desktop notification: %v; ntfy fallback: %w", desktopErr, ntfyErr)
		}
		return fmt.Errorf("ntfy fallback: %w", ntfyErr)
	}
	return nil
}

func (d *Daemon) notifyCredentialFailure(ctx context.Context, host string, hello credentialStreamHello, fingerprint, operation, reason string) {
	if !d.config.Notifications.NtfyEnabled {
		return
	}
	body := d.credentialNoticeContext(host, hello, fingerprint, operation) + " · " + reason
	notifyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, _, err := d.runner.Run(notifyCtx, d.config.Notifications.NtfyCommand,
		"-T", "Credential request refused", "-p", "5", "-g", "warning", body)
	if err != nil && ctx.Err() == nil {
		d.logger.Printf("credential ntfy delivery failed workspace=%s: %v", hello.Workspace, err)
	}
}
