package env

import (
	"bufio"
	"os"
	"strings"
)

// Load lit un fichier .env et définit les variables d'environnement
// pour celles qui ne sont pas déjà définies dans l'environnement du processus.
// Les lignes vides et commençant par # sont ignorées.
// Silencieux si le fichier est absent.
func Load(path string) {
	f, err := os.Open(path)
	if err != nil {
		return // fichier absent — pas d'erreur
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
		// Ne pas écraser une variable déjà définie dans l'environnement
		if os.Getenv(key) == "" {
			os.Setenv(key, value)
		}
	}
}
