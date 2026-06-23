package env

import (
	"bufio"
	"os"
	"strings"
)

// Load reads a .env file and sets environment variables not already defined.
// Empty lines and lines starting with # are ignored. Silent if the file is absent.
func Load(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		// Strip inline comments: a '#' preceded by at least one space or tab
		// (e.g. "30  # comment" → "30"). This matches common .env conventions
		// without mangling hash characters embedded in values like passphrases.
		for i := 1; i < len(value); i++ {
			if value[i] == '#' && (value[i-1] == ' ' || value[i-1] == '\t') {
				value = strings.TrimSpace(value[:i])
				break
			}
		}
		if os.Getenv(key) == "" {
			os.Setenv(key, value)
		}
	}
}
