package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/publicsuffix"
)

// The EV source reads VW group vehicles (VW ID.3 and friends, Audi, Skoda,
// Seat, Cupra) from the VW group "EU Data Act" portal — the same approach evcc
// uses for its "drivesomethinggreater" vehicle type (vehicle/vw/eudataact in
// the evcc repo). VW retired third-party access to the old WeConnect app API,
// so the portal is the sanctioned path. There is no standalone Go library for
// it, so the client below is a minimal port of evcc's implementation, adapted
// to this program's run-once model:
//
//   - Login is the OIDC authorization-code flow against identity.vwgroup.io
//     driven as an HTML form login (email step, password step), following the
//     redirect chain with a cookie jar until the portal session cookie is set.
//     No token exchange: the portal's data API authenticates via the session
//     cookie, and 401/403 triggers one re-login.
//   - The portal is not a live telemetry API: it stores a zipped JSON dataset
//     roughly every 15 minutes (only while the vehicle is active) that the
//     user enables once in a browser. Reading vehicle data means listing the
//     datasets, downloading the newest one(s) and decoding the flat list of
//     data points.
//   - Unlike evcc (which polls every 30s and merges datasets in a store), this
//     program runs once a day: it downloads only the newest content dataset,
//     walking back a few datasets when the newest one lacks the SoC point.

const (
	defaultVWPortalURL   = "https://eu-data-act.drivesomethinggreater.com"
	defaultVWIdentityURL = "https://identity.vwgroup.io"
	vwAuthorizePath      = "/oidc/v1/authorize"
	vwScope              = "openid cars profile"
)

// maxDatasetBackfill bounds how many of the newest content datasets are
// downloaded to find a state-of-charge value (the car may be asleep and the
// newest datasets may be sparse or "no content" placeholders).
const maxDatasetBackfill = 3

// errVWNotConfigured reports that the vehicle has no data request enabled on
// the portal (the one-time browser setup is missing).
var errVWNotConfigured = errors.New("no EU Data Act data request is configured for this vehicle — open the portal in a browser, link the vehicle and enable the continuous data request (every 15 minutes)")

// vwBrand holds the OIDC client id and state suffix for a VW group brand. All
// brands share the same portal and endpoints and differ only in the identity
// client id (same values as evcc's eudataact package / ioBroker.vw-connect).
type vwBrand struct {
	clientID string
	state    string
}

var vwBrands = map[string]vwBrand{
	"Volkswagen": {"9b58543e-1c15-4193-91d5-8a14145bebb0@apps_vw-dilab_com", "VOLKSWAGEN_PASSENGER_CARS"},
	"Audi":       {"cc29b87a-5e9a-4362-aecf-5adea6b01bbb@apps_vw-dilab_com", "AUDI"},
	"Skoda":      {"3ea88bf9-1d4e-4a68-b3ad-4098c1f1d246@apps_vw-dilab_com", "SKODA"},
	"Seat":       {"f85e5b69-e3b2-43aa-9c0d-1b7d0e0b576f@apps_vw-dilab_com", "SEAT"},
	"Cupra":      {"f85e5b69-e3b2-43aa-9c0d-1b7d0e0b576f@apps_vw-dilab_com", "CUPRA"},
}

// resolveVWBrand looks up a brand by name, case-insensitively.
func resolveVWBrand(name string) (vwBrand, bool) {
	for k, b := range vwBrands {
		if strings.EqualFold(k, name) {
			return b, true
		}
	}
	return vwBrand{}, false
}

// vwBrandKnown reports whether name is a known VW group brand.
func vwBrandKnown(name string) bool {
	_, ok := resolveVWBrand(name)
	return ok
}

// evStatus is the distilled state of the vehicle for the brief.
type evStatus struct {
	VehicleName  string
	VIN          string
	SocPercent   float64 // -1 when absent from the dataset
	RangeKm      int64   // -1 when absent from the dataset
	OdometerKm   float64 // -1 when absent from the dataset
	ChargeStatus string  // evCharge* constants; "" when unknown
	DataTime     time.Time
}

// Charge status values (evcc's StatusA/B/C: disconnected, connected, charging).
const (
	evChargeDisconnected = "disconnected"
	evChargeConnected    = "connected"
	evChargeCharging     = "charging"
)

