package tibber

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	http    *http.Client
	session *Session
}

func NewClient(s *Session) *Client {
	return &Client{
		http:    &http.Client{Timeout: 30 * time.Second},
		session: s,
	}
}

type Vehicle struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	ShortName string `json:"shortName"`
}

// ChargerVehicle is a car linked to a home's EV charger.
type ChargerVehicle struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	IsPreferred *bool  `json:"isPreferred"`
}

type Charger struct {
	ID                 string           `json:"id"`
	Type               string           `json:"type"`
	Name               string           `json:"name"`
	Vehicles           []ChargerVehicle `json:"vehicles"`
	PreferredVehicleID string           `json:"preferredVehicleId"`
}

// DisplayName returns the most descriptive available name for the vehicle.
func (v Vehicle) DisplayName() string {
	if v.ShortName != "" && v.ShortName != v.Title {
		return v.ShortName + " (" + v.Title + ")"
	}
	if v.Title != "" {
		return v.Title
	}
	return v.ID
}

type Home struct {
	ID string `json:"id"`
}

const listHomesQuery = `query ListHomes {
  me {
    homes {
      id
    }
  }
}`

const listVehiclesQuery = `query ListVehicles {
  me {
    myVehicles {
      vehicles {
        id
        title
      }
    }
  }
}`

const setVehicleSettingsMutation = `mutation SetVehicleSettings($vehicleId: String!, $homeId: String!, $settings: [SettingsItemInput!]) {
  me {
    setVehicleSettings(id: $vehicleId, homeId: $homeId, settings: $settings) {
      __typename
    }
  }
}`

const homeGizmosQuery = `query HomeGizmos($homeId: String!) {
  me {
    home(id: $homeId) {
      id
      gizmos {
        ... on Gizmo { id title type context { key value } }
        ... on GizmoGroup { gizmos { id title type } }
      }
    }
  }
}`

const chargerQuery = `query Charger($homeId: String!, $chargerId: String!) {
  me {
    home(id: $homeId) {
      id
      vehicleCharger(id: $chargerId) {
        id
        name
        vehicles { id name isPreferred }
        userSettings { key value }
      }
    }
  }
}`

const setChargerSettingsMutation = `mutation SetVehicleChargerSettings($chargerId: String!, $homeId: String!, $settings: [SettingsItemInput!]) {
  me {
    home(id: $homeId) {
      setVehicleChargerSettings(id: $chargerId, homeId: $homeId, settings: $settings) {
        __typename
      }
    }
  }
}`

// ListHomeGizmos returns the configured home's gizmos for charger resolution.
func (c *Client) ListHomeGizmos(ctx context.Context, homeID string) ([]Gizmo, error) {
	var out struct {
		Me struct {
			Home *struct {
				ID     string `json:"id"`
				Gizmos []struct {
					ID     string  `json:"id"`
					Type   string  `json:"type"`
					Title  string  `json:"title"`
					Gizmos []Gizmo `json:"gizmos"`
				} `json:"gizmos"`
			} `json:"home"`
		} `json:"me"`
	}
	if err := c.do(ctx, homeGizmosQuery, map[string]any{"homeId": homeID}, &out); err != nil {
		return nil, err
	}
	if out.Me.Home == nil || out.Me.Home.ID != homeID {
		return nil, fmt.Errorf("tibber home %q was not returned", homeID)
	}
	var gizmos []Gizmo
	for _, item := range out.Me.Home.Gizmos {
		if item.ID != "" || item.Type != "" || item.Title != "" {
			gizmos = append(gizmos, Gizmo{ID: item.ID, Type: item.Type, Name: item.Title})
		}
		gizmos = append(gizmos, item.Gizmos...)
	}
	return gizmos, nil
}

type Gizmo struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Name string `json:"title"`
}

// GetCharger reads both preferred-car representations and the charger's cars.
func (c *Client) GetCharger(ctx context.Context, homeID, chargerID string) (Charger, error) {
	var out struct {
		Me struct {
			Home *struct {
				ID             string `json:"id"`
				VehicleCharger *struct {
					ID           string           `json:"id"`
					Name         string           `json:"name"`
					Vehicles     []ChargerVehicle `json:"vehicles"`
					UserSettings []userSetting    `json:"userSettings"`
				} `json:"vehicleCharger"`
			} `json:"home"`
		} `json:"me"`
	}
	if err := c.do(ctx, chargerQuery, map[string]any{"homeId": homeID, "chargerId": chargerID}, &out); err != nil {
		return Charger{}, err
	}
	if out.Me.Home == nil || out.Me.Home.ID != homeID {
		return Charger{}, fmt.Errorf("tibber home %q was not returned", homeID)
	}
	g := out.Me.Home.VehicleCharger
	if g == nil {
		return Charger{}, fmt.Errorf("tibber charger %q was not returned for home %q", chargerID, homeID)
	}
	if g.ID != chargerID {
		return Charger{}, fmt.Errorf("tibber charger response ID %q does not match configured ID %q", g.ID, chargerID)
	}
	preferredVehicleID, err := preferredChargerVehicleID(g.UserSettings)
	if err != nil {
		return Charger{}, fmt.Errorf("tibber charger %q: %w", chargerID, err)
	}
	return Charger{
		ID:                 g.ID,
		Type:               "EV_CHARGER",
		Name:               g.Name,
		Vehicles:           g.Vehicles,
		PreferredVehicleID: preferredVehicleID,
	}, nil
}

