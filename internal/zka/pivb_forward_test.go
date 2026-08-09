package zka

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testPIVBCard(t *testing.T) CredentialPIVBCard {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(spki)
	return CredentialPIVBCard{Serial: 12345678, KeyID: base64.RawURLEncoding.EncodeToString(digest[:]), SPKIDER: spki}
}

func serveFakePIVB(t *testing.T, handler http.Handler) string {
	t.Helper()
	directory := shortPIVBTestDir(t)
	socket := filepath.Join(directory, "forward.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go server.Serve(listener)
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
	})
	return socket
}

func shortPIVBTestDir(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "zka-pivb-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}

func TestPIVBBundleConfigRequiresAliasAllowlist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("ZKA_CONFIG", path)
	for _, raw := range []string{
		`{"credentials":{"bundles":{"work":{"pivb":{"enable":true}}}}}`,
		`{"credentials":{"bundles":{"work":{"pivb":{"enable":true,"aliases":["Deploy"]}}}}}`,
		`{"credentials":{"bundles":{"work":{"pivb":{"enable":true,"aliases":["ro","ro"]}}}}}`,
		`{"credentials":{"pivb":{"forward_socket":"relative.sock"},"bundles":{"work":{"pivb":{"enable":true,"aliases":["ro"]}}}}}`,
	} {
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(); err == nil {
			t.Fatalf("accepted invalid PIVB bundle: %s", raw)
		}
	}
	if err := os.WriteFile(path, []byte(`{"credentials":{"bundles":{"work":{"pivb":{"enable":true,"aliases":["ro","deploy"]}}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil || !cfg.Credentials.Bundles["work"].PIVB.Enable {
		t.Fatalf("valid PIVB bundle = %#v, %v", cfg.Credentials.Bundles["work"], err)
	}
}

func TestPIVBForwardProtocolV2GoldenFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/pivb_forward_v2.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		MintRequest  json.RawMessage `json:"mint_request"`
		MintResponse json.RawMessage `json:"mint_response"`
	}
	if err := decodeStrictJSON(raw, &fixture); err != nil {
		t.Fatal(err)
	}

	var request pivbMintRequest
	assertPIVBForwardGoldenValue(t, fixture.MintRequest, &request)
	if request.Version != pivbForwardProtocolVersion {
		t.Fatalf("golden request version = %d, want %d", request.Version, pivbForwardProtocolVersion)
	}
	var response pivbMintResponse
	assertPIVBForwardGoldenValue(t, fixture.MintResponse, &response)
	if response.Version != pivbForwardProtocolVersion {
		t.Fatalf("golden response version = %d, want %d", response.Version, pivbForwardProtocolVersion)
	}
}

func assertPIVBForwardGoldenValue(t *testing.T, raw json.RawMessage, value any) {
	t.Helper()
	if err := decodeStrictJSON(raw, value); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, compact.Bytes()) {
		t.Fatalf("wire encoding changed\n got: %s\nwant: %s", encoded, compact.Bytes())
	}
}

func TestPIVBBundleAppearsInCredentialConfigDoctor(t *testing.T) {
	var cfg Config
	var bundle CredentialBundleConfig
	bundle.PIVB.Enable = true
	bundle.PIVB.Aliases = []string{"ro"}
	cfg.Credentials.Bundles = map[string]CredentialBundleConfig{"work": bundle}
	check := credentialsConfigDoctorCheck(cfg)
	if !check.OK || check.Detail != "work=pivb" {
		t.Fatalf("PIVB config doctor = %#v", check)
	}
	cfg.Credentials.PIVB.ForwardSocket = serveFakePIVB(t, http.NewServeMux())
	if provider := credentialsProviderDoctorCheck(context.Background(), cfg, quietRunner()); !provider.OK {
		t.Fatalf("PIVB provider doctor = %#v", provider)
	}
}

func TestBuildPIVBManifestFiltersAliasesAndPinsCard(t *testing.T) {
	card := testPIVBCard(t)
	handler := http.NewServeMux()
	handler.HandleFunc("GET /v1/describe", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(pivbForwardDescription{
			Version: pivbForwardProtocolVersion, ProviderResource: "projects/1/locations/global/workloadIdentityPools/pool/providers/provider",
			IssuerURI: "https://issuer.example", Card: card,
			Aliases: map[string]CredentialPIVBAlias{"ro": {Target: "ro@example.iam.gserviceaccount.com"}, "deploy": {Target: "deploy@example.iam.gserviceaccount.com"}},
		})
	})
	var cfg Config
	cfg.Credentials.PIVB.ForwardSocket = serveFakePIVB(t, handler)
	manifest, err := buildPIVBManifest(context.Background(), cfg, []string{"ro"})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Card.Serial != card.Serial || manifest.Card.KeyID != card.KeyID || len(manifest.Aliases) != 1 || manifest.Aliases["ro"].Target == "" {
		t.Fatalf("manifest = %#v", manifest)
	}
}

func TestPIVBClaimPolicyRejectsOriginMismatchBeforeRouting(t *testing.T) {
	card := testPIVBCard(t)
	handler := http.NewServeMux()
	handler.HandleFunc("GET /v1/policy", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(pivbForwardPolicy{
			Version: pivbForwardProtocolVersion, ProviderResource: "projects/1/locations/global/workloadIdentityPools/pool/providers/provider",
			IssuerURI: "https://issuer.example", Aliases: map[string]CredentialPIVBAlias{"ro": {Target: "different@example"}},
			EnrolledKeys: []pivbEnrolledKey{{Serial: card.Serial, KeyID: card.KeyID}},
		})
	})
	var cfg Config
	cfg.Credentials.PIVB.ForwardSocket = serveFakePIVB(t, handler)
	manifest := &CredentialPIVBManifest{
		ProtocolVersion: pivbForwardProtocolVersion, ProviderResource: "projects/1/locations/global/workloadIdentityPools/pool/providers/provider",
		IssuerURI: "https://issuer.example", Aliases: map[string]CredentialPIVBAlias{"ro": {Target: "ro@example"}}, Card: card,
	}
	err := validatePIVBClaimAgainstLocalPolicy(context.Background(), cfg, manifest)
	if err == nil || !strings.Contains(err.Error(), "alias") {
		t.Fatalf("origin policy mismatch = %v", err)
	}
}

func TestPIVBProxyInjectsClaimedIdentityAndContext(t *testing.T) {
	card := testPIVBCard(t)
	received := make(chan pivbMintRequest, 1)
	handler := http.NewServeMux()
	handler.HandleFunc("POST /v1/mint", func(w http.ResponseWriter, r *http.Request) {
		var req pivbMintRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		received <- req
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":"locked","code":"PIVB_LOCKED"}`)
	})
	var cfg Config
	cfg.Credentials.PIVB.ForwardSocket = serveFakePIVB(t, handler)
	d := &Daemon{config: cfg, state: StateData{Node: Host{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}, credentialActive: map[string]int{}}
	manifest := &CredentialPIVBManifest{ProtocolVersion: pivbForwardProtocolVersion, Aliases: map[string]CredentialPIVBAlias{"ro": {Target: "ro@example"}}, Card: card}
	client, server := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	go func() {
		done <- d.proxyPIVBMint(context.Background(), server, "workspace", "work", 7, "attachment", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", manifest)
	}()
	requestBody := `{"version":2,"alias":"ro","external_account_audience":"audience","impersonated_email":"ro@example","request_source":{"kind":"agent-session","label":"codex:project/ro","session_id":"0123456789abcdef0123456789abcdef"},"expected_card":{"serial":0,"jwk_kid":"","spki_der":null},"forward_context":{"origin_node_id":"","workspace_id":"","bundle":"","claim_generation":0,"provider_node_id":"","operation_id":""}}`
	req, _ := http.NewRequest(http.MethodPost, "http://pivb-forward/v1/mint", strings.NewReader(requestBody))
	if err := req.Write(client); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(client), req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got := <-received
	if got.ExpectedCard.Serial != card.Serial || got.ForwardContext.WorkspaceID != "workspace" || got.ForwardContext.ClaimGeneration != 7 || got.ForwardContext.OperationID == "" {
		t.Fatalf("injected request = %#v", got)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPIVBProxyRejectsAliasOutsideBundleBeforeProvider(t *testing.T) {
	providerCalled := make(chan struct{}, 1)
	handler := http.NewServeMux()
	handler.HandleFunc("POST /v1/mint", func(http.ResponseWriter, *http.Request) { providerCalled <- struct{}{} })
	var cfg Config
	cfg.Credentials.PIVB.ForwardSocket = serveFakePIVB(t, handler)
	d := &Daemon{config: cfg, state: StateData{Node: Host{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}, credentialActive: map[string]int{}}
	manifest := &CredentialPIVBManifest{ProtocolVersion: pivbForwardProtocolVersion, Aliases: map[string]CredentialPIVBAlias{"ro": {Target: "ro@example"}}, Card: testPIVBCard(t)}
	client, server := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	go func() {
		done <- d.proxyPIVBMint(context.Background(), server, "workspace", "work", 1, "", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", manifest)
	}()
	body := `{"version":2,"alias":"deploy","external_account_audience":"audience","impersonated_email":"deploy@example","request_source":{"kind":"agent-session","label":"codex:project/deploy","session_id":"0123456789abcdef0123456789abcdef"}}`
	req, _ := http.NewRequest(http.MethodPost, "http://pivb-forward/v1/mint", strings.NewReader(body))
	if err := req.Write(client); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(client), req)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(responseBody), "not allowed") {
		t.Fatalf("disallowed alias response = %d %s", resp.StatusCode, responseBody)
	}
	select {
	case <-providerCalled:
		t.Fatal("disallowed alias reached PIVB provider")
	default:
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestBindPIVBMintResponsePinsCardAndOriginContext(t *testing.T) {
	providerCard := testPIVBCard(t)
	pinnedCard := testPIVBCard(t)
	providerContext := pivbForwardContext{
		OriginNodeID: strings.Repeat("f", 32), WorkspaceID: strings.Repeat("e", 32), Bundle: "wrong",
		ClaimGeneration: 99, ProviderNodeID: strings.Repeat("d", 32), OperationID: strings.Repeat("c", 32),
	}
	raw, err := json.Marshal(pivbMintResponse{
		Version: pivbForwardProtocolVersion, IDToken: "header.payload.signature", ExpirationTime: 123,
		Card: providerCard, ExpectedCard: providerCard, ForwardContext: providerContext,
	})
	if err != nil {
		t.Fatal(err)
	}
	trusted := pivbForwardContext{
		OriginNodeID: strings.Repeat("a", 32), WorkspaceID: strings.Repeat("b", 32), Bundle: "work",
		ClaimGeneration: 7, ProviderNodeID: strings.Repeat("d", 32), ProviderAttachID: strings.Repeat("e", 32),
	}
	bound, err := bindPIVBMintResponse(raw, pinnedCard, trusted)
	if err != nil {
		t.Fatal(err)
	}
	var response pivbMintResponse
	if err := decodeStrictJSON(bound, &response); err != nil {
		t.Fatal(err)
	}
	if response.ExpectedCard.Serial != pinnedCard.Serial || response.ExpectedCard.KeyID != pinnedCard.KeyID ||
		!bytes.Equal(response.ExpectedCard.SPKIDER, pinnedCard.SPKIDER) {
		t.Fatalf("origin did not bind the active route card: %#v", response.ExpectedCard)
	}
	if response.ForwardContext.OriginNodeID != trusted.OriginNodeID || response.ForwardContext.WorkspaceID != trusted.WorkspaceID ||
		response.ForwardContext.Bundle != trusted.Bundle || response.ForwardContext.ClaimGeneration != trusted.ClaimGeneration ||
		response.ForwardContext.OperationID != providerContext.OperationID {
		t.Fatalf("origin did not bind trusted route context: %#v", response.ForwardContext)
	}
}

func TestCredentialTargetPIVBProxyBindsOriginRoute(t *testing.T) {
	providerCard := testPIVBCard(t)
	pinnedCard := testPIVBCard(t)
	providerContext := pivbForwardContext{
		OriginNodeID: strings.Repeat("f", 32), WorkspaceID: strings.Repeat("e", 32), Bundle: "provider",
		ClaimGeneration: 99, ProviderNodeID: strings.Repeat("d", 32), OperationID: strings.Repeat("c", 32),
	}
	trusted := pivbForwardContext{
		OriginNodeID: strings.Repeat("a", 32), WorkspaceID: strings.Repeat("b", 32), Bundle: "work",
		ClaimGeneration: 7, ProviderNodeID: strings.Repeat("d", 32), ProviderAttachID: strings.Repeat("e", 32),
	}
	responseBody, err := json.Marshal(pivbMintResponse{
		Version: pivbForwardProtocolVersion, IDToken: "header.payload.signature", ExpirationTime: 123,
		Card: providerCard, ExpectedCard: providerCard, ForwardContext: providerContext,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, body := runPIVBCredentialTargetProxy(t, pinnedCard, trusted, func(remote net.Conn) error {
		return writePIVBTestResponse(remote, http.StatusOK, responseBody)
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy status = %d, body = %s", resp.StatusCode, body)
	}
	var bound pivbMintResponse
	if err := decodeStrictJSON(body, &bound); err != nil {
		t.Fatal(err)
	}
	if bound.ExpectedCard.Serial != pinnedCard.Serial || bound.ExpectedCard.KeyID != pinnedCard.KeyID || !bytes.Equal(bound.ExpectedCard.SPKIDER, pinnedCard.SPKIDER) {
		t.Fatalf("proxy did not stamp the origin route card: %#v", bound.ExpectedCard)
	}
	if bound.ForwardContext.OriginNodeID != trusted.OriginNodeID || bound.ForwardContext.WorkspaceID != trusted.WorkspaceID ||
		bound.ForwardContext.Bundle != trusted.Bundle || bound.ForwardContext.ClaimGeneration != trusted.ClaimGeneration ||
		bound.ForwardContext.OperationID != providerContext.OperationID {
		t.Fatalf("proxy did not stamp the origin route context: %#v", bound.ForwardContext)
	}
}

func TestCredentialTargetPIVBProxyMapsTransportAndBindingFailures(t *testing.T) {
	card := testPIVBCard(t)
	trusted := pivbForwardContext{
		OriginNodeID: strings.Repeat("a", 32), WorkspaceID: strings.Repeat("b", 32), Bundle: "work",
		ClaimGeneration: 7, ProviderNodeID: strings.Repeat("c", 32),
	}
	tests := []struct {
		name       string
		remote     func(net.Conn) error
		wantStatus int
		wantCode   string
	}{
		{
			name: "transport",
			remote: func(remote net.Conn) error {
				return remote.Close()
			},
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "PIVB_UNAVAILABLE",
		},
		{
			name: "oversized",
			remote: func(remote net.Conn) error {
				return writePIVBTestResponse(remote, http.StatusOK, bytes.Repeat([]byte("x"), pivbForwardResponseMax+1))
			},
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "PIVB_UNAVAILABLE",
		},
		{
			name: "binding",
			remote: func(remote net.Conn) error {
				body, err := json.Marshal(pivbMintResponse{
					Version: pivbForwardProtocolVersion - 1, IDToken: "header.payload.signature", ExpirationTime: 123,
					Card: card, ExpectedCard: card, ForwardContext: pivbForwardContext{OperationID: strings.Repeat("d", 32)},
				})
				if err != nil {
					return err
				}
				return writePIVBTestResponse(remote, http.StatusOK, body)
			},
			wantStatus: http.StatusForbidden,
			wantCode:   "PIVB_CONFIG",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resp, body := runPIVBCredentialTargetProxy(t, card, trusted, test.remote)
			if resp.StatusCode != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", resp.StatusCode, test.wantStatus, body)
			}
			var result struct {
				Error string `json:"error"`
				Code  string `json:"code"`
			}
			if err := decodeStrictJSON(body, &result); err != nil {
				t.Fatal(err)
			}
			if result.Code != test.wantCode {
				t.Fatalf("code = %q, want %q; body = %s", result.Code, test.wantCode, body)
			}
		})
	}
}

func runPIVBCredentialTargetProxy(t *testing.T, card CredentialPIVBCard, trusted pivbForwardContext, remote func(net.Conn) error) (*http.Response, []byte) {
	t.Helper()
	client, sandbox := net.Pipe()
	stream, provider := net.Pipe()
	listener := &credentialTargetListener{
		capability: credentialCapabilityPIVB, done: make(chan struct{}), active: map[net.Conn]struct{}{},
		pivbCard: card, pivbContext: trusted,
	}
	listener.active[client] = struct{}{}
	proxyDone := make(chan struct{})
	go func() {
		listener.proxy(context.Background(), client, stream)
		close(proxyDone)
	}()
	remoteDone := make(chan error, 1)
	go func() {
		defer provider.Close()
		remoteDone <- remote(provider)
	}()
	if err := sandbox.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(sandbox), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	_ = sandbox.Close()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-proxyDone:
	case <-time.After(3 * time.Second):
		t.Fatal("PIVB credential target proxy did not stop")
	}
	if err := <-remoteDone; err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
	return resp, body
}

func writePIVBTestResponse(conn net.Conn, status int, body []byte) error {
	response := &http.Response{
		StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), ProtoMajor: 1, ProtoMinor: 1,
		Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)),
	}
	response.Header.Set("Content-Type", "application/json")
	return response.Write(conn)
}

func TestPIVBProxyCancelsLocalMintWhenClientDisconnects(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	handler := http.NewServeMux()
	handler.HandleFunc("POST /v1/mint", func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		close(started)
		<-r.Context().Done()
		close(cancelled)
	})
	var cfg Config
	cfg.Credentials.PIVB.ForwardSocket = serveFakePIVB(t, handler)
	d := &Daemon{config: cfg, state: StateData{Node: Host{ID: strings.Repeat("a", 32)}}, credentialActive: map[string]int{}}
	card := testPIVBCard(t)
	manifest := &CredentialPIVBManifest{
		ProtocolVersion: pivbForwardProtocolVersion,
		Aliases:         map[string]CredentialPIVBAlias{"ro": {Target: "ro@example"}},
		Card:            card,
	}
	client, server := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- d.proxyPIVBMint(context.Background(), server, "workspace", "work", 7, "", strings.Repeat("b", 32), manifest)
	}()
	body, err := json.Marshal(pivbMintRequest{
		Version: pivbForwardProtocolVersion, Alias: "ro", ExternalAccountAudience: "audience", ImpersonatedEmail: "ro@example",
		RequestSource: &pivbForwardSource{Kind: "agent-session", Label: "codex:agentic/ro"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://pivb-forward/v1/mint", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if err := req.Write(client); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("local PIVB provider did not receive mint")
	}
	_ = client.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("local PIVB proxy did not stop after client disconnect")
	}
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("local PIVB provider was not cancelled after client disconnect")
	}
}

