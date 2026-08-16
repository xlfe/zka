package zka

import (
	"bufio"
	"bytes"
	"context"
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
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	pivbForwardProtocolVersion = 3
	pivbForwardBodyMax         = 16 << 10
	pivbForwardResponseMax     = 96 << 10
)

var (
	errPIVBRemoteResponseTransport = errors.New("transport")
	errPIVBRemoteResponseBinding   = errors.New("binding")
)

type pivbForwardDescription struct {
	Version          int                            `json:"version"`
	ProviderResource string                         `json:"provider_resource"`
	IssuerURI        string                         `json:"issuer_uri"`
	Aliases          map[string]CredentialPIVBAlias `json:"aliases"`
	Card             CredentialPIVBCard             `json:"card"`
	MaxGrantWindowS  int64                          `json:"max_grant_window_s,omitempty"`
}

type pivbEnrolledKey struct {
	Serial uint32 `json:"serial"`
	KeyID  string `json:"jwk_kid"`
}

type pivbForwardPolicy struct {
	Version          int                            `json:"version"`
	ProviderResource string                         `json:"provider_resource"`
	IssuerURI        string                         `json:"issuer_uri"`
	Aliases          map[string]CredentialPIVBAlias `json:"aliases"`
	EnrolledKeys     []pivbEnrolledKey              `json:"enrolled_keys"`
	// MaxGrantWindowS is the longest authorisation window the provider will
	// grant a claim. Zero means the provider grants no windows at all.
	MaxGrantWindowS int64 `json:"max_grant_window_s,omitempty"`
}

