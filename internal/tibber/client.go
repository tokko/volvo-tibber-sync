package tibber

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	ID           string `json:"id"`
	Name         string `json:"name"`
	Model        string `json:"model,omitempty"`
	BatteryLevel int    `json:"batteryLevel"`
	Connected    bool   `json:"connected"`
	Charging     bool   `json:"charging"`
}

type Home struct {
	ID          string    `json:"id"`
	AppNickname string    `json:"appNickname"`
	Vehicles    []Vehicle `json:"vehicles"`
}

const listHomesQuery = `query ListHomesVehicles {
  me {
    homes {
      id
      appNickname
      vehicles {
        id
        name
        batteryLevel
        connected
        charging
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

// ListHomes returns every home on the account along with its vehicles.
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