// vwVehicle is a single entry of the portal vehicle list. VIN and name carry
// several alternative field names depending on the response variant.
type vwVehicle struct {
	VIN                         string `json:"vin"`
	VehicleIdentificationNumber string `json:"vehicleIdentificationNumber"`
	NickName                    string `json:"nickName"`
	VehicleNickname             string `json:"vehicleNickname"`
	Nickname                    string `json:"nickname"`
	ModelName                   string `json:"modelName"`
}

// Vin returns the vehicle identification number from whichever field is set.
func (v vwVehicle) Vin() string {
	if v.VIN != "" {
		return v.VIN
	}
	return v.VehicleIdentificationNumber
}

// Name returns the first non-empty display name.
func (v vwVehicle) Name() string {
	for _, s := range []string{v.NickName, v.VehicleNickname, v.Nickname, v.ModelName} {
		if s != "" {
			return s
		}
	}
	return ""
}

// vwDataset describes a single delivered dataset file.
type vwDataset struct {
	Name      string    `json:"name"`
	CreatedOn time.Time `json:"createdOn"`
}

// vwDataPoint is a single data point as delivered in the dataset JSON
// document. Key is the data point's unique GUID, used when DataFieldName is
// generic (e.g. "value"). All values are delivered as strings.
type vwDataPoint struct {
	Key           string     `json:"key"`
	DataFieldName string     `json:"dataFieldName"`
	Value         string     `json:"value"`
	TimestampUtc  *time.Time `json:"timestampUtc"`
}

// vwPoint is a decoded data point: its unique GUID (Key), delivered field
// Name, value and record time.
type vwPoint struct {
	Key       string
	Name      string
	Value     string
	Timestamp time.Time
}

// id is the point's deduplication and lookup identity: its unique GUID when
// present, otherwise the (possibly non-unique) field name.
func (p vwPoint) id() string {
	if p.Key != "" {
		return p.Key
	}
	return p.Name
}

// Data field names and GUID keys as delivered in the dataset (same values as
// evcc's eudataact package; see the EU Data Act data dictionary).
const (
	// charging status
	vwFieldChargingState                = "charging_state"
	vwFieldChargingPlug1ConnectionState = "charging_plug1_connectionstate"
	vwFieldCurrentChargeState           = "charging_state_report.current_charge_state"
	vwFieldChargingScenario             = "charging_state_report.charging_scenario"
	vwFieldPlugState                    = "plug_state"

	// state of charge
	vwFieldSoc                 = "state_of_charge"
	vwFieldHvSoc               = "hv_soc"
	vwFieldHvBatteryLevelValue = "battery_level_HV.value"

	// range
	vwFieldRangeCombined  = "cruising_range_combined"
	vwFieldRangePrimary   = "cruising_range_primary_engine"
	vwFieldRangeSecondary = "cruising_range_secondary_engine"

	vwKeyRangeID3              = "0ca40e18-0564-3eda-bcc0-7aee9ef44f04" // VW ID.3 range
	vwKeyBatteryStateReportSoc = "506cb83e-f99f-3af3-bbeb-0429b69a78d9" // VW ID.3 soc
	vwKeyEnyaqSoc              = "0a18a053-b4b0-3db1-be44-a6c5dba629b1" // Skoda Enyaq soc

	// odometer
	vwFieldOdometer      = "mileage"
	vwFieldOdometerValue = "mileage.value"
)

// Field lookup lists, most to least authoritative (same priority as evcc).
var (
	vwSocFields      = []string{vwFieldHvBatteryLevelValue, vwFieldSoc, vwFieldHvSoc, vwKeyBatteryStateReportSoc, vwKeyEnyaqSoc}
	vwRangeFields    = []string{vwFieldRangeSecondary, vwFieldRangePrimary, vwFieldRangeCombined, vwKeyRangeID3}
	vwOdometerFields = []string{vwFieldOdometer, vwFieldOdometerValue}
)

// vwPortalClient reads vehicle data from the VW EU Data Act portal.
type vwPortalClient struct {
	brand       vwBrand
	user        string
	password    string
	vin         string
	portalURL   string
	identityURL string
	client      *http.Client
	logger      *logrus.Logger

	loginMu  sync.Mutex // serializes logins
	loggedIn atomic.Bool
}