func TestLocalPIVBListenerRejectsConnectionAcceptedBeforeTakeover(t *testing.T) {
	d, err := newTestDaemon(t, shortPIVBTestDir(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	workspace := createTestWorkspace(t, d, 1)
	binding := WorkspacePIVBProvider{Source: "local", Bundle: "work", Generation: 1, State: "ready"}
	d.mu.Lock()
	d.state.Workspaces[workspace.ID].PIVBProvider = &binding
	if err := d.store.Save(d.state); err != nil {
		d.mu.Unlock()
		t.Fatal(err)
	}
	d.mu.Unlock()
	listener, err := d.startLocalPIVBListener(workspace.ID, binding)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(listener.close)

	// Hold the state lock so serve accepts and records the connection but
	// cannot authorize it until after the replacement is committed.
	d.mu.Lock()
	client, err := net.Dial("unix", listener.path)
	if err != nil {
		d.mu.Unlock()
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	accepted := false
	for time.Now().Before(deadline) {
		listener.mu.Lock()
		accepted = len(listener.active) == 1
		listener.mu.Unlock()
		if accepted {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !accepted {
		d.mu.Unlock()
		_ = client.Close()
		t.Fatal("local PIVB listener did not accept connection")
	}
	d.state.Workspaces[workspace.ID].PIVBProvider = &WorkspacePIVBProvider{
		Source: "attachment", Bundle: "work", Generation: 2, State: "ready", OwnerAttachmentID: "remote",
	}
	if err := d.store.Save(d.state); err != nil {
		d.mu.Unlock()
		_ = client.Close()
		t.Fatal(err)
	}
	d.mu.Unlock()

	if err := client.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	_ = client.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(responseBody), "local PIVB route has been replaced") {
		t.Fatalf("post-takeover response = %d %s", resp.StatusCode, responseBody)
	}
}

func TestActivateLocalPIVBIsWorkspaceScopedAndCannotReplaceRemote(t *testing.T) {
	card := testPIVBCard(t)
	handler := http.NewServeMux()
	handler.HandleFunc("GET /v1/describe", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(pivbForwardDescription{
			Version: pivbForwardProtocolVersion, ProviderResource: "projects/1/locations/global/workloadIdentityPools/pool/providers/provider",
			IssuerURI: "https://issuer.example", Card: card,
			Aliases: map[string]CredentialPIVBAlias{"ro": {Target: "ro@example.iam.gserviceaccount.com"}},
		})
	})
	handler.HandleFunc("GET /v1/policy", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(pivbForwardPolicy{
			Version: pivbForwardProtocolVersion, ProviderResource: "projects/1/locations/global/workloadIdentityPools/pool/providers/provider",
			IssuerURI: "https://issuer.example", Aliases: map[string]CredentialPIVBAlias{"ro": {Target: "ro@example.iam.gserviceaccount.com"}},
			EnrolledKeys: []pivbEnrolledKey{{Serial: card.Serial, KeyID: card.KeyID}},
		})
	})
	d, err := newTestDaemon(t, shortPIVBTestDir(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	var bundle CredentialBundleConfig
	bundle.PIVB.Enable = true
	bundle.PIVB.Aliases = []string{"ro"}
	d.config.Credentials.Bundles = map[string]CredentialBundleConfig{"work": bundle}
	d.config.Credentials.PIVB.ForwardSocket = serveFakePIVB(t, handler)
	workspace := createTestWorkspace(t, d, 1)
	status, err := d.activateLocalPIVB(context.Background(), workspace.ID, "work", false)
	if err != nil {
		t.Fatal(err)
	}
	if status.Capabilities[credentialCapabilityPIVB].Available != true {
		t.Fatalf("local status = %#v", status)
	}
	d.credentialMu.Lock()
	localListener := d.pivbLocalListeners[workspace.ID]
	d.credentialMu.Unlock()
	if localListener == nil {
		t.Fatal("local activation reported ready without publishing its listener")
	}
	firstGeneration := d.state.Workspaces[workspace.ID].PIVBProvider.Generation
	if _, err := d.activateLocalPIVB(context.Background(), workspace.ID, "work", false); err != nil {
		t.Fatal(err)
	}
	if got := d.state.Workspaces[workspace.ID].PIVBProvider.Generation; got != firstGeneration {
		t.Fatalf("idempotent local activation changed generation from %d to %d", firstGeneration, got)
	}
	endpoint, err := d.pivbEndpoint(workspace.ID)
	if err != nil || endpoint.Source != "local" || endpoint.Socket == "" {
		t.Fatalf("endpoint = %#v, %v", endpoint, err)
	}
	attachment := readyCredentialAttachment(t, d, workspace, "remote", "provider")
	manifest := &CredentialPIVBManifest{
		ProtocolVersion:  pivbForwardProtocolVersion,
		ProviderResource: "projects/1/locations/global/workloadIdentityPools/pool/providers/provider",
		IssuerURI:        "https://issuer.example",
		Aliases:          map[string]CredentialPIVBAlias{"ro": {Target: "ro@example.iam.gserviceaccount.com"}},
		Card:             card,
	}
	if _, err := d.claimWorkspaceCredentials(context.Background(), workspaceCredentialRequest{
		Workspace: workspace.ID, Attachment: attachment.ID, Bundle: "work",
		Manifest: credentialBundleManifest{Bundle: "work", PIVB: manifest},
	}); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	remoteProvider := d.state.Workspaces[workspace.ID].PIVBProvider
	d.mu.Unlock()
	if remoteProvider == nil || remoteProvider.Source != "attachment" || remoteProvider.OwnerAttachmentID != attachment.ID || remoteProvider.Generation <= firstGeneration {
		t.Fatalf("remote provider did not atomically replace local provider: %#v", remoteProvider)
	}
	d.credentialMu.Lock()
	staleLocal := d.pivbLocalListeners[workspace.ID]
	d.credentialMu.Unlock()
	if staleLocal != nil || d.localPIVBBindingCurrent(workspace.ID, firstGeneration) {
		t.Fatalf("local route remained authorized after remote takeover: listener=%#v", staleLocal)
	}
	if noOp, err := d.activateLocalPIVB(context.Background(), workspace.ID, "work", true); err != nil || noOp.OwnerAttachment != attachment.ID {
		t.Fatalf("if-unclaimed activation changed or rejected remote route: status=%#v err=%v", noOp, err)
	}
	if _, err := d.activateLocalPIVB(context.Background(), workspace.ID, "work", false); err == nil || !strings.Contains(err.Error(), "will not replace") {
		t.Fatalf("remote route replacement error = %v", err)
	}
	if _, err := d.releaseWorkspaceCredentials(workspace.ID); err != nil {
		t.Fatal(err)
	}
	endpoint, err = d.pivbEndpoint(workspace.ID)
	if err != nil || endpoint.State != "unclaimed" || d.state.Workspaces[workspace.ID].PIVBProvider != nil {
		t.Fatalf("release silently restored a PIVB provider: endpoint=%#v provider=%#v err=%v", endpoint, d.state.Workspaces[workspace.ID].PIVBProvider, err)
	}
}

func TestCredentialTransportDegradationPreservesLocalPIVB(t *testing.T) {
	status := workspaceCredentialStatus{
		State: "ready",
		Capabilities: map[string]credentialCapabilityView{
			credentialCapabilitySSH:  {State: "ready", Available: true},
			credentialCapabilityPIVB: {State: "ready", Available: true, Detail: "local YubiKey"},
		},
	}
	workspace := &Workspace{PIVBProvider: &WorkspacePIVBProvider{Source: "local", State: "ready"}}
	degradeCredentialStatus(&status, workspace)
	if status.Capabilities[credentialCapabilitySSH].Available {
		t.Fatal("disconnected credential transport left SSH available")
	}
	if pivb := status.Capabilities[credentialCapabilityPIVB]; !pivb.Available || pivb.State != "ready" || pivb.Detail != "local YubiKey" {
		t.Fatalf("transport degradation incorrectly blanked local PIVB: %#v", pivb)
	}
}

func TestLocalPIVBListenerFailureIsPersistedAsDegraded(t *testing.T) {
	card := testPIVBCard(t)
	handler := http.NewServeMux()
	handler.HandleFunc("GET /v1/describe", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(pivbForwardDescription{
			Version: pivbForwardProtocolVersion, ProviderResource: "projects/1/locations/global/workloadIdentityPools/pool/providers/provider",
			IssuerURI: "https://issuer.example", Card: card,
			Aliases: map[string]CredentialPIVBAlias{"ro": {Target: "ro@example.iam.gserviceaccount.com"}},
		})
	})
	d, err := newTestDaemon(t, shortPIVBTestDir(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	var bundle CredentialBundleConfig
	bundle.PIVB.Enable = true
	bundle.PIVB.Aliases = []string{"ro"}
	d.config.Credentials.Bundles = map[string]CredentialBundleConfig{"work": bundle}
	d.config.Credentials.PIVB.ForwardSocket = serveFakePIVB(t, handler)
	workspace := createTestWorkspace(t, d, 1)
	blocker, err := listenUnix(pivbRelaySocketPath(d.paths, workspace.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()

	if _, err := d.activateLocalPIVB(context.Background(), workspace.ID, "work", false); err == nil {
		t.Fatal("local activation reported success while another live listener owned the route")
	}
	d.mu.Lock()
	provider := d.state.Workspaces[workspace.ID].PIVBProvider
	d.mu.Unlock()
	if provider == nil || provider.State != "degraded" || provider.LastError == "" {
		t.Fatalf("listener failure was not observable in provider state: %#v", provider)
	}
	endpoint, err := d.pivbEndpoint(workspace.ID)
	if err != nil || endpoint.State != "degraded" || endpoint.Detail == "" {
		t.Fatalf("degraded endpoint = %#v, %v", endpoint, err)
	}
}
