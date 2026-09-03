package config

import "gopkg.in/yaml.v3"

// unmarshalYAML decodes YAML bytes into the provided Config, leaving fields
// absent from the document at their existing (default) values.
func unmarshalYAML(data []byte, cfg *Config) error {
	return yaml.Unmarshal(data, cfg)
}