// newVWPortalClient creates a client. portalURL/identityURL default to the VW
// group endpoints and are overridable for tests; httpClient may be nil (a
// default client is used) or carry a custom transport (e.g. test TLS). The
// client does not log in yet — the login happens lazily on the first Status.
func newVWPortalClient(brand, user, password, vin, portalURL, identityURL string, timeoutSeconds int, httpClient *http.Client, logger *logrus.Logger) (*vwPortalClient, error) {
	b, ok := resolveVWBrand(brand)
	if !ok {
		return nil, fmt.Errorf("unknown brand %q (supported: Volkswagen, Audi, Skoda, Seat, Cupra)", brand)
	}
	if user == "" || password == "" {
		return nil, fmt.Errorf("user and password are required (your Volkswagen ID credentials)")
	}
	if portalURL == "" {
		portalURL = defaultVWPortalURL
	}
	if identityURL == "" {
		identityURL = defaultVWIdentityURL
	}
	if timeoutSeconds <= 0 {
		timeoutSeconds = 30
	}
	if logger == nil {
		logger = logrus.New()
	}
	if httpClient == nil {
		httpClient = &http.Client{}
	}

	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return nil, err
	}

	client := *httpClient
	client.Jar = jar
	if client.Timeout == 0 {
		client.Timeout = time.Duration(timeoutSeconds) * time.Second
	}

	return &vwPortalClient{
		brand:       b,
		user:        user,
		password:    password,
		vin:         vin,
		portalURL:   strings.TrimRight(portalURL, "/"),
		identityURL: strings.TrimRight(identityURL, "/"),
		client:      &client,
		logger:      logger,
	}, nil
}

// Status logs in (if needed), selects the configured vehicle and returns its
// current state from the newest portal dataset(s).
func (c *vwPortalClient) Status(ctx context.Context) (*evStatus, error) {
	if err := c.ensureLoggedIn(ctx); err != nil {
		return nil, err
	}

	vehicles, err := c.vehicles(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing vehicles: %w", err)
	}
	vehicle, err := selectVWVehicle(vehicles, c.vin)
	if err != nil {
		return nil, err
	}

	identifier, err := c.identifier(ctx, vehicle.Vin())
	if err != nil {
		return nil, err
	}

	list, err := c.datasets(ctx, vehicle.Vin(), identifier)
	if err != nil {
		return nil, err
	}
	content := contentVWDatasets(list)
	if len(content) == 0 {
		return nil, errors.New("no vehicle datasets on the portal yet — the portal delivers a dataset roughly every 15 minutes, and only while the vehicle is active (driving or charging)")
	}

	points, dataTime, err := c.latestPoints(ctx, vehicle.Vin(), identifier, content)
	if err != nil {
		return nil, err
	}
	if len(points) == 0 {
		return nil, errors.New("the latest vehicle dataset contains no data points")
	}

	st := extractEVStatus(points, dataTime)
	st.VehicleName = vehicle.Name()
	st.VIN = vehicle.Vin()

	c.logger.WithFields(logrus.Fields{
		"vehicle": st.VehicleName,
		"vin":     st.VIN,
		"soc":     st.SocPercent,
		"range":   st.RangeKm,
		"status":  st.ChargeStatus,
	}).Info("EV status read via the EU Data Act portal")

	return st, nil
}

// ensureLoggedIn logs in unless a login already succeeded.
func (c *vwPortalClient) ensureLoggedIn(ctx context.Context) error {
	if c.loggedIn.Load() {
		return nil
	}
	return c.login(ctx)
}

