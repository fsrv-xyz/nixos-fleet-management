package fleet

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// LoadConfig decodes the JSON file at path over the defaults already in config.
// An empty path keeps the defaults; unknown fields are an error, so typos surface.
func LoadConfig(path string, config any) error {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(config); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// Duration reads durations such as "30s" from JSON.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return fmt.Errorf("duration must be a string such as \"30s\": %w", err)
	}
	parsed, err := time.ParseDuration(text)
	*d = Duration(parsed)
	return err
}
