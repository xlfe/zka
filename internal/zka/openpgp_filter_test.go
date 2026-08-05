package zka

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testAllowedGrip = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	testDeniedGrip  = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	testFingerprint = "1111222233334444555566667777888899990000"
)

type assuanFilterHarness struct {
	filter   *openPGPFilter
	client   net.Conn
	reader   *bufio.Reader
	commands chan string
	done     chan error
}

func newAssuanFilterHarness(t *testing.T, operationTimeout string, interactive bool, handler func(string) []string) *assuanFilterHarness {
	t.Helper()
	downstream, client := net.Pipe()
	upstream, agent := net.Pipe()
	runner := quietRunner()
	daemon := &Daemon{
		runner: runner, logger: log.New(io.Discard, "", 0), desktop: &fakeNotifier{},
		state:            StateData{Node: Host{ID: "provider", Name: "laptop"}, Remotes: map[string]*RemoteCache{}},
		credentialActive: map[string]int{}, credentialNotices: map[string]credentialNoticeState{},
	}
	daemon.config.Credentials.GnuPG.OperationTimeout = operationTimeout
	daemon.config.Notifications.NtfyEnabled = true
	daemon.config.Notifications.NtfyCommand = "ntfy-send"
	filter := &openPGPFilter{
		daemon: daemon, host: "devbox", hello: credentialStreamHello{Workspace: "workspace", Bundle: "work", Capability: credentialCapabilityOpenPGP, Generation: 1},
		downstream: downstream, upstream: upstream, reader: bufio.NewReaderSize(upstream, assuanLineMax),
		allowed: map[string]string{testAllowedGrip: testFingerprint}, interactive: func() bool { return interactive },
	}
	harness := &assuanFilterHarness{filter: filter, client: client, reader: bufio.NewReaderSize(client, assuanLineMax), commands: make(chan string, 64), done: make(chan error, 1)}
	go func() {
		reader := bufio.NewReaderSize(agent, assuanLineMax)
		for {
			line, err := readAssuanLine(reader)
			if err != nil {
				return
			}
			harness.commands <- line
			responses := []string{"OK"}
			if handler != nil {
				responses = handler(line)
			}
			for _, response := range responses {
				if err := writeAssuanLine(agent, response); err != nil {
					return
				}
			}
		}
	}()
	go func() { harness.done <- filter.serve(context.Background()) }()
	t.Cleanup(func() {
		_ = client.Close()
		_ = downstream.Close()
		_ = upstream.Close()
		_ = agent.Close()
	})
	return harness
}

func (h *assuanFilterHarness) exchange(t *testing.T, command string) []string {
	t.Helper()
	if err := writeAssuanLine(h.client, command); err != nil {
		t.Fatal(err)
	}
	var response []string
	for {
		line, err := readAssuanLine(h.reader)
		if err != nil {
			t.Fatal(err)
		}
		response = append(response, line)
		if strings.HasPrefix(line, "OK") || strings.HasPrefix(line, "ERR") {
			return response
		}
	}
}

