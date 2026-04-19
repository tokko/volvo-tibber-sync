package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// UpdateDotEnv merges the given key/value pairs into path. Existing lines are
// preserved (including comments and ordering); matching KEY= lines are
// rewritten in place; new keys are appended. If the file doesn't exist it is
// created. Values containing whitespace are quoted.
//
// The file is written with 0600 permissions.
func UpdateDotEnv(path string, kv map[string]string) error {
	var existing []string
	seen := make(map[string]bool, len(kv))

	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Text()
			key := parseKey(line)
			if key == "" {
				existing = append(existing, line)
				continue
			}
			if v, ok := kv[key]; ok {
				existing = append(existing, fmt.Sprintf("%s=%s", key, quoteIfNeeded(v)))
				seen[key] = true
			} else {
				existing = append(existing, line)
			}
		}
		if err := sc.Err(); err != nil {
			_ = f.Close()
			return err
		}
		_ = f.Close()
	} else if !os.IsNotExist(err) {
		return err
	}

	// Append any keys that weren't already present.
	for k, v := range kv {
		if !seen[k] {
			existing = append(existing, fmt.Sprintf("%s=%s", k, quoteIfNeeded(v)))
		}
	}

	tmp := path + ".tmp"
	out := strings.Join(existing, "\n")
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	if err := os.WriteFile(tmp, []byte(out), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func parseKey(line string) string {
	s := strings.TrimSpace(line)
	if s == "" || strings.HasPrefix(s, "#") {
		return ""
	}
	eq := strings.IndexByte(s, '=')
	if eq <= 0 {
		return ""
	}
	return strings.TrimSpace(s[:eq])
}

func quoteIfNeeded(v string) string {
	if strings.ContainsAny(v, " \t\"'`$#") {
		return `"` + strings.ReplaceAll(v, `"`, `\"`) + `"`
	}
	return v
}