// login performs the OIDC authorization-code flow against the VW identity
// service as an HTML form login. The portal relies on the session cookies set
// while following the redirect chain back to the portal, so the cookie jar is
// kept on the client for all subsequent data calls.
func (c *vwPortalClient) login(ctx context.Context) error {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()

	portalURL, err := url.Parse(c.portalURL)
	if err != nil {
		return fmt.Errorf("invalid portal URL %q: %w", c.portalURL, err)
	}
	identityURL, err := url.Parse(c.identityURL)
	if err != nil {
		return fmt.Errorf("invalid identity URL %q: %w", c.identityURL, err)
	}
	portalHost := portalURL.Host
	identityHost := identityURL.Host

	// Private client copy for the login redirect chain: cookies land in the
	// shared jar; redirects are restricted to the identity and portal hosts
	// and stop at the portal user page / the marketing consent page.
	client := *c.client
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		// Cookies from the redirect response are already stored in the jar.
		if req.URL.Scheme != "https" || isVWUserPage(req.URL, portalHost) || isVWMarketingConsentPage(req.URL, identityHost) {
			return http.ErrUseLastResponse
		}
		if !isVWLoginHost(req.URL.Host, portalHost, identityHost) {
			return fmt.Errorf("unexpected login redirect host %s", req.URL.Host)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}

	// Start the OIDC authorize flow.
	q := url.Values{
		"client_id":     {c.brand.clientID},
		"response_type": {"code"},
		"scope":         {vwScope},
		"state":         {fmt.Sprintf("de__en__%s", c.brand.state)},
		"redirect_uri":  {c.portalURL + "/login"},
		"prompt":        {"login"},
		"nonce":         {randomVWLetters(43)},
	}

	resp, err := c.do(ctx, &client, http.MethodGet, c.identityURL+vwAuthorizePath+"?"+q.Encode(), nil, nil)
	if err != nil {
		return err
	}
	body, err := readVWBody(resp)
	if err != nil {
		return err
	}

	// Email/identifier step.
	vars, err := vwFormValues(bytes.NewReader(body), "form#emailPasswordForm")
	if err != nil {
		return fmt.Errorf("identifier form: %w (portal layout may have changed)", err)
	}

	uri := c.identityURL + vars.Action
	resp, err = c.do(ctx, &client, http.MethodPost, uri, url.Values{
		"_csrf":      {vars.Inputs["_csrf"]},
		"relayState": {vars.Inputs["relayState"]},
		"hmac":       {vars.Inputs["hmac"]},
		"email":      {c.user},
	}, nil)
	if err != nil {
		return err
	}

	params, err := vwParseCredentialsPage(resp.Body)
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("credentials page: %w", err)
	}
	if params.TemplateModel.Error != "" {
		return errors.New(params.TemplateModel.Error)
	}

	// Password/authenticate step — the client follows the redirect chain back
	// to the portal, which sets the session cookie.
	uri = strings.ReplaceAll(uri, params.TemplateModel.IdentifierUrl, params.TemplateModel.PostAction)
	resp, err = c.do(ctx, &client, http.MethodPost, uri, url.Values{
		"_csrf":      {params.CsrfToken},
		"relayState": {params.TemplateModel.RelayState},
		"hmac":       {params.TemplateModel.Hmac},
		"email":      {c.user},
		"password":   {c.password},
	}, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	final, err := vwLoginLocation(resp)
	if err != nil {
		return err
	}

	// Skip the optional marketing consent page VW periodically interjects
	// after an otherwise successful login, using its callback without
	// fetching the page.
	if cb, err := vwMarketingConsentCallback(final); err != nil {
		return err
	} else if cb != nil {
		if final.Scheme != "https" || !isVWLoginHost(final.Host, portalHost, identityHost) || !vwValidMarketingCallback(cb, identityHost) {
			return errors.New("unexpected marketing consent callback URL")
		}
		resp, err = c.do(ctx, &client, http.MethodGet, cb.String(), nil, nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if final, err = vwLoginLocation(resp); err != nil {
			return err
		}
	}

	// A successful login lands on the portal user page; a remaining
	// signin/consent url means the user has not completed the one-time
	// browser consent and vehicle linking.
	if strings.Contains(final.Path, "signin-service") || strings.Contains(final.Path, "/consent") || strings.Contains(final.Path, "/error") {
		return fmt.Errorf("VW login did not complete — open %s in a browser, confirm the consent screen, link the vehicle and enable the continuous data request: %s", c.portalURL, final)
	}
	if !strings.EqualFold(final.Host, portalHost) {
		return fmt.Errorf("VW login did not complete: unexpected landing host %s", final.Host)
	}
	if !isVWUserPage(final, portalHost) {
		return errors.New("VW login did not complete: unexpected landing page")
	}

	c.loggedIn.Store(true)
	c.logger.Debug("VW EU Data Act portal login successful")
	return nil
}

// do executes a single HTTP request with the given client.
func (c *vwPortalClient) do(ctx context.Context, client *http.Client, method, uri string, form url.Values, headers map[string]string) (*http.Response, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, uri, body)
	if err != nil {
		return nil, err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return client.Do(req)
}

// readVWBody reads and closes a response body (bounded).
func readVWBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// httpStatusError is a non-200 response with a short body snippet.
type httpStatusError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *httpStatusError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("HTTP %s: %s", e.Status, e.Body)
	}
	return "HTTP " + e.Status
}

// HasStatus reports whether the error is a response with the given status code.
func (e *httpStatusError) HasStatus(code int) bool {
	return e.StatusCode == code
}