type userSetting struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

func preferredChargerVehicleID(settings []userSetting) (string, error) {
	var preferredVehicleID string
	matches := 0
	for _, setting := range settings {
		if setting.Key != "preferredVehicleId" {
			continue
		}
		matches++
		if matches > 1 {
			return "", fmt.Errorf("charger returned duplicate preferredVehicleId settings")
		}
		if len(setting.Value) == 0 || string(bytes.TrimSpace(setting.Value)) == "null" {
			return "", fmt.Errorf("charger returned an empty preferredVehicleId setting")
		}
		if err := json.Unmarshal(setting.Value, &preferredVehicleID); err != nil || preferredVehicleID == "" {
			return "", fmt.Errorf("charger returned an invalid preferredVehicleId setting")
		}
	}
	if matches == 0 {
		return "", fmt.Errorf("charger returned no preferredVehicleId setting")
	}
	return preferredVehicleID, nil
}

// SetPreferredVehicleID saves the user's charger preference. Applying it to
// the current charger selection is a separate request via SelectPreferredVehicle.
func (c *Client) SetPreferredVehicleID(ctx context.Context, homeID, chargerID, vehicleID string) error {
	vars := map[string]any{
		"chargerId": chargerID,
		"homeId":    homeID,
		"settings":  []map[string]any{{"key": "preferredVehicleId", "value": vehicleID}},
	}
	return c.do(ctx, setChargerSettingsMutation, vars, nil)
}

// SelectPreferredVehicle applies the saved preference to the live charger.
func (c *Client) SelectPreferredVehicle(ctx context.Context, homeID, chargerID, vehicleID string) error {
	vars := map[string]any{
		"chargerId": chargerID,
		"homeId":    homeID,
		"settings":  []map[string]any{{"key": "selectPreferredVehicle", "value": vehicleID}},
	}
	return c.do(ctx, setChargerSettingsMutation, vars, nil)
}

// ListHomes returns every home ID on the account.
func (c *Client) ListHomes(ctx context.Context) ([]Home, error) {
	var out struct {
		Me struct {
			Homes []Home `json:"homes"`
		} `json:"me"`
	}
	if err := c.do(ctx, listHomesQuery, nil, &out); err != nil {
		return nil, err
	}
	return out.Me.Homes, nil
}

// ListVehicles returns all electric vehicles linked to the account.
func (c *Client) ListVehicles(ctx context.Context) ([]Vehicle, error) {
	var out struct {
		Me struct {
			MyVehicles struct {
				Vehicles []Vehicle `json:"vehicles"`
			} `json:"myVehicles"`
		} `json:"me"`
	}
	if err := c.do(ctx, listVehiclesQuery, nil, &out); err != nil {
		return nil, err
	}
	return out.Me.MyVehicles.Vehicles, nil
}

// SetBatteryLevel writes the supplied state-of-charge percentage against the
// given vehicle in the given home. Pct must be 0..100.
func (c *Client) SetBatteryLevel(ctx context.Context, homeID, vehicleID string, pct int) error {
	if pct < 0 || pct > 100 {
		return fmt.Errorf("battery percent out of range: %d", pct)
	}
	vars := map[string]any{
		"vehicleId": vehicleID,
		"homeId":    homeID,
		"settings": []map[string]any{
			{"key": "offline.vehicle.batteryLevel", "value": pct},
		},
	}
	return c.do(ctx, setVehicleSettingsMutation, vars, nil)
}

type gqlError struct {
	Message string `json:"message"`
}

func (c *Client) do(ctx context.Context, query string, vars map[string]any, out any) error {
	// First attempt with whatever token we have cached, plus one retry after
	// forced re-login on an auth failure. Tibber invalidates prior sessions
	// whenever the user logs in elsewhere (e.g. opens the app on a new phone),
	// long before our hardcoded TTL would have expired, so we must recover.
	err := c.doOnce(ctx, query, vars, out)
	if err != nil && isAuthFailure(err) {
		c.session.Invalidate()
		err = c.doOnce(ctx, query, vars, out)
	}
	return err
}

func (c *Client) doOnce(ctx context.Context, query string, vars map[string]any, out any) error {
	tok, err := c.session.Token(ctx)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"query":     query,
		"variables": vars,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", GQLEndpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", "https://app.tibber.com")
	req.Header.Set("Referer", "https://app.tibber.com/")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("tibber gql request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<17))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("tibber gql returned %d: %s", resp.StatusCode, truncate(string(raw), 400))
	}

	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []gqlError      `json:"errors,omitempty"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode tibber gql: %w (body: %s)", err, truncate(string(raw), 400))
	}
	if len(env.Errors) > 0 {
		msgs := make([]string, 0, len(env.Errors))
		for _, e := range env.Errors {
			msgs = append(msgs, e.Message)
		}
		return fmt.Errorf("tibber gql errors: %v", msgs)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("decode tibber gql data: %w", err)
	}
	return nil
}

// isAuthFailure matches the two shapes Tibber returns when a JWT is dead:
// a HTTP 401, or a HTTP 200 with a GraphQL error message containing
// "Not authenticated" / "UNAUTHENTICATED".
func isAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "returned 401") ||
		strings.Contains(msg, "Not authenticated") ||
		strings.Contains(msg, "UNAUTHENTICATED")
}
