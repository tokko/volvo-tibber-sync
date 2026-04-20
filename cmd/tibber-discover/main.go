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

	if *envOut == "" {
		fmt.Printf("\nTIBBER_HOME_ID=%s\nTIBBER_VEHICLE_ID=%s\nTIBBER_VEHICLE_NAME=%s\n",
			home.ID, veh.ID, veh.Title)
		return nil
	}

	if err := config.UpdateDotEnv(*envOut, map[string]string{
		"TIBBER_EMAIL":        *email,
		"TIBBER_PASSWORD":     *password,
		"TIBBER_HOME_ID":      home.ID,
		"TIBBER_VEHICLE_ID":   veh.ID,
		"TIBBER_VEHICLE_NAME": veh.DisplayName(),
	}); err != nil {
		return fmt.Errorf("write %s: %w", *envOut, err)
	}
	fmt.Printf("Wrote %s (TIBBER_HOME_ID=%s, TIBBER_VEHICLE_ID=%s — %s).\n",
		*envOut, home.ID, veh.ID, veh.DisplayName())
	return nil
}
