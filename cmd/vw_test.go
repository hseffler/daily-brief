package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The VW source is tested against a fake identity service and a fake EU Data
// Act portal (both httptest TLS servers, so the https-only login redirect
// policy is exercised as in production). No test touches the real VW services.

const (
	testVWUser     = "id3@example.com"
	testVWPassword = "s3cret-password"
	testVWVIN      = "WVWZZZ1KZ6W123456"
	testVWGolfVIN  = "WVWZZZ9ZZ9X654321"

	vwTestIdentifierForm = "/signin-service/v1/signin/test@apps_vw-dilab_com/login/identifier"
	vwTestAuthForm       = "/signin-service/v1/signin/test@apps_vw-dilab_com/login/authenticate"
)

// vwFakeWorld bundles a fake VW identity service and a fake EU Data Act
// portal behind behavior knobs.
type vwFakeWorld struct {
	identity *httptest.Server
	portal   *httptest.Server

	marketingConsent atomic.Bool // interject the optional marketing consent page
	consentPending   atomic.Bool // password step lands on a signin-service page
	identifier404    atomic.Bool // metadata endpoint answers 404
	noDatasets       atomic.Bool // dataset list answers 404
	expireOnce       atomic.Bool // data endpoints answer 401 once (session expired)
	wrapVehicles     atomic.Bool // answer the vehicle list wrapped in {"vehicles": ...}

	datasets    map[string][]byte // dataset zip bytes by file name
	datasetList []vwDataset

	logins atomic.Int32 // completed logins (portal callback hits)
}

func newVWFakeWorld(t *testing.T) *vwFakeWorld {
	t.Helper()
	w := &vwFakeWorld{datasets: make(map[string][]byte)}
	w.portal = httptest.NewTLSServer(w.portalHandler())
	w.identity = httptest.NewTLSServer(w.identityHandler())
	t.Cleanup(w.portal.Close)
	t.Cleanup(w.identity.Close)
	return w
}

// httpClient returns a client that trusts the test servers' certificates.
func (w *vwFakeWorld) httpClient() *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
}

// newClient returns a portal client pointed at the fake servers.
func (w *vwFakeWorld) newClient(t *testing.T, vin string) *vwPortalClient {
	t.Helper()
	c, err := newVWPortalClient("Volkswagen", testVWUser, testVWPassword, vin,
		w.portal.URL, w.identity.URL, 30, w.httpClient(), testLogger())
	if err != nil {
		t.Fatalf("newVWPortalClient: %v", err)
	}
	return c
}

// addDataset registers a dataset (zip with an ID.3-style JSON document) and
// its list entry.
func (w *vwFakeWorld) addDataset(t *testing.T, name string, createdOn time.Time, vin string, points []vwDataPoint) {
	t.Helper()
	doc, err := json.Marshal(map[string]any{"vin": vin, "Data": points})
	if err != nil {
		t.Fatal(err)
	}
	w.datasets[name] = vwTestZipRaw(t, "dataset.json", string(doc))
	w.datasetList = append(w.datasetList, vwDataset{Name: name, CreatedOn: createdOn})
}

// vwTestID3Points is a realistic ID.3 dataset: SoC and range by GUID, plus
// charging state, plug state and odometer.
func vwTestID3Points() []vwDataPoint {
	ts := time.Date(2026, 10, 17, 4, 45, 0, 0, time.UTC)
	ts2 := ts.Add(-time.Minute)
	return []vwDataPoint{
		{Key: vwKeyBatteryStateReportSoc, DataFieldName: "battery_state_report.soc", Value: "67", TimestampUtc: &ts},
		{Key: vwKeyRangeID3, DataFieldName: "value", Value: "231", TimestampUtc: &ts2},
		{Key: "9da735bb-c5d5-39f8-bf53-0fa2a367aa8f", DataFieldName: "charging_state", Value: "charging", TimestampUtc: &ts2},
		{Key: "c111830c-f959-30d2-859e-ea996190d864", DataFieldName: "plug_state", Value: "connected", TimestampUtc: &ts2},
		{Key: "41c0805c-43e5-313e-9dfb-356cb8d20f7c", DataFieldName: "mileage", Value: "45231", TimestampUtc: &ts2},
	}
}