func TestOpenPGPFilterEnforcesKeygripsOptionsAndDefaultDeny(t *testing.T) {
	h := newAssuanFilterHarness(t, "2m", true, func(command string) []string {
		if strings.HasPrefix(command, "KEYINFO ") {
			return []string{"S KEYINFO " + testAllowedGrip + " T SERIAL - - - - -", "OK"}
		}
		return []string{"OK"}
	})
	for _, command := range []string{
		"SIGKEY " + testDeniedGrip,
		"SETKEY " + testDeniedGrip,
		"DELETE_KEY " + testAllowedGrip,
	} {
		response := h.exchange(t, command)
		if !strings.HasPrefix(response[len(response)-1], "ERR 67108963") {
			t.Fatalf("%s response = %#v", command, response)
		}
	}
	for _, option := range []string{
		"display=:99", "ttyname=/dev/pts/9", "ttytype=xterm", "xauthority=/tmp/remote",
		"putenv=DISPLAY=:99", "lc-ctype=C", "lc-messages=C", "pinentry-mode=loopback",
	} {
		response := h.exchange(t, "OPTION "+option)
		if response[len(response)-1] != "OK" {
			t.Fatalf("filtered option %s = %#v", option, response)
		}
	}
	if response := h.exchange(t, "HAVEKEY "+testDeniedGrip); !strings.Contains(response[len(response)-1], "No secret key") {
		t.Fatalf("denied HAVEKEY = %#v", response)
	}
	if response := h.exchange(t, "KEYINFO "+testDeniedGrip); !strings.Contains(response[len(response)-1], "No secret key") {
		t.Fatalf("denied KEYINFO = %#v", response)
	}
	if response := h.exchange(t, "HAVEKEY "+testDeniedGrip+" "+testAllowedGrip); response[len(response)-1] != "OK" {
		t.Fatalf("filtered HAVEKEY = %#v", response)
	}
	listed := h.exchange(t, "KEYINFO --list")
	if strings.Contains(strings.Join(listed, "\n"), testDeniedGrip) || !strings.Contains(strings.Join(listed, "\n"), testAllowedGrip) {
		t.Fatalf("filtered KEYINFO list = %#v", listed)
	}
	if response := h.exchange(t, "SIGKEY "+testAllowedGrip); response[len(response)-1] != "OK" {
		t.Fatalf("allowed SIGKEY = %#v", response)
	}

	var forwarded []string
	for {
		select {
		case command := <-h.commands:
			forwarded = append(forwarded, command)
		default:
			joined := strings.Join(forwarded, "\n")
			if strings.Contains(joined, testDeniedGrip) || strings.Contains(joined, "ttyname") || strings.Contains(joined, "DELETE_KEY") {
				t.Fatalf("unsafe command reached provider agent:\n%s", joined)
			}
			if !strings.Contains(joined, "HAVEKEY "+testAllowedGrip) || !strings.Contains(joined, "SIGKEY "+testAllowedGrip) {
				t.Fatalf("allowed commands were not forwarded:\n%s", joined)
			}
			return
		}
	}
}

func TestOpenPGPFilterRefusesWithoutInteractiveSessionAndNotifies(t *testing.T) {
	h := newAssuanFilterHarness(t, "2m", false, nil)
	h.filter.selectedGrip = testAllowedGrip
	h.filter.selectedFor = "sign"
	h.filter.hashLine = "SETHASH 10 00112233445566778899AABBCCDDEEFF00112233"
	response := h.exchange(t, "PKSIGN")
	if response[len(response)-1] != assuanNoPinentry {
		t.Fatalf("PKSIGN response = %#v", response)
	}
	select {
	case command := <-h.commands:
		t.Fatalf("private-key command reached agent without a session: %s", command)
	default:
	}
	calls := h.filter.daemon.runner.(*fakeRunner).Calls()
	if len(calls) != 1 || calls[0].Name != "ntfy-send" || strings.Contains(strings.Join(calls[0].Args, " "), "PKSIGN") {
		t.Fatalf("ntfy calls = %#v", calls)
	}
}

func TestOpenPGPFilterRefusesWhenSecurityNoticeCannotBeDelivered(t *testing.T) {
	h := newAssuanFilterHarness(t, "2m", true, nil)
	h.filter.daemon.runner = &fakeRunner{handler: func(context.Context, string, ...string) (string, string, error) {
		return "", "", errors.New("notification unavailable")
	}}
	h.filter.daemon.config.Notifications.DesktopEnabled = false
	h.filter.daemon.config.Notifications.NtfyEnabled = false
	h.filter.selectedGrip = testAllowedGrip
	h.filter.selectedFor = "sign"
	h.filter.hashLine = "SETHASH 2 00112233445566778899AABBCCDDEEFF00112233"
	response := h.exchange(t, "PKSIGN")
	if response[len(response)-1] != assuanNoPinentry {
		t.Fatalf("PKSIGN response = %#v", response)
	}
	select {
	case command := <-h.commands:
		t.Fatalf("private-key command reached agent without notification: %s", command)
	default:
	}
}

func TestOpenPGPFilterDeadlineClosesUpstreamAndClearsOperation(t *testing.T) {
	block := make(chan struct{})
	var once sync.Once
	h := newAssuanFilterHarness(t, "25ms", true, func(command string) []string {
		if command == "PKSIGN" {
			once.Do(func() { close(block) })
			return nil
		}
		return []string{"OK"}
	})
	h.filter.selectedGrip = testAllowedGrip
	h.filter.selectedFor = "sign"
	h.filter.hashLine = "SETHASH 2 00112233445566778899AABBCCDDEEFF00112233"
	response := h.exchange(t, "PKSIGN")
	if response[len(response)-1] != assuanTimeout {
		t.Fatalf("PKSIGN response = %#v", response)
	}
	select {
	case <-block:
	case <-time.After(time.Second):
		t.Fatal("PKSIGN never reached the fake agent")
	}
	deadline := time.Now().Add(time.Second)
	for len(h.filter.daemon.credentialActiveOperationStatus()) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if active := h.filter.daemon.credentialActiveOperationStatus(); len(active) != 0 {
		t.Fatalf("active operations after timeout = %#v", active)
	}
}

