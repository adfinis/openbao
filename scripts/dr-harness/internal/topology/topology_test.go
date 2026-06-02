package topology

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEnvParsesExportedQuotedValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".dr-test.env")
	if err := os.WriteFile(path, []byte(`
# generated
export DR_PRIMARY_TOKEN="primary-token"
export DR_SECONDARY1_TOKEN='secondary-one'
export DR_SECONDARY2_TOKEN=secondary-two
export DR_PRIMARY_ADDR="http://localhost:8800"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	env, err := LoadEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"DR_PRIMARY_TOKEN":    "primary-token",
		"DR_SECONDARY1_TOKEN": "secondary-one",
		"DR_SECONDARY2_TOKEN": "secondary-two",
		"DR_PRIMARY_ADDR":     "http://localhost:8800",
	} {
		if got := env[key]; got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestLayoutFromEnvHAUsesDefaultNodeAddrs(t *testing.T) {
	env := Env{
		"DR_PRIMARY_TOKEN":    "primary",
		"DR_SECONDARY1_TOKEN": "secondary1",
		"DR_SECONDARY2_TOKEN": "secondary2",
		"DR_PRIMARY_ADDR":     "http://localhost:8800",
		"DR_SECONDARY1_ADDR":  "http://localhost:8900",
		"DR_SECONDARY2_ADDR":  "http://localhost:9000",
	}

	layout, err := LayoutFromEnv("ha", env)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := layout.Primary.Addrs[1], "http://localhost:8802"; got != want {
		t.Fatalf("primary node 2 addr = %q, want %q", got, want)
	}
	if got, want := layout.Secondary2.Addrs[2], "http://localhost:9004"; got != want {
		t.Fatalf("secondary2 node 3 addr = %q, want %q", got, want)
	}
}