// vwTestCredentialsPage renders the fake credentials page (the window._IDK
// JS block the real identity service embeds in the password form page).
func vwTestCredentialsPage(errMsg string) string {
	return `<html><head><script>window._IDK = {"templateModel":{"hmac":"hmac-2","relayState":"relay-2",` +
		`"postAction":"` + vwTestAuthForm + `","identifierUrl":"` + vwTestIdentifierForm + `","error":` + strconv.Quote(errMsg) +
		`},"csrf_token":"csrf-2"};</script></head><body></body></html>`
}

func (w *vwFakeWorld) identityHandler() http.Handler {
	mux := http.NewServeMux()

	// The OIDC authorize endpoint redirects to the signin page first.
	mux.HandleFunc("GET /oidc/v1/authorize", func(rw http.ResponseWriter, r *http.Request) {
		http.Redirect(rw, r, "/signin-service/v1/signin/test@apps_vw-dilab_com", http.StatusFound)
	})

	// The signin page carries the email form.
	mux.HandleFunc("GET /signin-service/v1/signin/test@apps_vw-dilab_com", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "text/html")
		fmt.Fprint(rw, `<html><body><form id="emailPasswordForm" action="`+vwTestIdentifierForm+`" method="post">`+
			`<input type="hidden" name="_csrf" value="csrf-1">`+
			`<input type="hidden" name="relayState" value="relay-1">`+
			`<input type="hidden" name="hmac" value="hmac-1">`+
			`</form></body></html>`)
	})

	// Email/identifier step: unknown users get the error on the credentials page.
	mux.HandleFunc("POST "+vwTestIdentifierForm, func(rw http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		errMsg := ""
		if r.Form.Get("email") != testVWUser {
			errMsg = "Unknown user"
		}
		rw.Header().Set("Content-Type", "text/html")
		fmt.Fprint(rw, vwTestCredentialsPage(errMsg))
	})

	// Password/authenticate step: success starts the redirect chain back to
	// the portal; wrong passwords re-render the login page (200, no redirect).
	mux.HandleFunc("POST "+vwTestAuthForm, func(rw http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		if r.Form.Get("password") != testVWPassword {
			rw.Header().Set("Content-Type", "text/html")
			fmt.Fprint(rw, `<html><body>login failed</body></html>`)
			return
		}
		switch {
		case w.consentPending.Load():
			http.Redirect(rw, r, "/signin-service/v1/signin/test@apps_vw-dilab_com?error=consent_required", http.StatusFound)
		case w.marketingConsent.Load():
			cb := w.identity.URL + "/oidc/v1/oauth/client/callback/success?user_id=u1"
			http.Redirect(rw, r, "/consent/marketing/test?callback="+url.QueryEscape(cb), http.StatusFound)
		default:
			http.Redirect(rw, r, "/oidc/v1/oauth/sso?clientId=test&relayState=relay-2", http.StatusFound)
		}
	})

	mux.HandleFunc("GET /oidc/v1/oauth/sso", func(rw http.ResponseWriter, r *http.Request) {
		http.Redirect(rw, r, "/oidc/v1/oauth/client/callback/success?user_id=u1&relayState=relay-2", http.StatusFound)
	})
	mux.HandleFunc("GET /oidc/v1/oauth/client/callback/success", func(rw http.ResponseWriter, r *http.Request) {
		http.Redirect(rw, r, w.portal.URL+"/services/callbacklogin?code=abc", http.StatusFound)
	})
	mux.HandleFunc("GET /consent/marketing/test", func(rw http.ResponseWriter, r *http.Request) {
		fmt.Fprint(rw, `<html><body>marketing consent</body></html>`)
	})

	return mux
}