func TestOpenPGPFilterRetriesDecryptWithSanitizedState(t *testing.T) {
	h := newAssuanFilterHarness(t, "2s", true, func(command string) []string {
		if command == "PKDECRYPT" {
			return []string{"ERR 100663404 Card error <SCD>"}
		}
		return []string{"OK"}
	})
	replayed := make(chan string, 8)
	var retryPeer net.Conn
	h.filter.dial = func() (net.Conn, string, error) {
		client, agent := net.Pipe()
		retryPeer = agent
		go func() {
			reader := bufio.NewReaderSize(agent, assuanLineMax)
			for {
				command, err := readAssuanLine(reader)
				if err != nil {
					return
				}
				replayed <- command
				if command == "PKDECRYPT" {
					_ = writeAssuanLine(agent, "D plaintext")
				}
				if err := writeAssuanLine(agent, "OK"); err != nil {
					return
				}
			}
		}()
		return client, "OK retry agent", nil
	}
	t.Cleanup(func() {
		if retryPeer != nil {
			_ = retryPeer.Close()
		}
	})
	if response := h.exchange(t, "SETKEY "+testAllowedGrip); response[len(response)-1] != "OK" {
		t.Fatalf("SETKEY response = %#v", response)
	}
	response := h.exchange(t, "PKDECRYPT")
	if strings.Join(response, "\n") != "D plaintext\nOK" {
		t.Fatalf("retried PKDECRYPT response = %#v", response)
	}
	commands := make([]string, 0, 3)
	for len(commands) < 3 {
		select {
		case command := <-replayed:
			commands = append(commands, command)
		case <-time.After(time.Second):
			t.Fatalf("replayed commands = %#v", commands)
		}
	}
	if got, want := strings.Join(commands, "\n"), "RESET\nSETKEY "+testAllowedGrip+"\nPKDECRYPT"; got != want {
		t.Fatalf("replayed commands = %q, want %q", got, want)
	}
}

func TestValidSetHashRequiresKnownAlgorithmAndExactDigestLength(t *testing.T) {
	if !validSetHash("2 00112233445566778899AABBCCDDEEFF00112233") {
		t.Fatal("valid SHA-1 hash was rejected")
	}
	if !validSetHash("--hash=sha256 00112233445566778899AABBCCDDEEFF00112233445566778899AABBCCDDEEFF") {
		t.Fatal("valid named SHA-256 hash was rejected")
	}
	for _, value := range []string{
		"999 00112233445566778899AABBCCDDEEFF00112233",
		"10 00112233445566778899AABBCCDDEEFF00112233",
		"2 0011",
		"2 not-hex",
		"--hash=unknown 00112233445566778899AABBCCDDEEFF00112233",
		"--inquire",
	} {
		if validSetHash(value) {
			t.Fatalf("invalid SETHASH argument accepted: %q", value)
		}
	}
}

func TestCredentialNotificationUsesAuthoritativeFieldsAndCoalesces(t *testing.T) {
	notifier := &fakeNotifier{}
	d := &Daemon{
		desktop: notifier, runner: quietRunner(), credentialNotices: map[string]credentialNoticeState{},
		state: StateData{Node: Host{ID: "provider", Name: "laptop"}, Remotes: map[string]*RemoteCache{
			"devbox": {Workspaces: map[string]*Workspace{"workspace": {ID: "workspace", Name: "example-project"}}},
		}},
	}
	d.config.Notifications.DesktopEnabled = true
	d.config.Notifications.NtfyCommand = "ntfy-send"
	hello := credentialStreamHello{Workspace: "workspace", Bundle: "work", Capability: credentialCapabilityOpenPGP}
	if err := d.notifyCredentialOperation(context.Background(), "devbox", hello, testFingerprint, "sign"); err != nil {
		t.Fatal(err)
	}
	if err := d.notifyCredentialOperation(context.Background(), "devbox", hello, testFingerprint, "sign"); err != nil {
		t.Fatal(err)
	}
	notes := notifier.Notes()
	if len(notes) != 2 || notes[1].Body != "laptop · example-project · work · OpenPGP · sign · 11112222…99990000 · 2 requests" {
		t.Fatalf("notifications = %#v", notes)
	}
}

