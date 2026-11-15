// Package config loads and saves the host agent configuration (rmtd.toml).
package config

import (
	"bytes"
	"os"
	"path/filepath"

	"rmt-desk/internal/authz"

	"github.com/BurntSushi/toml"
)

type Host struct {
	Hostname string       `toml:"hostname"`
	Port     int          `toml:"port"`
	Tags     []string     `toml:"tags"`
	Policy   authz.Policy `toml:"policy"`
}

const (
	DefaultPort = 7070
	HostTag     = "tag:rmt-host"
)

func DefaultHost() Host {
	hn, _ := os.Hostname()
	return Host{Hostname: "rmt-" + hn, Port: DefaultPort, Tags: []string{HostTag}}
}

// LoadHost reads path, returning defaults if it doesn't exist.
func LoadHost(path string) (Host, error) {
	c := DefaultHost()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	err = toml.Unmarshal(b, &c)
	return c, err
}

// SaveHost writes c to path with 0600 permissions.
func SaveHost(path string, c Host) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(c); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
