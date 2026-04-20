package volvo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// ChargeState is the flattened snapshot logged each poll.
type ChargeState struct {
	VIN                      string    `json:"vin"`
	BatteryChargeLevelPct    *float64  `json:"battery_charge_level_pct,omitempty"`
	ElectricRangeKm          *int      `json:"electric_range_km,omitempty"`
	EstimatedChargingTimeMin *int      `json:"estimated_charging_time_min,omitempty"`
	ChargingSystemStatus     string    `json:"charging_status,omitempty"`
	ChargingConnectionStatus string    `json:"charger_connection_status,omitempty"`
	ObservedAt               time.Time `json:"observed_at"`
	Errors                   []string  `json:"errors,omitempty"`
}

// Energy API v2 returns a map of named resources. Each resource is one of
// three result types depending on whether it carries a numeric value+unit or a
// plain string. We decode into a flexible intermediate form.
type resourceFloat struct {
	Status string  `json:"status"`
	Value  float64 `json:"value"`
}

type resourceInt struct {
	Status string  `json:"status"`
	Value  float64 `json:"value"`
}

type resourceString struct {
	Status string `json:"status"`
	Value  string `json:"value"`
}

type energyStateResp struct {
	BatteryChargeLevel                          resourceFloat  `json:"batteryChargeLevel"`
	ElectricRange                               resourceInt    `json:"electricRange"`
	EstimatedChargingTimeToTargetBatteryCharge  resourceInt    `json:"estimatedChargingTimeToTargetBatteryChargeLevel"`
	ChargingStatus                              resourceString `json:"chargingStatus"`
	ChargerConnectionStatus                     resourceString `json:"chargerConnectionStatus"`
}

func (c *Client) FetchChargeState(ctx context.Context) ChargeState {
	state := ChargeState{VIN: c.vin, ObservedAt: time.Now().UTC()}

	u := fmt.Sprintf("%s/energy/v2/vehicles/%s/state", APIBase, c.vin)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		state.Errors = append(state.Errors, fmt.Sprintf("build request: %v", err))
		return state
	}
	tok, err := c.tokens.Access(ctx)
	if err != nil {
		state.Errors = append(state.Errors, fmt.Sprintf("get token: %v", err))
		return state
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("vcc-api-key", c.apiKey)
	req.Header.Set("accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		state.Errors = append(state.Errors, fmt.Sprintf("request: %v", err))
		return state
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	if resp.StatusCode != http.StatusOK {
		state.Errors = append(state.Errors, fmt.Sprintf("status %d: %s", resp.StatusCode, truncate(string(body), 400)))
		return state
	}

	var e energyStateResp
	if err := json.Unmarshal(body, &e); err != nil {
		state.Errors = append(state.Errors, fmt.Sprintf("decode: %v (body: %s)", err, truncate(string(body), 400)))
		return state
	}

	if e.BatteryChargeLevel.Status == "OK" {
		v := e.BatteryChargeLevel.Value
		state.BatteryChargeLevelPct = &v
	}
	if e.ElectricRange.Status == "OK" {
		v := int(e.ElectricRange.Value)
		state.ElectricRangeKm = &v
	}
	if e.EstimatedChargingTimeToTargetBatteryCharge.Status == "OK" {
		v := int(e.EstimatedChargingTimeToTargetBatteryCharge.Value)
		state.EstimatedChargingTimeMin = &v
	}
	if e.ChargingStatus.Status == "OK" {
		state.ChargingSystemStatus = e.ChargingStatus.Value
	}
	if e.ChargerConnectionStatus.Status == "OK" {
		state.ChargingConnectionStatus = e.ChargerConnectionStatus.Value
	}

	return state
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
