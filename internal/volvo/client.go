package volvo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

const APIBase = "https://api.volvocars.com"

type Client struct {
	http   *http.Client
	tokens *TokenSource
	apiKey string
	vin    string

	// logDoorsOnce logs the raw /doors payload the first time it's fetched, so
	// the operator can confirm the field names (centralLock vs carLocked, the
	// door keys) on a real vehicle without turning it into per-call log noise.
	logDoorsOnce sync.Once
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
	BatteryChargeLevel                         resourceFloat  `json:"batteryChargeLevel"`
	ElectricRange                              resourceInt    `json:"electricRange"`
	EstimatedChargingTimeToTargetBatteryCharge resourceInt    `json:"estimatedChargingTimeToTargetBatteryChargeLevel"`
	ChargingStatus                             resourceString `json:"chargingStatus"`
	ChargerConnectionStatus                    resourceString `json:"chargerConnectionStatus"`
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

// LockStatus is the flattened door/lock snapshot from the Connected Vehicle API.
type LockStatus struct {
	CentralLock string    // normalized upper: "LOCKED"/"UNLOCKED"/"" if absent
	AnyDoorOpen bool      // any non-lock part reported OPEN
	OpenParts   []string  // names of the parts reporting OPEN (for first-run inspection)
	DoorsKnown  bool      // true only if >=1 door part decoded — the guard is trustworthy
	ObservedAt  time.Time // centralLock timestamp, for staleness visibility
	Errors      []string  // HTTP-layer failures; carries the raw body for 403 classification
}

// LockResult is the outcome of a lock command.
type LockResult struct {
	InvokeStatus string // "COMPLETED" on success; "" if the request failed before decode
	Message      string
	Errors       []string
}

// connField mirrors a Connected Vehicle API value object: {value, timestamp}.
type connField struct {
	Value     string    `json:"value"`
	Timestamp time.Time `json:"timestamp"`
}

// doorsResp decodes the /doors envelope. Unlike the Energy endpoint, Connected
// Vehicle wraps its resources in "data". We decode into a map so we don't have
// to hard-code every door key (which varies by model) — the lock field is
// centralLock (older models: carLocked); every other key is a door/hatch/lid.
type doorsResp struct {
	Data map[string]connField `json:"data"`
}

type lockResp struct {
	Data struct {
		InvokeStatus string `json:"invokeStatus"`
		Message      string `json:"message"`
	} `json:"data"`
}

// FetchLockStatus reads central-lock and per-door open/closed state.
func (c *Client) FetchLockStatus(ctx context.Context) LockStatus {
	st := LockStatus{}

	u := fmt.Sprintf("%s/connected-vehicle/v2/vehicles/%s/doors", APIBase, c.vin)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		st.Errors = append(st.Errors, fmt.Sprintf("build request: %v", err))
		return st
	}
	tok, err := c.tokens.Access(ctx)
	if err != nil {
		st.Errors = append(st.Errors, fmt.Sprintf("get token: %v", err))
		return st
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("vcc-api-key", c.apiKey)
	req.Header.Set("accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		st.Errors = append(st.Errors, fmt.Sprintf("request: %v", err))
		return st
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	if resp.StatusCode != http.StatusOK {
		st.Errors = append(st.Errors, fmt.Sprintf("status %d: %s", resp.StatusCode, truncate(string(body), 400)))
		return st
	}

	c.logDoorsOnce.Do(func() {
		slog.Info("connected-vehicle /doors raw payload (first fetch — confirm field names)", "body", truncate(string(body), 800))
	})

	var dr doorsResp
	if err := json.Unmarshal(body, &dr); err != nil {
		st.Errors = append(st.Errors, fmt.Sprintf("decode: %v (body: %s)", err, truncate(string(body), 400)))
		return st
	}

	for key, f := range dr.Data {
		val := strings.ToUpper(strings.TrimSpace(f.Value))
		switch key {
		case "centralLock", "carLocked":
			st.CentralLock = val
			st.ObservedAt = f.Timestamp
		default:
			// Any other key is a physical part (door/hood/tailgate/tankLid).
			st.DoorsKnown = true
			if val == "OPEN" {
				st.AnyDoorOpen = true
				st.OpenParts = append(st.OpenParts, key)
			}
		}
	}

	return st
}

// Lock sends the central-lock command. The command is asynchronous: a non-error
// result carries an invokeStatus ("COMPLETED" means the car confirmed the lock).
func (c *Client) Lock(ctx context.Context) LockResult {
	res := LockResult{}

	u := fmt.Sprintf("%s/connected-vehicle/v2/vehicles/%s/commands/lock", APIBase, c.vin)
	req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader([]byte("{}")))
	if err != nil {
		res.Errors = append(res.Errors, fmt.Sprintf("build request: %v", err))
		return res
	}
	tok, err := c.tokens.Access(ctx)
	if err != nil {
		res.Errors = append(res.Errors, fmt.Sprintf("get token: %v", err))
		return res
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("vcc-api-key", c.apiKey)
	req.Header.Set("accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		res.Errors = append(res.Errors, fmt.Sprintf("request: %v", err))
		return res
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	// The command endpoint answers 200 (sync) or 202 (accepted); both carry an
	// invokeStatus body. Anything else is an HTTP-layer failure.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		res.Errors = append(res.Errors, fmt.Sprintf("status %d: %s", resp.StatusCode, truncate(string(body), 400)))
		return res
	}

	var lr lockResp
	if err := json.Unmarshal(body, &lr); err != nil {
		res.Errors = append(res.Errors, fmt.Sprintf("decode: %v (body: %s)", err, truncate(string(body), 400)))
		return res
	}
	res.InvokeStatus = lr.Data.InvokeStatus
	res.Message = lr.Data.Message
	return res
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