// get executes a GET request, re-authenticating once on 401/403, and returns
// the body.
func (c *vwPortalClient) get(ctx context.Context, uri string, headers map[string]string) ([]byte, error) {
	resp, err := c.do(ctx, c.client, http.MethodGet, uri, nil, headers)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		resp.Body.Close()
		c.logger.Debug("VW portal session expired, re-authenticating")
		c.loggedIn.Store(false)
		if err := c.login(ctx); err != nil {
			return nil, fmt.Errorf("re-login failed: %w", err)
		}
		resp, err = c.do(ctx, c.client, http.MethodGet, uri, nil, headers)
		if err != nil {
			return nil, err
		}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		snippet := strings.Join(strings.Fields(string(body)), " ")
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return nil, &httpStatusError{StatusCode: resp.StatusCode, Status: resp.Status, Body: snippet}
	}
	return body, nil
}

// getJSON executes a GET request and decodes the JSON response.
func (c *vwPortalClient) getJSON(ctx context.Context, uri string, res any) error {
	b, err := c.get(ctx, uri, map[string]string{"Accept": "application/json"})
	if err != nil {
		return err
	}
	return json.Unmarshal(b, res)
}

// vehicles enumerates the vehicles the user has linked to the portal.
func (c *vwPortalClient) vehicles(ctx context.Context) ([]vwVehicle, error) {
	uri := c.portalURL + "/proxy_api/consent/me/vehicles?viewPosition=FRONT_LEFT"

	b, err := c.get(ctx, uri, map[string]string{"Accept": "application/json"})
	if err != nil {
		return nil, err
	}

	// The response is either a bare array or wrapped in {"vehicles": [...]}.
	var arr []vwVehicle
	if err := json.Unmarshal(b, &arr); err == nil && len(arr) > 0 {
		return arr, nil
	}

	var wrap struct {
		Vehicles []vwVehicle `json:"vehicles"`
	}
	if err := json.Unmarshal(b, &wrap); err != nil {
		return nil, err
	}
	return wrap.Vehicles, nil
}

// identifier returns the data-request identifier required for the data
// delivery calls.
func (c *vwPortalClient) identifier(ctx context.Context, vin string) (string, error) {
	uri := fmt.Sprintf("%s/proxy_api/euda-apim/datarequest/vehicles/%s/metadata/partial", c.portalURL, vin)

	var res struct {
		Identifier string `json:"Identifier"`
	}
	if err := c.getJSON(ctx, uri, &res); err != nil {
		var se *httpStatusError
		if errors.As(err, &se) && se.HasStatus(http.StatusNotFound) {
			return "", errVWNotConfigured
		}
		return "", err
	}
	if res.Identifier == "" {
		return "", errors.New("no data request configured for vehicle")
	}
	return res.Identifier, nil
}

