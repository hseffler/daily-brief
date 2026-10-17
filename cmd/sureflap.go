package main

import (
	"context"
	"fmt"
	"time"

	gosure "github.com/perbu/go-sure"
)

// The pets source reads the inside/outside location of the user's cats from
// their SureFlap cat flap (Connect line: Cat Flap Connect + Sure Petcare Hub)
// via the Sure Petcare cloud API, using the gosure library
// (github.com/perbu/go-sure). Sure Petcare publishes no official developer
// API for these products; the library talks to the reverse-engineered app API
// at https://app-api.production.surehub.io — the same one the community
// clients use (surepy, which powers Home Assistant's surepetcare integration,
// PSSureFlap, mcp-server-surepetcare).
//
// The location is tracked by the flap: every passage through it updates the
// pet's whereabouts, and it can be overridden manually in the app. A cat that
// goes through a window or a human door is not accounted for.
//
// Library quirks to know: its methods take no context (its HTTP client has a
// fixed 30s timeout, which bounds the call), it does not re-login on an
// expired token, and each client registers a new random device id on the
// account (visible in the app).

// petFetcher fetches the user's pets with their inside/outside locations.
// The production implementation wraps the gosure library; tests use fakes.
type petFetcher interface {
	Pets(ctx context.Context) ([]petStatus, error)
}

// sureFlapClient is the production petFetcher: a thin adapter around the
// gosure library.
type sureFlapClient struct {
	client *gosure.Client
}

// newSureFlapClient creates a client for the Sure Petcare account.
func newSureFlapClient(email, password string) *sureFlapClient {
	return &sureFlapClient{client: gosure.NewClient(email, password)}
}

// Pets fetches the account's pets and maps them to petStatus.
func (c *sureFlapClient) Pets(ctx context.Context) ([]petStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pets, err := c.client.GetPets()
	if err != nil {
		return nil, fmt.Errorf("Sure Petcare request failed: %w", err)
	}
	return petStatusesFromGosure(pets), nil
}

// petLocation is a pet's inside/outside location.
type petLocation int

const (
	petLocationUnknown petLocation = iota
	petLocationInside
	petLocationOutside
)

// petStatus is one pet's location for the brief.
type petStatus struct {
	Name     string
	Location petLocation
	Since    time.Time // when the location last changed; zero when unknown
}

// petStatusesFromGosure maps the library's pets to petStatus.
func petStatusesFromGosure(pets []gosure.Pet) []petStatus {
	out := make([]petStatus, 0, len(pets))
	for _, p := range pets {
		st := petStatus{Name: p.Name}
		if p.Position != nil {
			if p.Position.Where != nil {
				switch *p.Position.Where {
				case gosure.PetPositionInside:
					st.Location = petLocationInside
				case gosure.PetPositionOutside:
					st.Location = petLocationOutside
				}
			}
			if p.Position.Since != nil {
				st.Since = *p.Position.Since
			}
		}
		out = append(out, st)
	}
	return out
}
