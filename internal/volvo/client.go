package volvo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const APIBase = "https://api.volvocars.com"

type Client struct {
	http   *http.Client
	tokens *TokenSource
	apiKey string
	vin    string
}

func NewClient(ts *TokenSource, apiKey, vin string) *Client {
	return &Client{
		http:   &http.Client{Timeout: 30 * time.Second},
		tokens: ts,
		apiKey: apiKey,
		vin:    vin,
	}
}

// ChargeState is the flattened snapshot we log each poll.
type ChargeState struct {
	VIN                      string    `json:"vin"`
	BatteryChargeLevelPct    *float64  `json:"battery_charge_level_pct,omitempty"`
	ElectricRangeKm          *float64  `json:"electric_range_km,omitempty"`
	EstimatedChargingTimeMin *int      `json:"estimated_charging_time_min,omitempty"`
	ChargingSystemStatus     string    `json:"charging_system_status,omitempty"`
	ChargingConnectionStatus string    `json:"charging_connection_status,omitempty"`
	ObservedAt               time.Time `json:"observed_at"`
	Errors                   []string  `json:"errors,omitempty"`
}

// Energy API v1 returns single-field resources in this envelope:
//   { "data": { "value": "72", "unit": "percentage", "timestamp": "..." } }
type energyField struct {
	Data struct {
		Value     string `json:"value"`
		Unit      string `json:"unit"`
		Timestamp string `json:"timestamp"`
	} `json:"data"`
}

func (c *Client) FetchChargeState(ctx context.Context) ChargeState {
	state := ChargeState{VIN: c.vin, ObservedAt: time.Now().UTC()}
	var mu sync.Mutex
	var wg sync.WaitGroup

	addErr := func(field string, err error) {
		mu.Lock()
		defer mu.Unlock()
		state.Errors = append(state.Errors, fmt.Sprintf("%s: %v", field, err))
	}

	fetchFloat := func(path string, dst **float64) {
		defer wg.Done()
		f, err := c.fetchField(ctx, path)
		if err != nil {
			addErr(path, err)
			return
		}
		v, err := strconv.ParseFloat(f.Data.Value, 64)
		if err != nil {
			addErr(path, fmt.Errorf("parse float %q: %w", f.Data.Value, err))
			return
		}
		mu.Lock()
		*dst = &v
		mu.Unlock()
	}

	fetchInt := func(path string, dst **int) {
		defer wg.Done()
		f, err := c.fetchField(ctx, path)
		if err != nil {
			addErr(path, err)
			return
		}
		v, err := strconv.Atoi(f.Data.Value)
		if err != nil {
			addErr(path, fmt.Errorf("parse int %q: %w", f.Data.Value, err))
			return
		}
		mu.Lock()
		*dst = &v
		mu.Unlock()
	}

	fetchString := func(path string, dst *string) {
		defer wg.Done()
		f, err := c.fetchField(ctx, path)
		if err != nil {
			addErr(path, err)
			return
		}
		mu.Lock()
		*dst = f.Data.Value
		mu.Unlock()
	}

	wg.Add(5)
	go fetchFloat("battery-charge-level", &state.BatteryChargeLevelPct)
	go fetchFloat("electric-range", &state.ElectricRangeKm)
	go fetchInt("estimated-charging-time", &state.EstimatedChargingTimeMin)
	go fetchString("charging-system-status", &state.ChargingSystemStatus)
	go fetchString("charging-connection-status", &state.ChargingConnectionStatus)
	wg.Wait()

	return state
}

func (c *Client) fetchField(ctx context.Context, path string) (energyField, error) {
	u := fmt.Sprintf("%s/energy/v1/vehicles/%s/%s", APIBase, c.vin, path)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return energyField{}, err
	}
	tok, err := c.tokens.Access(ctx)
	if err != nil {
		return energyField{}, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("vcc-api-key", c.apiKey)
	req.Header.Set("accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return energyField{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return energyField{}, fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(body), 400))
	}
	var f energyField
	if err := json.Unmarshal(body, &f); err != nil {
		return energyField{}, fmt.Errorf("decode: %w (body: %s)", err, truncate(string(body), 400))
	}
	return f, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