func (w *vwFakeWorld) portalHandler() http.Handler {
	mux := http.NewServeMux()

	// The login redirect chain ends here: the portal sets the session cookie
	// and redirects to the user page (which the client does not fetch).
	mux.HandleFunc("GET /services/callbacklogin", func(rw http.ResponseWriter, r *http.Request) {
		w.logins.Add(1)
		http.SetCookie(rw, &http.Cookie{Name: "euda_session", Value: "abc123", Path: "/"})
		http.Redirect(rw, r, "/content/euda/de/de/user.html", http.StatusFound)
	})
	mux.HandleFunc("GET /content/euda/de/de/user.html", func(rw http.ResponseWriter, r *http.Request) {
		fmt.Fprint(rw, `<html><body>user page</body></html>`)
	})

	mux.HandleFunc("GET /proxy_api/consent/me/vehicles", func(rw http.ResponseWriter, r *http.Request) {
		if !w.sessionOK(rw, r) {
			return
		}
		vehicles := []vwVehicle{
			{VIN: testVWVIN, NickName: "ID.3", ModelName: "ID.3 Pro"},
			{VIN: testVWGolfVIN, NickName: "e-Golf", ModelName: "e-Golf"},
		}
		rw.Header().Set("Content-Type", "application/json")
		if w.wrapVehicles.Load() {
			json.NewEncoder(rw).Encode(map[string]any{"vehicles": vehicles})
			return
		}
		json.NewEncoder(rw).Encode(vehicles)
	})

	mux.HandleFunc("GET /proxy_api/euda-apim/datarequest/vehicles/{vin}/metadata/partial", func(rw http.ResponseWriter, r *http.Request) {
		if !w.sessionOK(rw, r) {
			return
		}
		vin := r.PathValue("vin")
		if w.identifier404.Load() || (vin != testVWVIN && vin != testVWGolfVIN) {
			http.Error(rw, "not found", http.StatusNotFound)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		json.NewEncoder(rw).Encode(map[string]string{"Identifier": "req-1"})
	})

	mux.HandleFunc("GET /proxy_api/euda-apim/datadelivery/vehicles/{vin}/{id}/list", func(rw http.ResponseWriter, r *http.Request) {
		if !w.sessionOK(rw, r) {
			return
		}
		if w.noDatasets.Load() {
			http.Error(rw, "No files available for this request", http.StatusNotFound)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		json.NewEncoder(rw).Encode(w.datasetList)
	})

	mux.HandleFunc("GET /proxy_api/euda-apim/datadelivery/vehicles/{vin}/{id}/download", func(rw http.ResponseWriter, r *http.Request) {
		if !w.sessionOK(rw, r) {
			return
		}
		b, ok := w.datasets[r.Header.Get("filename")]
		if !ok {
			http.Error(rw, "unknown file", http.StatusNotFound)
			return
		}
		rw.Write(b)
	})

	return mux
}

// sessionOK enforces the portal session cookie; with expireOnce set, the next
// data call answers 401 once (simulating an expired session).
func (w *vwFakeWorld) sessionOK(rw http.ResponseWriter, r *http.Request) bool {
	if w.expireOnce.Load() {
		w.expireOnce.Store(false)
		http.Error(rw, "session expired", http.StatusUnauthorized)
		return false
	}
	c, err := r.Cookie("euda_session")
	if err != nil || c.Value != "abc123" {
		http.Error(rw, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func vwTestZipRaw(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestVWStatusHappyPath(t *testing.T) {
	w := newVWFakeWorld(t)
	ts := time.Date(2026, 10, 17, 4, 0, 0, 0, time.UTC)
	w.addDataset(t, "20261017044500_WVWZZZ.zip", ts, testVWVIN, vwTestID3Points())
	// A "no content" placeholder newer than the content dataset must be skipped.
	w.datasetList = append(w.datasetList, vwDataset{
		Name:      "20261017050000_WVWZZZ_no_content_found.zip",
		CreatedOn: ts.Add(15 * time.Minute),
	})

	st, err := w.newClient(t, "").Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	if st.VIN != testVWVIN || st.VehicleName != "ID.3" {
		t.Errorf("vehicle: got %q (%q), want %q (ID.3)", st.VehicleName, st.VIN, testVWVIN)
	}
	if st.SocPercent != 67 {
		t.Errorf("soc: got %v, want 67", st.SocPercent)
	}
	if st.RangeKm != 231 {
		t.Errorf("range: got %v, want 231", st.RangeKm)
	}
	if st.ChargeStatus != evChargeCharging {
		t.Errorf("charge status: got %q, want %q", st.ChargeStatus, evChargeCharging)
	}
	if st.OdometerKm != 45231 {
		t.Errorf("odometer: got %v, want 45231", st.OdometerKm)
	}
	if want := time.Date(2026, 10, 17, 4, 45, 0, 0, time.UTC); !st.DataTime.Equal(want) {
		t.Errorf("data time: got %v, want %v", st.DataTime, want)
	}
	if w.logins.Load() != 1 {
		t.Errorf("logins: got %d, want 1", w.logins.Load())
	}
}

func TestVWStatusSelectsVehicleByVIN(t *testing.T) {
	w := newVWFakeWorld(t)
	w.addDataset(t, "20261017044500_WVWZZZ.zip",
		time.Date(2026, 10, 17, 4, 0, 0, 0, time.UTC), testVWGolfVIN, vwTestID3Points())

	// VIN matching is case-insensitive.
	st, err := w.newClient(t, strings.ToLower(testVWGolfVIN)).Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.VIN != testVWGolfVIN || st.VehicleName != "e-Golf" {
		t.Errorf("vehicle: got %q (%q), want %q (e-Golf)", st.VehicleName, st.VIN, testVWGolfVIN)
	}

	_, err = w.newClient(t, "WVWZZZ0000000000000").Status(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("unknown VIN: expected 'not found' error, got %v", err)
	}
}

func TestVWStatusVehiclesWrappedResponse(t *testing.T) {
	w := newVWFakeWorld(t)
	w.wrapVehicles.Store(true)
	w.addDataset(t, "20261017044500_WVWZZZ.zip",
		time.Date(2026, 10, 17, 4, 0, 0, 0, time.UTC), testVWVIN, vwTestID3Points())

	st, err := w.newClient(t, "").Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.SocPercent != 67 {
		t.Errorf("soc: got %v, want 67", st.SocPercent)
	}
}

func TestVWStatusBadPassword(t *testing.T) {
	w := newVWFakeWorld(t)
	c, err := newVWPortalClient("Volkswagen", testVWUser, "wrong-password", "",
		w.portal.URL, w.identity.URL, 30, w.httpClient(), testLogger())
	if err != nil {
		t.Fatalf("newVWPortalClient: %v", err)
	}
	_, err = c.Status(context.Background())
	if err == nil || !strings.Contains(err.Error(), "login did not complete") {
		t.Errorf("expected login failure, got %v", err)
	}
}

func TestVWStatusUnknownUser(t *testing.T) {
	w := newVWFakeWorld(t)
	c, err := newVWPortalClient("Volkswagen", "nobody@example.com", testVWPassword, "",
		w.portal.URL, w.identity.URL, 30, w.httpClient(), testLogger())
	if err != nil {
		t.Fatalf("newVWPortalClient: %v", err)
	}
	_, err = c.Status(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Unknown user") {
		t.Errorf("expected 'Unknown user' error, got %v", err)
	}
}

func TestVWStatusConsentPending(t *testing.T) {
	w := newVWFakeWorld(t)
	w.consentPending.Store(true)
	_, err := w.newClient(t, "").Status(context.Background())
	if err == nil || !strings.Contains(err.Error(), "consent") {
		t.Errorf("expected consent hint, got %v", err)
	}
}

func TestVWStatusMarketingConsentSkipped(t *testing.T) {
	w := newVWFakeWorld(t)
	w.marketingConsent.Store(true)
	w.addDataset(t, "20261017044500_WVWZZZ.zip",
		time.Date(2026, 10, 17, 4, 0, 0, 0, time.UTC), testVWVIN, vwTestID3Points())

	// Login must succeed despite the interjected marketing consent page.
	st, err := w.newClient(t, "").Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.SocPercent != 67 {
		t.Errorf("soc: got %v, want 67", st.SocPercent)
	}
	if w.logins.Load() != 1 {
		t.Errorf("logins: got %d, want 1", w.logins.Load())
	}
}

func TestVWStatusNotConfigured(t *testing.T) {
	w := newVWFakeWorld(t)
	w.identifier404.Store(true)
	_, err := w.newClient(t, "").Status(context.Background())
	if !errors.Is(err, errVWNotConfigured) {
		t.Errorf("expected errVWNotConfigured, got %v", err)
	}
}

func TestVWStatusNoDatasets(t *testing.T) {
	w := newVWFakeWorld(t)
	w.noDatasets.Store(true)
	_, err := w.newClient(t, "").Status(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no vehicle datasets") {
		t.Errorf("expected 'no vehicle datasets' error, got %v", err)
	}
}

func TestVWStatusReloginOn401(t *testing.T) {
	w := newVWFakeWorld(t)
	w.addDataset(t, "20261017044500_WVWZZZ.zip",
		time.Date(2026, 10, 17, 4, 0, 0, 0, time.UTC), testVWVIN, vwTestID3Points())
	w.expireOnce.Store(true)

	st, err := w.newClient(t, "").Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.SocPercent != 67 {
		t.Errorf("soc: got %v, want 67", st.SocPercent)
	}
	if w.logins.Load() != 2 {
		t.Errorf("logins: got %d, want 2 (initial + re-login)", w.logins.Load())
	}
}

func TestVWStatusBackfillsOlderDatasetForSoc(t *testing.T) {
	w := newVWFakeWorld(t)
	older := time.Date(2026, 10, 17, 3, 0, 0, 0, time.UTC)
	newer := older.Add(30 * time.Minute)

	// Newest dataset: only the odometer (the car was asleep, sparse data).
	tsN := newer
	w.addDataset(t, "20261017033000_WVWZZZ.zip", newer, testVWVIN, []vwDataPoint{
		{Key: "41c0805c-43e5-313e-9dfb-356cb8d20f7c", DataFieldName: "mileage", Value: "45231", TimestampUtc: &tsN},
	})
	// Older dataset: the SoC.
	tsO := older
	w.addDataset(t, "20261017030000_WVWZZZ.zip", older, testVWVIN, []vwDataPoint{
		{Key: vwKeyBatteryStateReportSoc, DataFieldName: "battery_state_report.soc", Value: "67", TimestampUtc: &tsO},
	})

	st, err := w.newClient(t, "").Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.SocPercent != 67 {
		t.Errorf("soc: got %v, want 67 (from the older dataset)", st.SocPercent)
	}
	if st.OdometerKm != 45231 {
		t.Errorf("odometer: got %v, want 45231 (from the newest dataset)", st.OdometerKm)
	}
	if !st.DataTime.Equal(newer) {
		t.Errorf("data time: got %v, want %v (newest point)", st.DataTime, newer)
	}
}

func TestVWNewClientValidation(t *testing.T) {
	if _, err := newVWPortalClient("Tesla", "u", "p", "", "", "", 0, nil, testLogger()); err == nil {
		t.Error("expected error for unknown brand")
	}
	if _, err := newVWPortalClient("Volkswagen", "", "p", "", "", "", 0, nil, testLogger()); err == nil {
		t.Error("expected error for missing user")
	}
	if _, err := newVWPortalClient("Volkswagen", "u", "", "", "", "", 0, nil, testLogger()); err == nil {
		t.Error("expected error for missing password")
	}

	c, err := newVWPortalClient("volkswagen", "u", "p", "", "", "", 0, nil, testLogger())
	if err != nil {
		t.Fatalf("newVWPortalClient: %v", err)
	}
	if c.portalURL != defaultVWPortalURL || c.identityURL != defaultVWIdentityURL {
		t.Errorf("default URLs not applied: %q %q", c.portalURL, c.identityURL)
	}
	if c.client.Timeout != 30*time.Second {
		t.Errorf("default timeout: got %v, want 30s", c.client.Timeout)
	}
}

func TestVWResolveBrand(t *testing.T) {
	for _, name := range []string{"volkswagen", "VOLKSWAGEN", "Volkswagen"} {
		if _, ok := resolveVWBrand(name); !ok {
			t.Errorf("brand %q must resolve", name)
		}
	}
	if _, ok := resolveVWBrand("tesla"); ok {
		t.Error("brand tesla must not resolve")
	}
	if !vwBrandKnown("Audi") || !vwBrandKnown("Skoda") || vwBrandKnown("Tesla") {
		t.Error("vwBrandKnown mapping broken")
	}
	// The VW brand parameters are the ones evcc's eudataact package uses.
	b, ok := resolveVWBrand("Volkswagen")
	if !ok {
		t.Fatal("Volkswagen must resolve")
	}
	if b.clientID != "9b58543e-1c15-4193-91d5-8a14145bebb0@apps_vw-dilab_com" || b.state != "VOLKSWAGEN_PASSENGER_CARS" {
		t.Errorf("unexpected VW brand parameters: %+v", b)
	}
}

func TestVWParseDataset(t *testing.T) {
	ts1 := time.Date(2026, 10, 17, 4, 0, 0, 0, time.UTC)
	ts2 := ts1.Add(time.Hour)
	points := []vwDataPoint{
		{Key: "k1", DataFieldName: "a", Value: "1", TimestampUtc: &ts1},
		{Key: "k1", DataFieldName: "a", Value: "2", TimestampUtc: &ts2}, // newer wins
		{Key: "k2", DataFieldName: "b", Value: "", TimestampUtc: &ts1},  // empty dropped
		{DataFieldName: "", Value: "3"},                                 // no id dropped
		{DataFieldName: "c", Value: "4", TimestampUtc: &ts1},            // name fallback id
	}
	got, err := parseVWDataset(vwTestZipRaw(t, "dataset.json",
		`{"vin":"WVWZZZ","Data":`+mustJSON(t, points)+`}`))
	if err != nil {
		t.Fatalf("parseVWDataset: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("points: got %d, want 2 (k1 deduped, k2 empty dropped, no-id dropped, c kept)", len(got))
	}
	if p := findVWPoint(got, "k1"); p == nil || p.Value != "2" {
		t.Errorf("k1: got %+v, want value 2 (newest)", p)
	}
	if p := findVWPoint(got, "c"); p == nil || p.Value != "4" {
		t.Errorf("c: got %+v, want value 4 (matched by name)", p)
	}

	if _, err := parseVWDataset([]byte("not a zip")); err == nil {
		t.Error("expected error for non-zip input")
	}
	if _, err := parseVWDataset(vwTestZipRaw(t, "doc.txt", "x")); err == nil {
		t.Error("expected error for zip without json document")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestVWContentDatasets(t *testing.T) {
	base := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	list := []vwDataset{
		{Name: "b_no_content_found.zip", CreatedOn: base.Add(30 * time.Minute)},
		{Name: "c.zip", CreatedOn: base.Add(45 * time.Minute)},
		{Name: "a.zip", CreatedOn: base.Add(15 * time.Minute)},
		{Name: "d_NO_CONTENT_FOUND.ZIP", CreatedOn: base.Add(60 * time.Minute)},
	}
	got := contentVWDatasets(list)
	if len(got) != 2 {
		t.Fatalf("content datasets: got %d, want 2", len(got))
	}
	if got[0].Name != "a.zip" || got[1].Name != "c.zip" {
		t.Errorf("content datasets not sorted oldest first: %v", got)
	}
}

func TestVWExtractSoc(t *testing.T) {
	ts := time.Date(2026, 10, 17, 4, 45, 0, 0, time.UTC)
	cases := []struct {
		name   string
		points []vwPoint
		want   float64
	}{
		{"id3 guid", []vwPoint{{Key: vwKeyBatteryStateReportSoc, Name: "battery_state_report.soc", Value: "67", Timestamp: ts}}, 67},
		{"hv battery level value", []vwPoint{{Name: vwFieldHvBatteryLevelValue, Value: "55.5", Timestamp: ts}}, 55.5},
		{"state_of_charge", []vwPoint{{Name: vwFieldSoc, Value: "42", Timestamp: ts}}, 42},
		{"hv_soc", []vwPoint{{Name: vwFieldHvSoc, Value: "80", Timestamp: ts}}, 80},
		{"enyaq guid", []vwPoint{{Key: vwKeyEnyaqSoc, Name: "value", Value: "73", Timestamp: ts}}, 73},
		{"priority: battery_level_HV.value over id3 guid", []vwPoint{
			{Key: vwKeyBatteryStateReportSoc, Name: "battery_state_report.soc", Value: "67", Timestamp: ts},
			{Name: vwFieldHvBatteryLevelValue, Value: "55", Timestamp: ts},
		}, 55},
		{"unparsable", []vwPoint{{Key: vwKeyBatteryStateReportSoc, Value: "n/a", Timestamp: ts}}, -1},
		{"missing", nil, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractEVStatus(tc.points, time.Time{}).SocPercent; got != tc.want {
				t.Errorf("soc: got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestVWExtractRangeAndOdometer(t *testing.T) {
	ts := time.Date(2026, 10, 17, 4, 45, 0, 0, time.UTC)

	st := extractEVStatus([]vwPoint{
		{Key: vwKeyRangeID3, Name: "value", Value: "231", Timestamp: ts},
		{Name: vwFieldOdometer, Value: "45231", Timestamp: ts},
	}, time.Time{})
	if st.RangeKm != 231 {
		t.Errorf("range: got %v, want 231", st.RangeKm)
	}
	if st.OdometerKm != 45231 {
		t.Errorf("odometer: got %v, want 45231", st.OdometerKm)
	}

	st = extractEVStatus([]vwPoint{
		{Name: vwFieldRangeCombined, Value: "250.7", Timestamp: ts},
		{Name: vwFieldOdometerValue, Value: "100", Timestamp: ts},
	}, time.Time{})
	if st.RangeKm != 250 {
		t.Errorf("range: got %v, want 250", st.RangeKm)
	}
	if st.OdometerKm != 100 {
		t.Errorf("odometer: got %v, want 100", st.OdometerKm)
	}

	// Missing fields are -1; the fallback time is used as the data time.
	st = extractEVStatus(nil, ts)
	if st.RangeKm != -1 || st.OdometerKm != -1 {
		t.Errorf("missing fields: got range %v, odo %v, want -1/-1", st.RangeKm, st.OdometerKm)
	}
	if !st.DataTime.Equal(ts) {
		t.Errorf("data time: got %v, want fallback %v", st.DataTime, ts)
	}
}

func TestVWChargeStatus(t *testing.T) {
	cases := []struct {
		name   string
		points []vwPoint
		want   string
	}{
		{"no fields", nil, ""},
		{"plug disconnected", []vwPoint{{Name: vwFieldPlugState, Value: "disconnected"}}, evChargeDisconnected},
		{"plug connected", []vwPoint{{Name: vwFieldPlugState, Value: "connected"}}, evChargeConnected},
		{"charging_state charging", []vwPoint{{Name: vwFieldChargingState, Value: "charging"}}, evChargeCharging},
		{"charging_state conservation", []vwPoint{{Name: vwFieldChargingState, Value: "conservationCharging"}}, evChargeCharging},
		{"current_charge_state conservation", []vwPoint{{Name: vwFieldCurrentChargeState, Value: "CHARGE_STATE_CONSERVATION_CHARGING"}}, evChargeCharging},
		{"current_charge_state charging_hv", []vwPoint{{Name: vwFieldCurrentChargeState, Value: "CHARGE_STATE_CHARGING_HV"}}, evChargeCharging},
		{"charge ready, no plug", []vwPoint{{Name: vwFieldChargingState, Value: "readyForCharging"}}, evChargeDisconnected},
		{"scenario active", []vwPoint{{Name: vwFieldChargingScenario, Value: "CHARGING_SCENARIO_IMMEDIATELY_CHARGING_ACTIVE"}}, evChargeCharging},
		{"scenario finished", []vwPoint{{Name: vwFieldChargingScenario, Value: "CHARGING_SCENARIO_IMMEDIATELY_CHARGING_FINISHED"}}, evChargeConnected},
		{"scenario off", []vwPoint{{Name: vwFieldChargingScenario, Value: "CHARGING_SCENARIO_OFF"}}, evChargeDisconnected},
		{"scenario mixed case", []vwPoint{{Name: vwFieldChargingScenario, Value: "Charging_Scenario_Off"}}, evChargeDisconnected},
		// Charging wins over a stale connected plug state.
		{"plug connected + charging", []vwPoint{
			{Name: vwFieldPlugState, Value: "connected"},
			{Name: vwFieldChargingState, Value: "charging"},
		}, evChargeCharging},
		// A finished scenario means plugged in but not charging.
		{"plug connected + scenario finished", []vwPoint{
			{Name: vwFieldPlugState, Value: "connected"},
			{Name: vwFieldChargingScenario, Value: "CHARGING_SCENARIO_IMMEDIATELY_CHARGING_FINISHED"},
		}, evChargeConnected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := vwChargeStatus(tc.points); got != tc.want {
				t.Errorf("status: got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestVWSelectVehicle(t *testing.T) {
	vehicles := []vwVehicle{{VIN: "AAA111"}, {VIN: "BBB222", NickName: "Golf"}}

	if _, err := selectVWVehicle(nil, ""); err == nil {
		t.Error("expected error for empty vehicle list")
	}
	v, err := selectVWVehicle(vehicles, "")
	if err != nil || v.VIN != "AAA111" {
		t.Errorf("no VIN: got %+v, %v — want the first vehicle", v, err)
	}
	v, err = selectVWVehicle(vehicles, "bbb222")
	if err != nil || v.VIN != "BBB222" {
		t.Errorf("VIN match: got %+v, %v — want BBB222 (case-insensitive)", v, err)
	}
	if _, err := selectVWVehicle(vehicles, "CCC333"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("unknown VIN: got %v", err)
	}
}

func TestVWFormValues(t *testing.T) {
	html := `<html><body><form id="emailPasswordForm" action="/login/identifier" method="post">` +
		`<input type="hidden" name="_csrf" value="c1">` +
		`<input type="hidden" name="relayState" value="r1">` +
		`</form></body></html>`
	vars, err := vwFormValues(strings.NewReader(html), "form#emailPasswordForm")
	if err != nil {
		t.Fatalf("vwFormValues: %v", err)
	}
	if vars.Action != "/login/identifier" {
		t.Errorf("action: got %q", vars.Action)
	}
	if vars.Inputs["_csrf"] != "c1" || vars.Inputs["relayState"] != "r1" {
		t.Errorf("inputs: got %v", vars.Inputs)
	}

	if _, err := vwFormValues(strings.NewReader("<html></html>"), "form#emailPasswordForm"); err == nil {
		t.Error("expected error for missing form")
	}
}

func TestVWParseCredentials(t *testing.T) {
	page := `<script>window._IDK = {"templateModel":{"hmac":"h","relayState":"r","postAction":"/login/authenticate","identifierUrl":"/login/identifier","error":""},"csrf_token":"c"};</script>`
	params, err := vwParseCredentials(page)
	if err != nil {
		t.Fatalf("vwParseCredentials: %v", err)
	}
	if params.CsrfToken != "c" || params.TemplateModel.PostAction != "/login/authenticate" ||
		params.TemplateModel.IdentifierUrl != "/login/identifier" || params.TemplateModel.Error != "" {
		t.Errorf("params: got %+v", params)
	}

	page = strings.Replace(page, `"error":""`, `"error":"invalid login"`, 1)
	params, err = vwParseCredentials(page)
	if err != nil {
		t.Fatalf("vwParseCredentials: %v", err)
	}
	if params.TemplateModel.Error != "invalid login" {
		t.Errorf("error: got %q", params.TemplateModel.Error)
	}

	if _, err := vwParseCredentials("<html></html>"); err == nil {
		t.Error("expected error for missing IDK block")
	}
}

func TestVWIsUserPage(t *testing.T) {
	const portal = "eu-data-act.drivesomethinggreater.com"
	cases := []struct {
		uri  string
		want bool
	}{
		{"https://" + portal + "/content/euda/de/de/user.html", true},
		{"https://" + portal + "/content/euda/gb/en/user.html?next=/error", true},
		{"https://" + portal + "/content/euda/de/de/error.html?next=user.html", false},
		{"https://" + portal + "/content/other/de/de/user.html", false},
		{"https://" + portal + "/content/euda/de/de/user.html/extra", false},
		{"http://" + portal + "/content/euda/de/de/user.html", false},
		{"https://example.org/content/euda/de/de/user.html", false},
	}
	for _, tc := range cases {
		t.Run(tc.uri, func(t *testing.T) {
			u, err := url.Parse(tc.uri)
			if err != nil {
				t.Fatal(err)
			}
			if got := isVWUserPage(u, portal); got != tc.want {
				t.Errorf("isVWUserPage(%s): got %v, want %v", tc.uri, got, tc.want)
			}
		})
	}
}
