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
	socket := filepath.Join(testRoot(t), "forward.sock")
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

// ZKA does not call POST /v1/invalidate yet, but the golden fixture is shared
// byte-for-byte with PIVB's copy at internal/forwardapi/testdata, so its shape
// is pinned here now to keep the two repos from drifting apart.
type pivbInvalidateRequestGolden struct {
	Version         int    `json:"version"`
	WorkspaceID     string `json:"workspace_id"`
	ClaimGeneration uint64 `json:"claim_generation"`
}

type pivbInvalidateResponseGolden struct {
	Version int `json:"version"`
	Purged  int `json:"purged"`
}

func TestPIVBForwardProtocolV3GoldenFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/pivb_forward_v3.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		MintRequest        json.RawMessage `json:"mint_request"`
		MintResponse       json.RawMessage `json:"mint_response"`
		Policy             json.RawMessage `json:"policy"`
		Description        json.RawMessage `json:"description"`
		InvalidateRequest  json.RawMessage `json:"invalidate_request"`
		InvalidateResponse json.RawMessage `json:"invalidate_response"`
	}
	if err := decodeStrictJSON(raw, &fixture); err != nil {
		t.Fatal(err)
	}

	var request pivbMintRequest
	assertPIVBForwardGoldenValue(t, fixture.MintRequest, &request)
	var response pivbMintResponse
	assertPIVBForwardGoldenValue(t, fixture.MintResponse, &response)
	var policy pivbForwardPolicy
	assertPIVBForwardGoldenValue(t, fixture.Policy, &policy)
	var description pivbForwardDescription
	assertPIVBForwardGoldenValue(t, fixture.Description, &description)
	var invalidateRequest pivbInvalidateRequestGolden
	assertPIVBForwardGoldenValue(t, fixture.InvalidateRequest, &invalidateRequest)
	var invalidateResponse pivbInvalidateResponseGolden
	assertPIVBForwardGoldenValue(t, fixture.InvalidateResponse, &invalidateResponse)

	for name, version := range map[string]int{
		"mint request": request.Version, "mint response": response.Version,
		"policy": policy.Version, "description": description.Version,
		"invalidate request": invalidateRequest.Version, "invalidate response": invalidateResponse.Version,
	} {
		if version != pivbForwardProtocolVersion {
			t.Errorf("golden %s version = %d, want %d", name, version, pivbForwardProtocolVersion)
		}
	}
	// Every protocol 3 addition is omitempty, so only non-zero fixture values
	// prove the fields reach the wire at all.
	if request.ForwardContext.WindowSeconds == 0 || request.ForwardContext.WindowDeadline == 0 ||
		response.GrantedWindowSeconds == 0 || response.GrantedWindowDeadline == 0 ||
		policy.MaxGrantWindowS == 0 || description.MaxGrantWindowS == 0 ||
		policy.Aliases["deploy"].AssertionLifetimeS == 0 || description.Aliases["deploy"].AssertionLifetimeS == 0 {
		t.Fatal("golden fixture does not pin the protocol 3 window and assertion-lifetime fields")
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
			IssuerURI: "https://issuer.example", Card: card, MaxGrantWindowS: 3600,
			Aliases: map[string]CredentialPIVBAlias{
				"ro":     {Target: "ro@example.iam.gserviceaccount.com", AssertionLifetimeS: 900},
				"deploy": {Target: "deploy@example.iam.gserviceaccount.com", AssertionLifetimeS: 60},
			},
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
	// What the provider advertises about windows and assertion reuse has to
	// survive into the persisted claim; a later mint is authorised against the
	// manifest, not against a fresh describe.
	if manifest.MaxGrantWindowS != 3600 || manifest.Aliases["ro"].AssertionLifetimeS != 900 {
		t.Fatalf("manifest dropped the protocol 3 advertisements: %#v", manifest)
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
	// The pane supplies a card, a route context and a grant window; every one
	// of them is the daemon's to stamp, so none may survive to the provider.
	requestBody := `{"version":3,"alias":"ro","external_account_audience":"audience","impersonated_email":"ro@example","request_source":{"kind":"agent-session","label":"codex:project/ro","session_id":"0123456789abcdef0123456789abcdef"},"expected_card":{"serial":0,"jwk_kid":"","spki_der":null},"forward_context":{"origin_node_id":"attacker","workspace_id":"attacker","bundle":"attacker","claim_generation":99,"provider_node_id":"attacker","operation_id":"attacker","window_s":86400,"window_deadline":4102444800}}`
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
	// No claim window is plumbed through yet, so the stamped context carries
	// none; what matters here is that the pane's window did not become one.
	if got.ForwardContext.WindowSeconds != 0 || got.ForwardContext.WindowDeadline != 0 {
		t.Fatalf("pane-supplied grant window survived into the forwarded request: %#v", got.ForwardContext)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Version skew has one remedy — upgrade both sides — and it has to be the one
// the operator reads. Before the version peek, a protocol 2 body failed strict
// decoding first and was reported as a malformed request.
func TestPIVBProxyReportsVersionSkewAsAnUpgradeBeforeProvider(t *testing.T) {
	providerCalled := make(chan struct{}, 1)
	handler := http.NewServeMux()
	handler.HandleFunc("POST /v1/mint", func(http.ResponseWriter, *http.Request) { providerCalled <- struct{}{} })
	var cfg Config
	cfg.Credentials.PIVB.ForwardSocket = serveFakePIVB(t, handler)
	d := &Daemon{config: cfg, state: StateData{Node: Host{ID: strings.Repeat("a", 32)}}, credentialActive: map[string]int{}}
	manifest := &CredentialPIVBManifest{ProtocolVersion: pivbForwardProtocolVersion, Aliases: map[string]CredentialPIVBAlias{"ro": {Target: "ro@example"}}, Card: testPIVBCard(t)}
	client, server := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	go func() {
		done <- d.proxyPIVBMint(context.Background(), server, "workspace", "work", 1, "", strings.Repeat("b", 32), manifest)
	}()
	body := `{"version":2,"alias":"ro","external_account_audience":"audience","impersonated_email":"ro@example","request_source":{"kind":"agent-session","label":"codex:project/ro","session_id":"0123456789abcdef0123456789abcdef"},"retired_v2_field":true}`
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
	var result struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := decodeStrictJSON(responseBody, &result); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest || result.Code != "PIVB_CONFIG" ||
		!strings.Contains(result.Error, "speaks forwarding protocol 2") ||
		!strings.Contains(result.Error, "upgrade PIVB and ZKA together") {
		t.Fatalf("version-skew response = %d %s", resp.StatusCode, responseBody)
	}
	select {
	case <-providerCalled:
		t.Fatal("a skewed mint request reached the PIVB provider")
	default:
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
	body := `{"version":3,"alias":"deploy","external_account_audience":"audience","impersonated_email":"deploy@example","request_source":{"kind":"agent-session","label":"codex:project/deploy","session_id":"0123456789abcdef0123456789abcdef"}}`
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
		GrantedWindowSeconds: 600, GrantedWindowDeadline: 1785586470,
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
	// Binding replaces the forwarded context wholesale, which is why what the
	// provider granted is reported outside it.
	if response.GrantedWindowSeconds != 600 || response.GrantedWindowDeadline != 1785586470 {
		t.Fatalf("binding dropped the granted window: %#v", response)
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
	proxyDone := make(chan struct{})
	go func() {
		proxyRemotePIVBResponse(stream, client, card, trusted)
		_ = client.Close()
		_ = stream.Close()
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

func TestPIVBBindingTransfersWithoutReplacingStableWorkspaceRoute(t *testing.T) {
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
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	var bundle CredentialBundleConfig
	bundle.PIVB.Enable = true
	bundle.PIVB.Aliases = []string{"ro"}
	d.config.Credentials.Bundles = map[string]CredentialBundleConfig{"work": bundle}
	d.config.Credentials.PIVB.ForwardSocket = serveFakePIVB(t, handler)
	workspace := createTestWorkspace(t, d, 1)
	localOwner := readyCredentialAttachment(t, d, workspace, "local-owner", d.state.Node.ID)
	status, err := d.activateLocalCredentialBundle(context.Background(), workspace.ID, "work", false, "", localOwner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Capabilities[credentialCapabilityPIVB].Available != true {
		t.Fatalf("local status = %#v", status)
	}
	d.credentialMu.Lock()
	stableRoute := d.credentialRoutes[credentialRouteKey(workspace.ID, credentialCapabilityPIVB)]
	d.credentialMu.Unlock()
	if stableRoute == nil {
		t.Fatal("local activation reported ready without publishing the stable workspace route")
	}
	d.mu.Lock()
	firstGeneration := d.state.Workspaces[workspace.ID].CredentialClaim.Generation
	d.mu.Unlock()
	if _, err := d.activateLocalCredentialBundle(context.Background(), workspace.ID, "work", false, "", localOwner.ID); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	gotGeneration := d.state.Workspaces[workspace.ID].CredentialClaim.Generation
	d.mu.Unlock()
	if gotGeneration != firstGeneration {
		t.Fatalf("idempotent local activation changed generation from %d to %d", firstGeneration, gotGeneration)
	}
	endpoint, err := d.pivbEndpoint(workspace.ID)
	if err != nil || endpoint.Source != "local" || endpoint.Socket == "" {
		t.Fatalf("endpoint = %#v, %v", endpoint, err)
	}
	attachment := readyCredentialAttachment(t, d, workspace, "remote", "provider")
	readyCredentialTransport(t, d, attachment.Node)
	manifest := &CredentialPIVBManifest{
		ProtocolVersion:  pivbForwardProtocolVersion,
		ProviderResource: "projects/1/locations/global/workloadIdentityPools/pool/providers/provider",
		IssuerURI:        "https://issuer.example",
		Aliases:          map[string]CredentialPIVBAlias{"ro": {Target: "ro@example.iam.gserviceaccount.com"}},
		Card:             card,
	}
	if _, err := d.claimWorkspaceCredentials(context.Background(), workspaceCredentialRequest{
		Workspace: workspace.ID, Provider: attachment.Node, ProviderSource: "remote", Bundle: "work",
		OwnerAttachmentID: attachment.ID,
		Manifest:          credentialBundleManifest{Bundle: "work", PIVB: manifest},
	}); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	remoteProvider := d.state.Workspaces[workspace.ID].CredentialClaim
	d.mu.Unlock()
	if remoteProvider == nil || remoteProvider.ProviderSource != "remote" || remoteProvider.OwnerNodeID != attachment.Node.ID || remoteProvider.Generation <= firstGeneration {
		t.Fatalf("remote provider did not atomically replace local provider: %#v", remoteProvider)
	}
	d.credentialMu.Lock()
	currentRoute := d.credentialRoutes[credentialRouteKey(workspace.ID, credentialCapabilityPIVB)]
	d.credentialMu.Unlock()
	if currentRoute != stableRoute {
		t.Fatal("provider transfer replaced the pane-facing workspace route")
	}
	if noOp, err := d.activateLocalCredentialBundle(context.Background(), workspace.ID, "work", true, "", localOwner.ID); err != nil || noOp.OwnerNode != attachment.Node.ID {
		t.Fatalf("if-unclaimed activation changed or rejected remote route: status=%#v err=%v", noOp, err)
	}
	d.credentialMu.Lock()
	transport := d.credentialTransports[attachment.Node.ID]
	d.credentialMu.Unlock()
	if err := d.setIncomingCredentialTransport(credentialTransportSessionRequest{
		Provider: attachment.Node, State: "disconnected", Endpoint: transport.Endpoint,
	}); err != nil {
		t.Fatal(err)
	}
	endpoint, err = d.pivbEndpoint(workspace.ID)
	if err != nil || endpoint.State != "degraded" || !strings.Contains(endpoint.Detail, "credential transport is unavailable") {
		t.Fatalf("disconnected provider endpoint = %#v, %v", endpoint, err)
	}
	localAgain, err := d.activateLocalCredentialBundle(context.Background(), workspace.ID, "work", false, "", localOwner.ID)
	if err != nil || localAgain.OwnerNode != d.state.Node.ID {
		t.Fatalf("explicit local transfer = %#v, %v", localAgain, err)
	}
	if _, err := d.releaseWorkspaceCredentials(workspace.ID); err != nil {
		t.Fatal(err)
	}
	endpoint, err = d.pivbEndpoint(workspace.ID)
	if err != nil || endpoint.State != "unclaimed" || d.state.Workspaces[workspace.ID].CredentialClaim != nil {
		t.Fatalf("release silently restored a provider: endpoint=%#v claim=%#v err=%v", endpoint, d.state.Workspaces[workspace.ID].CredentialClaim, err)
	}
}

func TestPIVBEndpointReflectsWholeBundleHealthAndLocalRefresh(t *testing.T) {
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
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	var bundle CredentialBundleConfig
	bundle.SSHAgent.Enable = true
	bundle.PIVB.Enable = true
	bundle.PIVB.Aliases = []string{"ro"}
	d.config.Credentials.Bundles = map[string]CredentialBundleConfig{"work": bundle}
	d.config.Credentials.PIVB.ForwardSocket = serveFakePIVB(t, handler)
	workspace := createTestWorkspace(t, d, 1)
	localOwner := readyCredentialAttachment(t, d, workspace, "local-owner", d.state.Node.ID)
	sshPath := filepath.Join(d.paths.RuntimeDir, "local-agent.sock")
	sshAgent, err := listenUnix(sshPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sshAgent.Close() })

	status, err := d.activateLocalCredentialBundle(context.Background(), workspace.ID, "work", false, sshPath, localOwner.ID)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := d.pivbEndpoint(workspace.ID)
	if err != nil || endpoint.State != "ready" {
		t.Fatalf("healthy endpoint = %#v, %v", endpoint, err)
	}

	d.mu.Lock()
	sshCapability := d.state.Workspaces[workspace.ID].CredentialClaim.Capabilities[credentialCapabilitySSH]
	sshCapability.State = "unavailable"
	sshCapability.Available = false
	sshCapability.Detail = "persisted SSH capability failure"
	d.state.Workspaces[workspace.ID].CredentialClaim.Capabilities[credentialCapabilitySSH] = sshCapability
	d.mu.Unlock()
	endpoint, err = d.pivbEndpoint(workspace.ID)
	if err != nil || endpoint.State != "degraded" || !strings.Contains(endpoint.Detail, "persisted SSH capability failure") {
		t.Fatalf("unavailable bundle capability endpoint = %#v, %v", endpoint, err)
	}
	d.mu.Lock()
	sshCapability.State = "ready"
	sshCapability.Available = true
	sshCapability.Detail = ""
	d.state.Workspaces[workspace.ID].CredentialClaim.Capabilities[credentialCapabilitySSH] = sshCapability
	d.mu.Unlock()

	d.credentialMu.Lock()
	delete(d.credentialSSHSources, credentialSSHSourceKey("", workspace.ID, status.Generation))
	d.credentialMu.Unlock()
	endpoint, err = d.pivbEndpoint(workspace.ID)
	if err != nil || endpoint.State != "degraded" || endpoint.Source != "local" ||
		!strings.Contains(endpoint.Detail, "ssh-agent: local credential source is unavailable") {
		t.Fatalf("missing SSH source endpoint = %#v, %v", endpoint, err)
	}

	refreshed, err := d.activateLocalCredentialBundle(context.Background(), workspace.ID, "work", false, sshPath, localOwner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Generation != status.Generation {
		t.Fatalf("local refresh changed generation from %d to %d", status.Generation, refreshed.Generation)
	}
	endpoint, err = d.pivbEndpoint(workspace.ID)
	if err != nil || endpoint.State != "ready" {
		t.Fatalf("refreshed endpoint = %#v, %v", endpoint, err)
	}
}

func TestPIVBEndpointRejectsBundleWithoutPIVB(t *testing.T) {
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	d.config.Credentials.Bundles = map[string]CredentialBundleConfig{"work": sshCredentialBundle()}
	workspace := createTestWorkspace(t, d, 1)
	localOwner := readyCredentialAttachment(t, d, workspace, "local-owner", d.state.Node.ID)
	sshPath := filepath.Join(d.paths.RuntimeDir, "local-agent.sock")
	sshAgent, err := listenUnix(sshPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sshAgent.Close() })
	if _, err := d.activateLocalCredentialBundle(context.Background(), workspace.ID, "work", false, sshPath, localOwner.ID); err != nil {
		t.Fatal(err)
	}
	endpoint, err := d.pivbEndpoint(workspace.ID)
	if err != nil || endpoint.State != "degraded" || !strings.Contains(endpoint.Detail, "does not enable PIVB") {
		t.Fatalf("SSH-only endpoint = %#v, %v", endpoint, err)
	}
}

func TestCredentialTransportDegradationDoesNotAffectLocalBundle(t *testing.T) {
	status := workspaceCredentialStatus{
		State: "ready",
		Capabilities: map[string]credentialCapabilityView{
			credentialCapabilitySSH:  {State: "ready", Available: true},
			credentialCapabilityPIVB: {State: "ready", Available: true, Detail: "local YubiKey"},
		},
	}
	workspace := &Workspace{CredentialClaim: &CredentialClaim{ProviderSource: "local", State: "ready"}}
	degradeCredentialStatus(&status, workspace)
	if !status.Capabilities[credentialCapabilitySSH].Available {
		t.Fatal("remote transport degradation blanked local SSH")
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
	d, err := newTestDaemon(t, testRoot(t), quietRunner())
	if err != nil {
		t.Fatal(err)
	}
	var bundle CredentialBundleConfig
	bundle.PIVB.Enable = true
	bundle.PIVB.Aliases = []string{"ro"}
	d.config.Credentials.Bundles = map[string]CredentialBundleConfig{"work": bundle}
	d.config.Credentials.PIVB.ForwardSocket = serveFakePIVB(t, handler)
	workspace := createTestWorkspace(t, d, 1)
	localOwner := readyCredentialAttachment(t, d, workspace, "local-owner", d.state.Node.ID)
	blocker, err := listenUnix(pivbRelaySocketPath(d.paths, workspace.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()

	if _, err := d.activateLocalCredentialBundle(context.Background(), workspace.ID, "work", false, "", localOwner.ID); err == nil {
		t.Fatal("local activation reported success while another live listener owned the route")
	}
	d.mu.Lock()
	claim := d.state.Workspaces[workspace.ID].CredentialClaim
	d.mu.Unlock()
	if claim != nil {
		t.Fatalf("listener failure published a partial credential claim: %#v", claim)
	}
	endpoint, err := d.pivbEndpoint(workspace.ID)
	if err != nil || endpoint.State != "unclaimed" {
		t.Fatalf("unclaimed endpoint = %#v, %v", endpoint, err)
	}
}
