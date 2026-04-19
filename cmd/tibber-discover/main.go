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
	match := flag.String("match", "", "optional substring to match against vehicle name (case-insensitive); e.g. --match Ragnar")
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
		return err
	}
	if len(homes) == 0 {
		return errors.New("no homes returned; is the account correct?")
	}

	type row struct {
		homeIdx, vehIdx int
		home            tibber.Home
		veh             tibber.Vehicle
	}
	var rows []row
	fmt.Println()
	fmt.Println("Tibber homes and vehicles on this account:")
	fmt.Println()
	for hi, h := range homes {
		nick := h.AppNickname
		if nick == "" {
			nick = "(unnamed home)"
		}
		fmt.Printf("Home %d: %s  [id=%s]\n", hi, nick, h.ID)
		if len(h.Vehicles) == 0 {
			fmt.Println("  (no vehicles linked)")
			continue
		}
		for vi, v := range h.Vehicles {
			rows = append(rows, row{homeIdx: hi, vehIdx: vi, home: h, veh: v})
			idx := len(rows) - 1
			tag := ""
			if *match != "" && strings.Contains(strings.ToLower(v.Name), strings.ToLower(*match)) {
				tag = "  ← matches --match"
			}
			fmt.Printf("  [%d] %s  battery=%d%%  connected=%v  charging=%v  [vehicle_id=%s]%s\n",
				idx, v.Name, v.BatteryLevel, v.Connected, v.Charging, v.ID, tag)
		}
	}
	if len(rows) == 0 {
		return errors.New("no vehicles found on any home — link your car in the Tibber app first")
	}
	fmt.Println()

	// Autoselect if --match hits exactly one row.
	selected := -1
	if *match != "" {
		hits := 0
		for i, r := range rows {
			if strings.Contains(strings.ToLower(r.veh.Name), strings.ToLower(*match)) {
				selected = i
				hits++
			}
		}
		if hits == 0 {
			return fmt.Errorf("--match %q did not match any vehicle name", *match)
		}
		if hits > 1 {
			return fmt.Errorf("--match %q matched %d vehicles; be more specific", *match, hits)
		}
		fmt.Printf("Auto-selected row %d (%s) via --match.\n", selected, rows[selected].veh.Name)
	} else {
		fmt.Printf("Enter the row number to save as TIBBER_HOME_ID / TIBBER_VEHICLE_ID (or blank to skip): ")
		sc := bufio.NewScanner(os.Stdin)
		if sc.Scan() {
			txt := strings.TrimSpace(sc.Text())
			if txt == "" {
				fmt.Println("No selection made; nothing written.")
				return nil
			}
			n, err := strconv.Atoi(txt)
			if err != nil || n < 0 || n >= len(rows) {
				return fmt.Errorf("invalid row number %q", txt)
			}
			selected = n
		}
	}

	r := rows[selected]
	if *envOut == "" {
		fmt.Printf("\nTIBBER_HOME_ID=%s\nTIBBER_VEHICLE_ID=%s\nTIBBER_VEHICLE_NAME=%s\n",
			r.home.ID, r.veh.ID, r.veh.Name)
		return nil
	}

	if err := config.UpdateDotEnv(*envOut, map[string]string{
		"TIBBER_EMAIL":        *email,
		"TIBBER_PASSWORD":     *password,
		"TIBBER_HOME_ID":      r.home.ID,
		"TIBBER_VEHICLE_ID":   r.veh.ID,
		"TIBBER_VEHICLE_NAME": r.veh.Name,
	}); err != nil {
		return fmt.Errorf("write %s: %w", *envOut, err)
	}
	fmt.Printf("Wrote %s (TIBBER_HOME_ID, TIBBER_VEHICLE_ID=%s — %s).\n", *envOut, r.veh.ID, r.veh.Name)
	return nil
}
