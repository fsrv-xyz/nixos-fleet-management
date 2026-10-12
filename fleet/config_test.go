package fleet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfig(t *testing.T) {
	type config struct {
		Servers  []string `json:"servers"`
		Interval Duration `json:"interval"`
		Certname string   `json:"certname"`
	}
	write := func(content string) string {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	c := config{Interval: Duration(10 * time.Second), Certname: "default"}
	if err := LoadConfig(write(`{"servers": ["a:1", "b:2"], "interval": "1m"}`), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Servers) != 2 || time.Duration(c.Interval) != time.Minute || c.Certname != "default" {
		t.Errorf("config = %+v", c)
	}

	for content, want := range map[string]string{
		`{"intervall": "1m"}`: "unknown field",
		`{"interval": 60}`:    "duration must be a string",
		`{"interval": "1x"}`:  "unknown unit",
	} {
		if err := LoadConfig(write(content), &c); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want error containing %q", content, err, want)
		}
	}

	c = config{Certname: "default"}
	if err := LoadConfig("", &c); err != nil || c.Certname != "default" {
		t.Errorf("without a path: %+v, %v", c, err)
	}
}
