// Command tibber-discover logs into Tibber's app API with email and password,
// lists every home + electric vehicle on the account, prints them, and
// optionally writes the matching TIBBER_HOME_ID / TIBBER_VEHICLE_ID pair
// for the selected vehicle into a .env file.
//
// Run it once after registering your car in the Tibber app. The monitor
// service then uses the IDs directly and never needs to list again.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tokko/volvo-tibber-sync/internal/config"
	"github.com/tokko/volvo-tibber-sync/internal/tibber"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	_ = config.LoadDotEnv(".env")

	email := flag.String("email", os.Getenv("TIBBER_EMAIL"), "Tibber account email")
	password := flag.String("password", os.Getenv("TIBBER_PASSWORD"), "Tibber account password (use env TIBBER_PASSWORD to avoid shell history)")
	match := flag.String("match", "", "optional substring to match against vehicle title (case-insensitive); e.g. --match Ragnar")
	envOut := flag.String("out", ".env", "path to merge selection into; empty to just print")
	flag.Parse()

	if *email == "" || *password == "" {
		flag.Usage()
		return errors.New("email and password are required (flags or env TIBBER_EMAIL/TIBBER_PASSWORD)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess := tibber.NewSession(*email, *password)
	client := tibber.NewClient(sess)

	homes, err := client.ListHomes(ctx)
	if err != nil {
		return fmt.Errorf("list homes: %w", err)
	}
	if len(homes) == 0 {
		return errors.New("no homes returned; is the account correct?")
	}

	vehicles, err := client.ListVehicles(ctx)
	if err != nil {
		return fmt.Errorf("list vehicles: %w", err)
	}
	if len(vehicles) == 0 {
		return errors.New("no vehicles found on this account — link your car in the Tibber app first")
	}

	fmt.Println()
	fmt.Println("Tibber homes on this account:")
	fmt.Println()
	for hi, h := range homes {
		fmt.Printf("  [%d] home id=%s\n", hi, h.ID)
	}

	fmt.Println()
	fmt.Println("Electric vehicles on this account:")
	fmt.Println()
	for vi, v := range vehicles {
		tag := ""
		if *match != "" && strings.Contains(strings.ToLower(v.DisplayName()), strings.ToLower(*match)) {
			tag = "  ← matches --match"
		}
		fmt.Printf("  [%d] %s  [vehicle_id=%s]%s\n", vi, v.DisplayName(), v.ID, tag)
	}
	fmt.Println()

	// Select vehicle.
	selectedVeh := -1
	if *match != "" {
		hits := 0
		for i, v := range vehicles {
			if strings.Contains(strings.ToLower(v.DisplayName()), strings.ToLower(*match)) {
				selectedVeh = i
				hits++
			}
		}
		if hits == 0 {
			return fmt.Errorf("--match %q did not match any vehicle title", *match)
		}
		if hits > 1 {
			return fmt.Errorf("--match %q matched %d vehicles; be more specific", *match, hits)
		}
		fmt.Printf("Auto-selected vehicle %d (%s) via --match.\n", selectedVeh, vehicles[selectedVeh].DisplayName())
	} else {
		sc := bufio.NewScanner(os.Stdin)
		fmt.Printf("Enter vehicle row number to save (or blank to skip): ")
		if sc.Scan() {
			txt := strings.TrimSpace(sc.Text())
			if txt == "" {
				fmt.Println("No selection made; nothing written.")
				return nil
			}
			n, err := strconv.Atoi(txt)
			if err != nil || n < 0 || n >= len(vehicles) {
				return fmt.Errorf("invalid row number %q", txt)
			}
			selectedVeh = n
		}
	}

	// Select home: auto if only one, else prompt.
	selectedHome := 0
	if len(homes) > 1 {
		sc := bufio.NewScanner(os.Stdin)
		fmt.Printf("Enter home row number to associate with this vehicle: ")
		if sc.Scan() {
			txt := strings.TrimSpace(sc.Text())
			n, err := strconv.Atoi(txt)
			if err != nil || n < 0 || n >= len(homes) {
				return fmt.Errorf("invalid row number %q", txt)
			}
			selectedHome = n
		}
	} else {
		fmt.Printf("Using only home: %s\n", homes[0].ID)
	}

	veh := vehicles[selectedVeh]
	home := homes[selectedHome]

	var chargerEnv map[string]string
	cancel()
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	gizmos, err := client.ListHomeGizmos(ctx, home.ID)
	if err != nil {
		return fmt.Errorf("list home chargers: %w", err)
	}
	var chargers []tibber.Gizmo
	for _, g := range gizmos {
		if g.Type == "EV_CHARGER" {
			chargers = append(chargers, g)
		}
	}
	var selectedCharger tibber.Gizmo
	var preferredVehicleName string
	if len(chargers) > 0 {
		fmt.Println("\nEV chargers on the selected home:")
		for i, charger := range chargers {
			fmt.Printf("  [%d] %s  [charger_id=%s]\n", i, charger.Name, charger.ID)
		}
		chargerIndex, err := promptRow("Enter charger row number to configure (or blank to skip): ", len(chargers), true)
		if err != nil {
			return err
		}
		if chargerIndex >= 0 {
			selectedCharger = chargers[chargerIndex]
			cancel()
			ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			charger, err := client.GetCharger(ctx, home.ID, selectedCharger.ID)
			if err != nil {
				return fmt.Errorf("read charger %q: %w", selectedCharger.Name, err)
			}
			if charger.ID != selectedCharger.ID || charger.Name != selectedCharger.Name || charger.Type != "EV_CHARGER" {
				return fmt.Errorf("charger identity did not match the selected home gizmo")
			}
			if len(charger.Vehicles) == 0 {
				return fmt.Errorf("charger %q has no linked vehicles", selectedCharger.Name)
			}
			chargerEnv = make(map[string]string, len(charger.Vehicles))
			fmt.Println("\nCars linked to the selected charger:")
			for i, car := range charger.Vehicles {
				if car.Name == "" || car.ID == "" {
					return fmt.Errorf("charger returned a car with a missing name or ID")
				}
				if _, duplicate := chargerEnv[car.Name]; duplicate {
					return fmt.Errorf("charger returned duplicate car name %q; no selection was saved", car.Name)
				}
				chargerEnv[car.Name] = car.ID
				fmt.Printf("  [%d] %s  [vehicle_id=%s]\n", i, car.Name, car.ID)
			}
			preferredIndex, err := promptRow("Enter preferred car row number (or blank to skip): ", len(charger.Vehicles), true)
			if err != nil {
				return err
			}
			if preferredIndex >= 0 {
				preferredVehicleName = charger.Vehicles[preferredIndex].Name
			}
		}
	} else {
		fmt.Println("\nNo EV_CHARGER gizmos found on the selected home; charger selection was skipped.")
	}

	if *envOut == "" {
		fmt.Printf("\nTIBBER_HOME_ID=%s\nTIBBER_VEHICLE_ID=%s\nTIBBER_VEHICLE_NAME=%s\n",
			home.ID, veh.ID, veh.Title)
		if selectedCharger.ID != "" && preferredVehicleName != "" {
			mapping := encodeChargerVehicleMap(chargerEnv)
			fmt.Printf("TIBBER_CHARGER_NAME=%s\nTIBBER_CHARGER_ID=%s\nTIBBER_CHARGER_VEHICLES=%s\nTIBBER_PREFERRED_VEHICLE_NAME=%s\n",
				selectedCharger.Name, selectedCharger.ID, mapping, preferredVehicleName)
		}
		return nil
	}

	values := map[string]string{
		"TIBBER_EMAIL":                  *email,
		"TIBBER_PASSWORD":               *password,
		"TIBBER_HOME_ID":                home.ID,
		"TIBBER_VEHICLE_ID":             veh.ID,
		"TIBBER_VEHICLE_NAME":           veh.DisplayName(),
		"TIBBER_CHARGER_NAME":           "",
		"TIBBER_CHARGER_ID":             "",
		"TIBBER_CHARGER_VEHICLES":       "",
		"TIBBER_PREFERRED_VEHICLE_NAME": "",
	}
	if selectedCharger.ID != "" && preferredVehicleName != "" {
		mapping := encodeChargerVehicleMap(chargerEnv)
		values["TIBBER_CHARGER_NAME"] = selectedCharger.Name
		values["TIBBER_CHARGER_ID"] = selectedCharger.ID
		values["TIBBER_CHARGER_VEHICLES"] = string(mapping)
		values["TIBBER_PREFERRED_VEHICLE_NAME"] = preferredVehicleName
	}
	if err := config.UpdateDotEnv(*envOut, values); err != nil {
		return fmt.Errorf("write %s: %w", *envOut, err)
	}
	fmt.Printf("Wrote %s (TIBBER_HOME_ID=%s, TIBBER_VEHICLE_ID=%s — %s).\n",
		*envOut, home.ID, veh.ID, veh.DisplayName())
	return nil
}

func encodeChargerVehicleMap(cars map[string]string) string {
	values := make(url.Values, len(cars))
	for name, id := range cars {
		values.Set(name, id)
	}
	return values.Encode()
}

func promptRow(prompt string, count int, optional bool) (int, error) {
	sc := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print(prompt)
		if !sc.Scan() {
			if err := sc.Err(); err != nil {
				return -1, err
			}
			return -1, errors.New("could not read selection")
		}
		txt := strings.TrimSpace(sc.Text())
		if txt == "" && optional {
			return -1, nil
		}
		n, err := strconv.Atoi(txt)
		if err == nil && n >= 0 && n < count {
			return n, nil
		}
		fmt.Printf("Enter a number between 0 and %d", count-1)
		if optional {
			fmt.Print(" or leave blank")
		}
		fmt.Println(".")
	}
}
