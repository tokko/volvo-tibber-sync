// Package config loads .env files and environment variables.
// Stdlib-only to keep the binary small and dependency-free.
package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// LoadDotEnv reads KEY=VALUE lines from path and sets them in the process
// environment unless already set. Missing file is not an error.
func LoadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			return fmt.Errorf("%s:%d: malformed line", path, lineNo)
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"'`)
		if _, set := os.LookupEnv(key); !set {
			_ = os.Setenv(key, val)
		}
	}
	return sc.Err()
}

// Require returns the value of key or an error if it's empty.
func Require(key string) (string, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return "", fmt.Errorf("missing required env var %s", key)
	}
	return v, nil
}

// Optional returns the value of key or fallback if unset.
func Optional(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
}