// datasets lists the available datasets for the given data request. A 404
// means the vehicle has not delivered any dataset yet.
func (c *vwPortalClient) datasets(ctx context.Context, vin, identifier string) ([]vwDataset, error) {
	uri := fmt.Sprintf("%s/proxy_api/euda-apim/datadelivery/vehicles/%s/%s/list", c.portalURL, vin, identifier)

	b, err := c.get(ctx, uri, map[string]string{"Accept": "application/json", "type": "partial"})
	if err != nil {
		var se *httpStatusError
		if errors.As(err, &se) && se.HasStatus(http.StatusNotFound) {
			return nil, nil
		}
		return nil, err
	}

	// The response is either a bare array or wrapped in {"files": [...]}.
	var arr []vwDataset
	if err := json.Unmarshal(b, &arr); err == nil && len(arr) > 0 {
		return arr, nil
	}

	var res struct {
		Files []vwDataset `json:"files"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return nil, err
	}
	return res.Files, nil
}

// download fetches a dataset zip archive.
func (c *vwPortalClient) download(ctx context.Context, vin, identifier, name string) ([]byte, error) {
	uri := fmt.Sprintf("%s/proxy_api/euda-apim/datadelivery/vehicles/%s/%s/download", c.portalURL, vin, identifier)
	return c.get(ctx, uri, map[string]string{"filename": name, "type": "partial"})
}

// latestPoints downloads up to maxDatasetBackfill of the newest content
// datasets and merges their points (newest dataset first, so the first
// dataset containing a state-of-charge value ends the walk). It returns the
// merged points and the newest dataset's delivery time.
func (c *vwPortalClient) latestPoints(ctx context.Context, vin, identifier string, content []vwDataset) ([]vwPoint, time.Time, error) {
	var points []vwPoint
	var newest time.Time

	for i := len(content) - 1; i >= 0 && len(content)-i <= maxDatasetBackfill; i-- {
		d := content[i]
		if d.CreatedOn.After(newest) {
			newest = d.CreatedOn
		}

		b, err := c.download(ctx, vin, identifier, d.Name)
		if err != nil {
			return nil, time.Time{}, err
		}
		pts, err := parseVWDataset(b)
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("parsing dataset %s: %w", d.Name, err)
		}
		for _, p := range pts {
			if findVWPoint(points, p.id()) == nil {
				points = append(points, p)
			}
		}

		if lookupVWPoint(points, vwSocFields...) != nil {
			break
		}
	}

	return points, newest, nil
}

// selectVWVehicle picks the configured VIN, or the first linked vehicle.
func selectVWVehicle(vehicles []vwVehicle, vin string) (vwVehicle, error) {
	if len(vehicles) == 0 {
		return vwVehicle{}, errors.New("no vehicles are linked to the VW EU Data Act portal — link the vehicle in the portal first")
	}
	if vin == "" {
		return vehicles[0], nil
	}
	for _, v := range vehicles {
		if strings.EqualFold(v.Vin(), vin) {
			return v, nil
		}
	}
	return vwVehicle{}, fmt.Errorf("VIN %s not found among the %d vehicle(s) linked to the portal", vin, len(vehicles))
}

// contentVWDatasets returns the content datasets, sorted oldest to newest. The
// portal emits "..._no_content_found.zip" placeholders while the vehicle is
// asleep; those are skipped.
func contentVWDatasets(list []vwDataset) []vwDataset {
	content := make([]vwDataset, 0, len(list))
	for _, d := range list {
		if strings.HasSuffix(strings.ToLower(d.Name), "_no_content_found.zip") {
			continue
		}
		content = append(content, d)
	}
	slices.SortStableFunc(content, func(a, b vwDataset) int {
		return a.CreatedOn.Compare(b.CreatedOn)
	})
	return content
}

// parseVWDataset extracts the inner JSON document from a dataset zip archive
// and decodes it into its data points.
func parseVWDataset(b []byte) ([]vwPoint, error) {
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return nil, err
	}

	var file *zip.File
	for _, f := range zr.File {
		if strings.HasSuffix(strings.ToLower(f.Name), ".json") {
			file = f
			break
		}
	}
	if file == nil {
		return nil, errors.New("no json document in dataset")
	}

	rc, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	raw, err := io.ReadAll(io.LimitReader(rc, 64<<20))
	if err != nil {
		return nil, err
	}

	var ds struct {
		VIN  string        `json:"vin"`
		Data []vwDataPoint `json:"Data"`
	}
	if err := json.Unmarshal(raw, &ds); err != nil {
		return nil, err
	}

	return vwPoints(ds.Data), nil
}

// vwPoints decodes data points, keeping the newest entry per id (see
// vwPoint.id). Empty values and points without any identity are dropped.
func vwPoints(data []vwDataPoint) []vwPoint {
	var res []vwPoint

	for _, dp := range data {
		if dp.Value == "" {
			continue
		}

		var ts time.Time
		if dp.TimestampUtc != nil {
			ts = *dp.TimestampUtc
		}
		p := vwPoint{Key: dp.Key, Name: dp.DataFieldName, Value: dp.Value, Timestamp: ts}
		if p.id() == "" {
			continue
		}

		if e := findVWPoint(res, p.id()); e != nil {
			// Newest wins; on equal timestamps the later entry wins.
			if !e.Timestamp.After(p.Timestamp) {
				*e = p
			}
			continue
		}
		res = append(res, p)
	}

	return res
}

// findVWPoint returns the data point identified by id, matched by Key first
// and Name second, or nil if none is present.
func findVWPoint(data []vwPoint, id string) *vwPoint {
	if i := slices.IndexFunc(data, func(p vwPoint) bool { return p.Key == id || p.Name == id }); i >= 0 {
		return &data[i]
	}
	return nil
}

// lookupVWPoint returns the first present value among the given field names
// (most to least authoritative), or nil.
func lookupVWPoint(data []vwPoint, fields ...string) *vwPoint {
	for _, f := range fields {
		if p := findVWPoint(data, f); p != nil {
			return p
		}
	}
	return nil
}

// extractEVStatus distills the dataset points into an evStatus. It is a pure
// function so tests can feed it fixtures without any network. fallbackTime
// (the newest dataset's delivery time) is used as the data timestamp when no
// point carries one.
func extractEVStatus(points []vwPoint, fallbackTime time.Time) *evStatus {
	st := &evStatus{
		SocPercent: -1,
		RangeKm:    -1,
		OdometerKm: -1,
		DataTime:   fallbackTime,
	}

	for _, p := range points {
		if !p.Timestamp.IsZero() && p.Timestamp.After(st.DataTime) {
			st.DataTime = p.Timestamp
		}
	}

	if p := lookupVWPoint(points, vwSocFields...); p != nil {
		if f, err := strconv.ParseFloat(p.Value, 64); err == nil {
			st.SocPercent = f
		}
	}
	if p := lookupVWPoint(points, vwRangeFields...); p != nil {
		if f, err := strconv.ParseFloat(p.Value, 64); err == nil {
			st.RangeKm = int64(f)
		}
	}
	if p := lookupVWPoint(points, vwOdometerFields...); p != nil {
		if f, err := strconv.ParseFloat(p.Value, 64); err == nil {
			st.OdometerKm = f
		}
	}

	st.ChargeStatus = vwChargeStatus(points)
	return st
}

// vwChargeStatus maps the dataset's plug/charging fields to a charge status
// (same logic as evcc's eudataact Provider.Status). It returns "" when the
// dataset carries no charging-related field at all (unknown); when fields are
// present but none indicate a plug or a charge, the car counts as not plugged
// in (evcc's default StatusA).
func vwChargeStatus(points []vwPoint) string {
	status := ""
	found := false

	// Explicit plug state.
	if p := lookupVWPoint(points, vwFieldPlugState, vwFieldChargingPlug1ConnectionState); p != nil {
		found = true
		if strings.EqualFold(p.Value, "connected") {
			status = evChargeConnected
		} else {
			status = evChargeDisconnected
		}
	}

	// Flat charging_state field and the current_charge_state field.
	if p := lookupVWPoint(points, vwFieldChargingState, vwFieldCurrentChargeState); p != nil {
		found = true
		if strings.Contains(strings.ToUpper(p.Value), "CHARGING_HV") ||
			strings.EqualFold(p.Value, "charging") ||
			strings.EqualFold(p.Value, "conservationCharging") ||
			strings.EqualFold(p.Value, "CHARGE_STATE_CONSERVATION_CHARGING") {
			status = evChargeCharging
		}
	}

	// charging_scenario is the most explicit plug/charge signal.
	if p := lookupVWPoint(points, vwFieldChargingScenario); p != nil {
		found = true
		if status != evChargeCharging {
			upper := strings.ToUpper(p.Value)
			switch {
			case strings.HasSuffix(upper, "_ACTIVE"):
				status = evChargeCharging
			// The car reports finished if it is plugged in but not charging.
			case strings.HasSuffix(upper, "_FINISHED"):
				status = evChargeConnected
			}
		}
	}

	if !found {
		return ""
	}
	if status == "" {
		status = evChargeDisconnected
	}
	return status
}

// vwFormVars holds the HTML form input values required for the login.
type vwFormVars struct {
	Action string
	Inputs map[string]string
}

// vwFormValues extracts the form's action and input values from an HTML
// document (same approach as evcc's vwidentity package).
func vwFormValues(reader io.Reader, selector string) (vwFormVars, error) {
	vars := vwFormVars{Inputs: make(map[string]string)}

	doc, err := goquery.NewDocumentFromReader(reader)
	if err != nil {
		return vars, err
	}

	form := doc.Find(selector).First()
	if form.Length() != 1 {
		return vars, errors.New("form not found")
	}
	action, exists := form.Attr("action")
	if !exists {
		return vars, errors.New("form action attribute not found")
	}
	vars.Action = action

	form.Find("input").Each(func(_ int, el *goquery.Selection) {
		if name, ok := el.Attr("name"); ok {
			vars.Inputs[name], _ = el.Attr("value")
		}
	})

	return vars, nil
}

// vwCredentialParams is the parsed credentials page: the window._IDK JS block
// the identity service embeds in the password form page.
type vwCredentialParams struct {
	TemplateModel struct {
		Hmac          string `json:"hmac"`
		RelayState    string `json:"relayState"`
		PostAction    string `json:"postAction"`
		IdentifierUrl string `json:"identifierUrl"`
		Error         string `json:"error"`
	} `json:"templateModel"`
	CsrfToken string `json:"csrf_token"`
}

// vwParseCredentialsPage reads and parses a credentials page.
func vwParseCredentialsPage(r io.Reader) (vwCredentialParams, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return vwCredentialParams{}, err
	}
	return vwParseCredentials(string(b))
}

// vwParseCredentials extracts the window._IDK JS block from the credentials
// page body. The block is JavaScript, not strict JSON (single quotes,
// unquoted keys, trailing commas), so it is cleaned up before decoding.
func vwParseCredentials(body string) (vwCredentialParams, error) {
	var res vwCredentialParams

	match := regexp.MustCompile(`(?s)window._IDK\s*=\s*(.*?)[;<]`).FindAllStringSubmatch(body, -1)
	if len(match) < 1 || len(match[0]) < 2 {
		return res, errors.New("IDK form not found")
	}

	// Clean quotes.
	quotes1 := strings.ReplaceAll(match[0][1], `'`, `"`)
	quotes2 := regexp.MustCompile(`\s(\w+)(?s)*:`).ReplaceAllString(quotes1, ` "$1":`)

	// Strip ", }".
	tmpl := regexp.MustCompile(`(?s),\s+}`).ReplaceAllString(quotes2, "}")

	err := json.Unmarshal([]byte(tmpl), &res)
	return res, err
}

// isVWUserPage reports whether u is the portal page a successful login lands
// on (https://<portal>/content/euda/<market>/<lang>/user.html).
func isVWUserPage(u *url.URL, portalHost string) bool {
	return u.Scheme == "https" && strings.EqualFold(u.Host, portalHost) &&
		strings.HasPrefix(u.Path, "/content/euda/") && strings.HasSuffix(u.Path, "/user.html")
}

// isVWMarketingConsentPage reports whether u is the optional marketing
// consent page the identity service periodically interjects after login.
func isVWMarketingConsentPage(u *url.URL, identityHost string) bool {
	return strings.EqualFold(u.Host, identityHost) && strings.Contains(u.Path, "/consent/marketing/")
}

// isVWLoginHost reports whether host belongs to the identity service or the
// portal (the only hosts the login redirect chain may visit).
func isVWLoginHost(host, portalHost, identityHost string) bool {
	return strings.EqualFold(host, identityHost) || strings.EqualFold(host, portalHost)
}

// vwValidMarketingCallback reports whether u is a valid OIDC client callback
// on the identity service (the marketing consent page's skip target).
func vwValidMarketingCallback(u *url.URL, identityHost string) bool {
	validPath := u.Path == "/oidc/v1/oauth/client/callback" || u.Path == "/oidc/v1/oauth/client/callback/success"
	return u.Scheme == "https" && strings.EqualFold(u.Host, identityHost) && validPath
}

// vwMarketingConsentCallback returns the OIDC callback url embedded in an
// optional marketing consent page, or nil if u is not such a page.
func vwMarketingConsentCallback(u *url.URL) (*url.URL, error) {
	if u == nil || !strings.Contains(u.Path, "/consent/marketing/") {
		return nil, nil
	}

	callback := u.Query().Get("callback")
	if callback == "" {
		return nil, errors.New("marketing consent page is missing callback url")
	}

	cb, err := url.Parse(callback)
	if err != nil {
		return nil, err
	}
	// Normalize spaces in query values (e.g. scopes) so the request is valid.
	cb.RawQuery = cb.Query().Encode()

	return cb, nil
}

// vwLoginLocation returns the URL a login step landed on: the redirect target
// for redirect responses, otherwise the request URL of the last response.
func vwLoginLocation(resp *http.Response) (*url.URL, error) {
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, errors.New(resp.Status)
	}

	switch resp.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		location, err := resp.Location()
		if err != nil {
			return nil, fmt.Errorf("login redirect: %w", err)
		}
		return location, nil
	}

	if resp.Request == nil || resp.Request.URL == nil {
		return nil, errors.New("login response URL missing")
	}
	return resp.Request.URL, nil
}

// randomVWLetters returns n random ASCII letters (OIDC nonce).
func randomVWLetters(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	b := make([]byte, n)
	// crypto/rand never fails on supported platforms; the nonce is not secret.
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = letters[int(b[i])%len(letters)]
	}
	return string(b)
}