type pivbForwardSource struct {
	Kind      string `json:"kind"`
	Label     string `json:"label,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

type pivbForwardContext struct {
	OriginNodeID     string `json:"origin_node_id"`
	WorkspaceID      string `json:"workspace_id"`
	Bundle           string `json:"bundle"`
	ClaimGeneration  uint64 `json:"claim_generation"`
	ProviderNodeID   string `json:"provider_node_id"`
	ProviderAttachID string `json:"provider_attachment_id,omitempty"`
	OperationID      string `json:"operation_id"`
	// WindowSeconds is the authorisation window the claim asks the mint to be
	// covered by, and WindowDeadline is the absolute unix second the claim
	// anchored it to. Both are stamped by this daemon from claim state, never
	// accepted from the pane, and travel together or not at all.
	WindowSeconds  int64 `json:"window_s,omitempty"`
	WindowDeadline int64 `json:"window_deadline,omitempty"`
}

type pivbMintRequest struct {
	Version                 int                `json:"version"`
	Alias                   string             `json:"alias"`
	ExternalAccountAudience string             `json:"external_account_audience"`
	ImpersonatedEmail       string             `json:"impersonated_email"`
	RequestSource           *pivbForwardSource `json:"request_source,omitempty"`
	ExpectedCard            CredentialPIVBCard `json:"expected_card"`
	ForwardContext          pivbForwardContext `json:"forward_context"`
}

type pivbMintResponse struct {
	Version        int                `json:"version"`
	IDToken        string             `json:"id_token"`
	ExpirationTime int64              `json:"expiration_time"`
	Card           CredentialPIVBCard `json:"card"`
	ExpectedCard   CredentialPIVBCard `json:"expected_card"`
	ForwardContext pivbForwardContext `json:"forward_context"`
	// The granted window sits outside ForwardContext because binding a
	// response to the active route replaces the forwarded context wholesale;
	// what the provider granted has to survive that rewrite.
	GrantedWindowSeconds  int64 `json:"granted_window_s,omitempty"`
	GrantedWindowDeadline int64 `json:"granted_window_deadline,omitempty"`
}

// pivbInvalidateRequest retires the reuse state pivbd holds for a claim.
// ClaimGeneration zero means every generation of the workspace, which is what
// a release asks for; a non-zero generation purges everything before it and
// leaves that generation's own grant intact.
type pivbInvalidateRequest struct {
	Version         int    `json:"version"`
	WorkspaceID     string `json:"workspace_id"`
	ClaimGeneration uint64 `json:"claim_generation"`
}

type pivbInvalidateResponse struct {
	Version int `json:"version"`
	Purged  int `json:"purged"`
}

func credentialPIVBForwardSocket(cfg Config) string {
	if cfg.Credentials.PIVB.ForwardSocket != "" {
		return cfg.Credentials.PIVB.ForwardSocket
	}
	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
		return filepath.Join(runtimeDir, "pivb", "forward.sock")
	}
	return ""
}

func pivbRelaySocketPath(paths Paths, workspaceID string) string {
	return agentRelaySocketPath(filepath.Join(paths.RuntimeDir, "pivb"), workspaceID)
}

func newPIVBHTTPClient(socket string, timeout time.Duration) *http.Client {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	return &http.Client{Transport: transport, Timeout: timeout}
}

func buildPIVBManifest(ctx context.Context, cfg Config, aliases []string) (*CredentialPIVBManifest, error) {
	socket := credentialPIVBForwardSocket(cfg)
	if socket == "" || !filepath.IsAbs(socket) {
		return nil, errors.New("PIVB forwarding socket is not configured as an absolute path")
	}
	client := newPIVBHTTPClient(socket, 25*time.Second)
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://pivb-forward/v1/describe", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, describePIVBError("describe PIVB provider", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, pivbForwardResponseMax+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, describePIVBError("describe PIVB provider", errors.New(strings.TrimSpace(string(body))))
	}
	var description pivbForwardDescription
	if err := decodeStrictJSON(body, &description); err != nil {
		return nil, fmt.Errorf("decode PIVB provider description: %w", err)
	}
	if description.Version != pivbForwardProtocolVersion {
		return nil, errors.New(pivbForwardVersionSkew("PIVB provider", description.Version))
	}
	if description.ProviderResource == "" || description.IssuerURI == "" {
		return nil, errors.New("PIVB provider description is incomplete")
	}
	if err := validatePIVBCard(description.Card); err != nil {
		return nil, fmt.Errorf("PIVB provider card: %w", err)
	}
	allowed := make(map[string]CredentialPIVBAlias, len(aliases))
	for _, alias := range aliases {
		binding, ok := description.Aliases[alias]
		if !ok || binding.Target == "" {
			return nil, fmt.Errorf("PIVB provider does not configure allowed alias %q", alias)
		}
		allowed[alias] = binding
	}
	return &CredentialPIVBManifest{
		ProtocolVersion: description.Version, ProviderResource: description.ProviderResource,
		IssuerURI: description.IssuerURI, Aliases: allowed, Card: description.Card,
		MaxGrantWindowS: description.MaxGrantWindowS,
	}, nil
}

// invalidatePIVBReuse tells the local pivbd to stop reusing a claim's
// authorisation. It is deliberately advisory and asynchronous: by the time it
// runs the claim change is already durable, so a pivbd that is absent, older,
// or failing must cost the caller nothing beyond a journal line. The worst
// case is a grant that outlives its claim inside pivbd until it expires on its
// own, which is why the caller never waits on this and never fails for it.
func (d *Daemon) invalidatePIVBReuse(workspaceID string, generation uint64) {
	socket := credentialPIVBForwardSocket(d.config)
	if workspaceID == "" || socket == "" || !filepath.IsAbs(socket) {
		return
	}
	// A node with no pivbd holds no reuse state to retire, and claims there are
	// routine. Skip it silently so an SSH-only or OpenPGP-only deployment does
	// not journal a PIVB failure for every claim it makes; a socket that is
	// present but dead is a real fault and still reports below.
	if info, err := os.Lstat(socket); err != nil || info.Mode()&os.ModeSocket == 0 {
		return
	}
	d.startWorker(func(ctx context.Context) {
		callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		purged, err := postPIVBInvalidate(callCtx, socket, workspaceID, generation)
		if err != nil {
			d.logger.Printf("PIVB reuse invalidation failed workspace=%s generation=%d: %v", workspaceID, generation, err)
			return
		}
		d.logger.Printf("PIVB reuse invalidated workspace=%s generation=%d purged=%d", workspaceID, generation, purged)
	})
}

func postPIVBInvalidate(ctx context.Context, socket, workspaceID string, generation uint64) (int, error) {
	encoded, err := json.Marshal(pivbInvalidateRequest{
		Version: pivbForwardProtocolVersion, WorkspaceID: workspaceID, ClaimGeneration: generation,
	})
	if err != nil {
		return 0, err
	}
	client := newPIVBHTTPClient(socket, 5*time.Second)
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://pivb-forward/v1/invalidate", bytes.NewReader(encoded))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, pivbForwardResponseMax+1))
	if err != nil || len(body) > pivbForwardResponseMax {
		return 0, errors.New("PIVB invalidate response is invalid")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, errors.New(strings.TrimSpace(string(body)))
	}
	var decoded pivbInvalidateResponse
	if err := decodeStrictJSON(body, &decoded); err != nil {
		return 0, fmt.Errorf("decode PIVB invalidate response: %w", err)
	}
	if decoded.Version != pivbForwardProtocolVersion {
		return 0, errors.New(pivbForwardVersionSkew("PIVB invalidate response", decoded.Version))
	}
	return decoded.Purged, nil
}

func validatePIVBClaimAgainstLocalPolicy(ctx context.Context, cfg Config, manifest *CredentialPIVBManifest) error {
	socket := credentialPIVBForwardSocket(cfg)
	if socket == "" || !filepath.IsAbs(socket) {
		return errors.New("origin PIVB forwarding socket is not configured as an absolute path")
	}
	client := newPIVBHTTPClient(socket, 25*time.Second)
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://pivb-forward/v1/policy", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("read origin PIVB policy: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, pivbForwardResponseMax+1))
	if err != nil || len(body) > pivbForwardResponseMax {
		return errors.New("origin PIVB policy response is invalid")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("read origin PIVB policy: %s", strings.TrimSpace(string(body)))
	}
	var policy pivbForwardPolicy
	if err := decodeStrictJSON(body, &policy); err != nil {
		return fmt.Errorf("decode origin PIVB policy: %w", err)
	}
	if policy.Version != pivbForwardProtocolVersion {
		return errors.New(pivbForwardVersionSkew("origin PIVB provider", policy.Version))
	}
	if policy.ProviderResource != manifest.ProviderResource || policy.IssuerURI != manifest.IssuerURI {
		return errors.New("remote PIVB provider or issuer does not match the origin PIVB configuration")
	}
	for alias, binding := range manifest.Aliases {
		if local, ok := policy.Aliases[alias]; !ok || local.Target != binding.Target {
			return fmt.Errorf("remote PIVB alias %q target does not match the origin PIVB configuration", alias)
		}
	}
	for _, key := range policy.EnrolledKeys {
		if key.Serial == manifest.Card.Serial && key.KeyID == manifest.Card.KeyID {
			return nil
		}
	}
	return fmt.Errorf("remote PIVB card %d/%s is not enrolled on the origin", manifest.Card.Serial, manifest.Card.KeyID)
}

func describePIVBError(action string, err error) error {
	message := err.Error()
	if strings.Contains(message, "cooperative smart-card lease") || strings.Contains(message, "context deadline") || strings.Contains(message, "Client.Timeout") {
		return fmt.Errorf("%s: PIVB provider smart-card lease is busy or unavailable; retry after the current OpenPGP/PIVB operation: %w", action, err)
	}
	return fmt.Errorf("%s: %w", action, err)
}

func validatePIVBCard(card CredentialPIVBCard) error {
	if card.Serial == 0 || card.KeyID == "" || len(card.SPKIDER) == 0 {
		return errors.New("identity is incomplete")
	}
	parsed, err := x509.ParsePKIXPublicKey(card.SPKIDER)
	if err != nil {
		return fmt.Errorf("parse SPKI: %w", err)
	}
	pub, ok := parsed.(*rsa.PublicKey)
	if !ok || pub.N.BitLen() != 2048 || pub.E != 65537 {
		return errors.New("slot 9c key is not RSA-2048/F4")
	}
	digest := sha256.Sum256(card.SPKIDER)
	if derived := base64.RawURLEncoding.EncodeToString(digest[:]); derived != card.KeyID {
		return fmt.Errorf("SPKI derives key id %q, not %q", derived, card.KeyID)
	}
	return nil
}

// peekPIVBForwardVersion reads only the protocol version out of an already
// captured forwarded document. It is deliberately tolerant of everything else:
// a skewed peer is diagnosed by its version, before strict decoding rejects
// the unknown fields that skew introduces.
func peekPIVBForwardVersion(raw []byte) (int, bool) {
	var peek struct {
		Version *int `json:"version"`
	}
	if err := json.Unmarshal(raw, &peek); err != nil || peek.Version == nil {
		return 0, false
	}
	return *peek.Version, true
}

func pivbForwardVersionSkew(subject string, version int) string {
	return fmt.Sprintf("%s speaks forwarding protocol %d, not %d; upgrade PIVB and ZKA together", subject, version, pivbForwardProtocolVersion)
}

func decodeStrictJSON(raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return errors.New("multiple JSON values")
	}
	return nil
}

func clonePIVBManifest(manifest *CredentialPIVBManifest) *CredentialPIVBManifest {
	if manifest == nil {
		return nil
	}
	raw, _ := json.Marshal(manifest)
	var clone CredentialPIVBManifest
	_ = json.Unmarshal(raw, &clone)
	return &clone
}

func samePIVBPolicy(bundle CredentialBundleConfig, manifest *CredentialPIVBManifest) bool {
	if !bundle.PIVB.Enable || manifest == nil || manifest.ProtocolVersion != pivbForwardProtocolVersion ||
		manifest.ProviderResource == "" || manifest.IssuerURI == "" {
		return false
	}
	want := append([]string(nil), bundle.PIVB.Aliases...)
	got := make([]string, 0, len(manifest.Aliases))
	for alias := range manifest.Aliases {
		if manifest.Aliases[alias].Target == "" {
			return false
		}
		got = append(got, alias)
	}
	sort.Strings(want)
	sort.Strings(got)
	return sameStringSet(want, got) && validatePIVBCard(manifest.Card) == nil
}

// proxyPIVBMint is ZKA's semantic adapter. It accepts exactly one mint
// request, enforces the bundle alias allowlist, injects authenticated route
// and card identity, and forwards it to the local networkless pivbd.
func (d *Daemon) proxyPIVBMint(ctx context.Context, stream net.Conn, workspace, bundleName string, generation uint64, ownerAttachment, originNode string, manifest *CredentialPIVBManifest) error {
	if manifest == nil {
		return errors.New("PIVB claim has no manifest")
	}
	stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer stop()
	_ = stream.SetDeadline(deadlineOr(ctx, time.Now().Add(26*time.Second)))
	request, err := http.ReadRequest(bufio.NewReader(io.LimitReader(stream, pivbForwardBodyMax+8<<10)))
	if err != nil {
		return fmt.Errorf("read PIVB mint request: %w", err)
	}
	defer request.Body.Close()
	if request.Method != http.MethodPost || request.URL.Path != "/v1/mint" {
		return writePIVBProxyError(stream, http.StatusForbidden, "PIVB_CONFIG", "ZKA PIVB routes accept only POST /v1/mint")
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, pivbForwardBodyMax+1))
	if err != nil || len(body) > pivbForwardBodyMax {
		return writePIVBProxyError(stream, http.StatusBadRequest, "PIVB_CONFIG", "PIVB mint request exceeds the fixed size limit")
	}
	// A caller one protocol behind sends fields this build has never heard of,
	// so strict decoding would report a malformed request and send an operator
	// hunting the request shape instead of the version skew. Read the version
	// out of the captured bytes first; the body is never read twice.
	if version, ok := peekPIVBForwardVersion(body); ok && version != pivbForwardProtocolVersion {
		return writePIVBProxyError(stream, http.StatusBadRequest, "PIVB_CONFIG", pivbForwardVersionSkew("PIVB mint request", version))
	}
	var mint pivbMintRequest
	if err := decodeStrictJSON(body, &mint); err != nil {
		return writePIVBProxyError(stream, http.StatusBadRequest, "PIVB_CONFIG", "invalid PIVB mint request: "+err.Error())
	}
	if mint.Version != pivbForwardProtocolVersion {
		return writePIVBProxyError(stream, http.StatusBadRequest, "PIVB_CONFIG", pivbForwardVersionSkew("PIVB mint request", mint.Version))
	}
	if mint.RequestSource == nil {
		return writePIVBProxyError(stream, http.StatusBadRequest, "PIVB_CONFIG", "PIVB mint request has no request source")
	}
	if _, ok := manifest.Aliases[mint.Alias]; !ok {
		return writePIVBProxyError(stream, http.StatusForbidden, "PIVB_CONFIG", fmt.Sprintf("PIVB alias %q is not allowed by bundle %q", mint.Alias, bundleName))
	}
	operationID, err := randomID()
	if err != nil {
		return err
	}
	mint.ExpectedCard = manifest.Card
	d.mu.Lock()
	providerNode := d.state.Node.ID
	d.mu.Unlock()
	mint.ForwardContext = pivbForwardContext{
		OriginNodeID: originNode, WorkspaceID: workspace, Bundle: bundleName,
		ClaimGeneration: generation, ProviderNodeID: providerNode,
		ProviderAttachID: ownerAttachment, OperationID: operationID,
	}
	encoded, err := json.Marshal(mint)
	if err != nil {
		return err
	}
	upstreamURL := &url.URL{Scheme: "http", Host: "pivb-forward", Path: "/v1/mint"}
	upstreamReq := &http.Request{Method: http.MethodPost, URL: upstreamURL, Host: "pivb-forward", Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(encoded)), ContentLength: int64(len(encoded))}
	operationCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// One route connection carries exactly one mint. Once its client closes
	// the connection (session teardown, release, or generation change), cancel
	// the provider-side HTTP request even if it is waiting on touch.
	go func() {
		var one [1]byte
		_, _ = stream.Read(one[:])
		cancel()
	}()
	upstreamReq = upstreamReq.WithContext(operationCtx)
	upstreamReq.Header.Set("Content-Type", "application/json")
	client := newPIVBHTTPClient(credentialPIVBForwardSocket(d.config), 25*time.Second)
	defer client.CloseIdleConnections()
	finish := d.beginCredentialOperation(workspace, credentialCapabilityPIVB, "mint")
	defer finish()
	resp, err := client.Do(upstreamReq)
	if err != nil {
		return writePIVBProxyError(stream, http.StatusBadGateway, "PIVB_UNAVAILABLE", "PIVB provider is unavailable: "+err.Error())
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, pivbForwardResponseMax+1))
	if err != nil || len(responseBody) > pivbForwardResponseMax {
		return writePIVBProxyError(stream, http.StatusBadGateway, "PIVB_INTERNAL", "PIVB provider returned an invalid response")
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		responseBody, err = bindPIVBMintResponse(responseBody, manifest.Card, mint.ForwardContext)
		if err != nil {
			return writePIVBProxyError(stream, http.StatusBadGateway, "PIVB_INTERNAL", "PIVB provider returned an invalid response: "+err.Error())
		}
		d.logger.Printf("PIVB route mint succeeded attachment_mode=route-required protocol=%d route=local workspace=%s bundle=%s generation=%d provider_node=%s provider_attachment=%s operation=%s alias=%s",
			managedPIVBAttachmentProtocol(d.config), workspace, bundleName, generation, providerNode, ownerAttachment, operationID, mint.Alias)
	}
	response := &http.Response{StatusCode: resp.StatusCode, Status: resp.Status, ProtoMajor: 1, ProtoMinor: 1, Header: resp.Header.Clone(), Body: io.NopCloser(bytes.NewReader(responseBody)), ContentLength: int64(len(responseBody))}
	return response.Write(stream)
}

func bindPIVBMintResponse(raw []byte, expected CredentialPIVBCard, trusted pivbForwardContext) ([]byte, error) {
	var response pivbMintResponse
	if err := decodeStrictJSON(raw, &response); err != nil {
		return nil, err
	}
	if response.Version != pivbForwardProtocolVersion {
		return nil, errors.New(pivbForwardVersionSkew("PIVB mint response", response.Version))
	}
	if response.IDToken == "" || response.ExpirationTime <= 0 ||
		response.Card.Serial == 0 || response.Card.KeyID == "" || len(response.Card.SPKIDER) == 0 {
		return nil, errors.New("incomplete PIVB mint response")
	}
	if trusted.OperationID == "" {
		trusted.OperationID = response.ForwardContext.OperationID
	}
	if validateRemoteWorkspaceID(trusted.OperationID) != nil {
		return nil, errors.New("PIVB mint response has an invalid operation id")
	}
	response.ExpectedCard = expected
	response.ForwardContext = trusted
	return json.Marshal(response)
}

func proxyBoundPIVBResponse(stream net.Conn, client net.Conn, expected CredentialPIVBCard, trusted pivbForwardContext) (pivbForwardContext, bool, error) {
	resp, err := http.ReadResponse(bufio.NewReader(io.LimitReader(stream, pivbForwardResponseMax+8<<10)), nil)
	if err != nil {
		return trusted, false, fmt.Errorf("%w: read remote PIVB response: %w", errPIVBRemoteResponseTransport, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, pivbForwardResponseMax+1))
	if err != nil || len(body) > pivbForwardResponseMax {
		return trusted, false, fmt.Errorf("%w: remote PIVB response exceeds the fixed size limit", errPIVBRemoteResponseTransport)
	}
	succeeded := resp.StatusCode >= 200 && resp.StatusCode < 300
	if succeeded {
		body, err = bindPIVBMintResponse(body, expected, trusted)
		if err != nil {
			return trusted, false, fmt.Errorf("%w: bind remote PIVB response to active route: %w", errPIVBRemoteResponseBinding, err)
		}
		var rebound pivbMintResponse
		if err := decodeStrictJSON(body, &rebound); err != nil {
			return trusted, false, fmt.Errorf("%w: decode rebound PIVB response: %w", errPIVBRemoteResponseBinding, err)
		}
		trusted = rebound.ForwardContext
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	if err := resp.Write(client); err != nil {
		return trusted, false, fmt.Errorf("%w: write remote PIVB response: %w", errPIVBRemoteResponseTransport, err)
	}
	return trusted, succeeded, nil
}

func writePIVBProxyError(conn io.Writer, status int, code, message string) error {
	body, _ := json.Marshal(map[string]string{"error": message, "code": code})
	response := &http.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), ProtoMajor: 1, ProtoMinor: 1, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}
	response.Header.Set("Content-Type", "application/json")
	return response.Write(conn)
}

func deadlineOr(ctx context.Context, fallback time.Time) time.Time {
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(fallback) {
		return deadline
	}
	return fallback
}
