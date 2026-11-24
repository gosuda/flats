package portal

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/gosuda/portal-tunnel/v2/sdk"
)

// The seams of docs/spec/portal-sign-in.md, "Completion criteria", for the tests of this package and of package
// portal_test: a Net whose exposures are loopback listeners, an OpenID Connect provider on a loopback http server
// (which the SDK accepts as an issuer; only package core refuses http), and a visitor that keeps the cookies of the
// gate. No test needs the network.

// NewTestNet returns a Net whose exposures are loopback listeners that the real sdk.RunHTTP serves.
func NewTestNet(t *testing.T, cfg Config) (*Net, *TestExposures) {
	t.Helper()
	n, ff := newTestNet(t, cfg)
	return n, &TestExposures{ff}
}

// SetLeaseTTL shortens the time after which a failed unregistration counts as confirmed: the relay has dropped the lease. Call
// it before the Net serves.
func (n *Net) SetLeaseTTL(d time.Duration) { n.leaseTTL = d }

// TestExposures are the exposures that a test Net made.
type TestExposures struct{ ff *fakeFactory }

// Count is how many exposures the Net made.
func (x *TestExposures) Count() int { return x.ff.count() }

// Last is the latest exposure.
func (x *TestExposures) Last(t *testing.T) *TestExposure {
	t.Helper()
	return &TestExposure{x.ff.last(t)}
}

// Of is the latest exposure of the identity of slug.
func (x *TestExposures) Of(t *testing.T, slug string) *TestExposure {
	t.Helper()
	x.ff.mu.Lock()
	defer x.ff.mu.Unlock()
	for i := len(x.ff.exps) - 1; i >= 0; i-- {
		if x.ff.exps[i].id.Name == slug {
			return &TestExposure{x.ff.exps[i]}
		}
	}
	t.Fatalf("no exposure of %s", slug)
	return nil
}

// TestExposure is one exposure of a test Net.
type TestExposure struct{ f *fakeExposure }

// Addr is the loopback address that serves the exposure.
func (x *TestExposure) Addr() string { return x.f.Addr().String() }

// SetRelays replaces what the exposure reports.
func (x *TestExposure) SetRelays(sts ...sdk.RelayStatus) { x.f.set(sts...) }

// FailClose makes Close return err, as a relay that refuses the unregistration does, until it is called with nil.
func (x *TestExposure) FailClose(err error) {
	x.f.mu.Lock()
	x.f.closeErr = err
	x.f.mu.Unlock()
}

// Closes counts the Close calls, each of which unregisters the exposure.
func (x *TestExposure) Closes() int { return x.f.closeCount() }

// ReadyRelay is a ready relay that names the canonical host name host.
func ReadyRelay(relay, host string) sdk.RelayStatus {
	return sdk.RelayStatus{RelayURL: "https://" + relay, PublicURL: "https://" + host, State: sdk.RelayReady}
}

// SetCheckTimeout shortens the background check of rule 37 for a test.
func SetCheckTimeout(t *testing.T, d time.Duration) {
	old := checkTimeout
	checkTimeout = d
	t.Cleanup(func() { checkTimeout = old })
}

// TestIdP is an OpenID Connect provider that signs ID tokens with go-jose. Code registers what the token endpoint
// answers for one authorization code.
type TestIdP struct {
	URL      string
	ClientID string

	signer jose.Signer
	jwk    jose.JSONWebKey

	mu      sync.Mutex
	codes   map[string]jwt.Claims
	extra   map[string]map[string]any
	secrets []string // the client secret of each token request, "" for none
}

// NewTestIdP starts a provider for the client ID clientID.
func NewTestIdP(t *testing.T, clientID string) *TestIdP {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	p := &TestIdP{ClientID: clientID, signer: signer, jwk: jose.JSONWebKey{Key: &key.PublicKey, KeyID: "k1", Algorithm: string(jose.ES256), Use: "sig"},
		codes: map[string]jwt.Claims{}, extra: map[string]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(srv.Close)
	p.URL = srv.URL
	return p
}

func (p *TestIdP) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": p.URL, "authorization_endpoint": p.URL + "/authorize",
			"token_endpoint": p.URL + "/token", "jwks_uri": p.URL + "/jwks"})
	case "/jwks":
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{p.jwk}})
	case "/token":
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		secret := r.PostForm.Get("client_secret")
		if _, s, ok := r.BasicAuth(); ok {
			secret = s
		}
		p.mu.Lock()
		p.secrets = append(p.secrets, secret)
		claims, ok := p.codes[r.PostForm.Get("code")]
		extra := p.extra[r.PostForm.Get("code")]
		delete(p.codes, r.PostForm.Get("code"))
		p.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		token, err := jwt.Signed(p.signer).Claims(claims).Claims(extra).Serialize()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": token})
	default:
		http.NotFound(w, r)
	}
}

