package env

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "" +
		"# a comment\n" +
		"RC_TEST_A=hello\n" +
		"RC_TEST_B=30  # inline comment\n" +
		"RC_TEST_C=pass#word\n" +
		"RC_TEST_EXISTING=should-not-override\n" +
		"\n" +
		"not a valid line without equals\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	keys := []string{"RC_TEST_A", "RC_TEST_B", "RC_TEST_C", "RC_TEST_EXISTING"}
	for _, k := range keys {
		os.Unsetenv(k)
	}
	os.Setenv("RC_TEST_EXISTING", "keep-me")
	t.Cleanup(func() {
		for _, k := range keys {
			os.Unsetenv(k)
		}
	})

	Load(path)

	if got := os.Getenv("RC_TEST_A"); got != "hello" {
		t.Errorf("RC_TEST_A = %q, want %q", got, "hello")
	}
	if got := os.Getenv("RC_TEST_B"); got != "30" {
		t.Errorf("RC_TEST_B = %q, want %q (inline comment stripped)", got, "30")
	}
	if got := os.Getenv("RC_TEST_C"); got != "pass#word" {
		t.Errorf("RC_TEST_C = %q, want %q ('#' with no preceding space/tab is kept)", got, "pass#word")
	}
	if got := os.Getenv("RC_TEST_EXISTING"); got != "keep-me" {
		t.Errorf("RC_TEST_EXISTING = %q, want unchanged %q (Load must not override an already-set var)", got, "keep-me")
	}
}

func TestLoad_MissingFileIsSilent(t *testing.T) {
	// Must not panic when the .env file doesn't exist.
	Load("/nonexistent/path/.env")
}