func TestCredentialNotificationFallsBackToNtfyAndFailsClosed(t *testing.T) {
	hello := credentialStreamHello{Workspace: "workspace", Bundle: "work", Capability: credentialCapabilityOpenPGP}
	for _, desktopEnabled := range []bool{false, true} {
		runner := quietRunner()
		notifier := &fakeNotifier{err: errors.New("desktop unavailable")}
		d := &Daemon{
			desktop: notifier, runner: runner, credentialNotices: map[string]credentialNoticeState{},
			state: StateData{Node: Host{Name: "laptop"}, Remotes: map[string]*RemoteCache{}},
		}
		d.config.Notifications.DesktopEnabled = desktopEnabled
		d.config.Notifications.NtfyEnabled = false
		d.config.Notifications.NtfyCommand = "ntfy-send"
		if err := d.notifyCredentialOperation(context.Background(), "devbox", hello, testFingerprint, "sign"); err != nil {
			t.Fatalf("desktop enabled %v: %v", desktopEnabled, err)
		}
		if calls := runner.Calls(); len(calls) != 1 || calls[0].Name != "ntfy-send" {
			t.Fatalf("desktop enabled %v fallback calls = %#v", desktopEnabled, calls)
		}
	}

	runner := &fakeRunner{handler: func(context.Context, string, ...string) (string, string, error) {
		return "", "", errors.New("ntfy unavailable")
	}}
	d := &Daemon{
		runner: runner, credentialNotices: map[string]credentialNoticeState{},
		state: StateData{Node: Host{Name: "laptop"}, Remotes: map[string]*RemoteCache{}},
	}
	d.config.Notifications.NtfyCommand = "ntfy-send"
	if err := d.notifyCredentialOperation(context.Background(), "devbox", hello, testFingerprint, "sign"); err == nil {
		t.Fatal("private-key notice unexpectedly succeeded without a delivery channel")
	}
}

func TestCredentialSessionLockDecisionFailsOnlyOnAuthoritativeLock(t *testing.T) {
	if credentialSessionUnlocked(false) {
		t.Fatal("missing session bus was accepted")
	}
	if !credentialSessionUnlocked(true, credentialLockState{}, credentialLockState{known: true}) {
		t.Fatal("unknown and explicitly unlocked probes were rejected")
	}
	if credentialSessionUnlocked(true, credentialLockState{known: true, locked: true}, credentialLockState{}) {
		t.Fatal("authoritative locked state was accepted")
	}
}

func TestFailedReplayClearsPrivateKeySelection(t *testing.T) {
	oldClient, oldAgent := net.Pipe()
	t.Cleanup(func() { _ = oldAgent.Close() })
	f := &openPGPFilter{
		upstream: oldClient, selectedGrip: testAllowedGrip, selectedFor: "sign",
		hashLine: "SETHASH 2 00112233445566778899AABBCCDDEEFF00112233",
	}
	f.dial = func() (net.Conn, string, error) {
		client, agent := net.Pipe()
		go func() {
			defer agent.Close()
			reader := bufio.NewReaderSize(agent, assuanLineMax)
			for index := 0; index < 2; index++ {
				if _, err := readAssuanLine(reader); err != nil {
					return
				}
				if index == 0 {
					_ = writeAssuanLine(agent, "OK")
				} else {
					_ = writeAssuanLine(agent, assuanNoSecretKey)
				}
			}
		}()
		return client, "OK", nil
	}
	if err := f.reconnectAndReplay(time.Now().Add(time.Second)); err == nil {
		t.Fatal("failed replay unexpectedly succeeded")
	}
	if f.selectedGrip != "" || f.selectedFor != "" || f.hashLine != "" || f.description != "" {
		t.Fatalf("failed replay retained private state: %#v", f)
	}
}

func FuzzAssuanLineParser(f *testing.F) {
	for _, seed := range []string{"RESET\n", "OPTION ttyname=/dev/pts/1\n", "D %00%FF\n", strings.Repeat("A", assuanLineMax) + "\n"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		_, _ = readAssuanLine(bufio.NewReaderSize(strings.NewReader(input), 64))
	})
}