// Code registers an authorization code whose ID token names sub and, when email is not empty, a verified email.
func (p *TestIdP) Code(sub, email, nonce string) string {
	code := rand.Text()
	now := time.Now()
	extra := map[string]any{"nonce": nonce}
	if email != "" {
		extra["email"], extra["email_verified"] = email, true
	}
	p.mu.Lock()
	p.codes[code] = jwt.Claims{Issuer: p.URL, Subject: sub, Audience: jwt.Audience{p.ClientID}, IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(time.Hour))}
	p.extra[code] = extra
	p.mu.Unlock()
	return code
}

// Secrets lists the client secret of each token request so far.
func (p *TestIdP) Secrets() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.secrets...)
}

// TestVisitor is a browser on Host that talks to the exposure at Addr and keeps the cookies of the gate.
type TestVisitor struct {
	Addr, Host string
	Cookies    map[string]*http.Cookie
}

// NewTestVisitor is a visitor without cookies.
func NewTestVisitor(addr, host string) *TestVisitor {
	return &TestVisitor{Addr: addr, Host: host, Cookies: map[string]*http.Cookie{}}
}

var noRedirect = &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// Do sends one request and keeps the cookies that it sets. It follows no redirect.
func (v *TestVisitor) Do(t *testing.T, method, path string, body []byte, header http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, "http://"+v.Addr+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = v.Host
	for k, vs := range header {
		req.Header[k] = vs
	}
	for _, c := range v.Cookies {
		req.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range resp.Cookies() {
		if c.MaxAge < 0 || c.Value == "" {
			delete(v.Cookies, c.Name)
		} else {
			v.Cookies[c.Name] = c
		}
	}
	return resp
}

// Get sends a GET and returns its status and body.
func (v *TestVisitor) Get(t *testing.T, path string, header http.Header) (int, string) {
	t.Helper()
	resp := v.Do(t, http.MethodGet, path, nil, header)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// OIDCLogin signs the visitor in at idp as sub with email, through the login and the callback of the gate.
func (v *TestVisitor) OIDCLogin(t *testing.T, idp *TestIdP, sub, email string) {
	t.Helper()
	resp := v.Do(t, http.MethodGet, "/_portal/auth/login?next=%2F", nil, nil)
	resp.Body.Close()
	auth, err := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusSeeOther || err != nil || !strings.HasPrefix(auth.String(), idp.URL+"/authorize") {
		t.Fatalf("login = %d to %q, want a redirect to the provider", resp.StatusCode, resp.Header.Get("Location"))
	}
	q := auth.Query()
	code := idp.Code(sub, email, q.Get("nonce"))
	resp = v.Do(t, http.MethodGet, "/_portal/auth/callback?state="+url.QueryEscape(q.Get("state"))+"&code="+url.QueryEscape(code), nil, nil)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || v.Cookies["__Host-portal_access"] == nil {
		t.Fatalf("callback = %d %s, want a session", resp.StatusCode, b)
	}
}

// Redeem exchanges a credential for a session, as the page of an access link does.
func (v *TestVisitor) Redeem(t *testing.T, credential string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"credential": credential})
	resp := v.Do(t, http.MethodPost, "/_portal/auth/redeem", body, http.Header{"Origin": {"https://" + v.Host}, "Content-Type": {"application/json"}})
	resp.Body.Close()
	return resp.StatusCode
}

// CredentialOf returns the credential in the fragment of an access link.
func CredentialOf(t *testing.T, link string) string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	q, err := url.ParseQuery(u.Fragment)
	if err != nil || q.Get("credential") == "" {
		t.Fatalf("link %q has no credential in its fragment", link)
	}
	return q.Get("credential")
}
