// Package config reads the agent's settings from the environment.
//
// Everything about this machine — where docker is, which ports may be used,
// what to bind to — lives here rather than in the control plane. A laptop is
// configured by whoever owns it, and its docker socket is nobody else's
// business.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	EnvStateManagerURL  = "STATE_MANAGER_URL"
	EnvInformerURL      = "INFORMER_URL"
	EnvInformerUsername = "INFORMER_USERNAME"
	EnvInformerPassword = "INFORMER_PASSWORD"
	EnvInformerTLS      = "INFORMER_ENABLE_TLS"

	// EnvShardID is the location this agent serves. Runtimes created for that
	// location carry it as their shard, which is how they reach this machine
	// and not the Kubernetes controller.
	EnvShardID = "SHARD_ID"

	EnvBindAddress  = "BIND_ADDRESS"
	EnvPortMin      = "PORT_MIN"
	EnvPortMax      = "PORT_MAX"
	EnvExternalHost = "EXTERNAL_HOST"
	EnvDockerBinary = "DOCKER_BINARY"
	// EnvLocalUserConfig is a file this machine keeps for the boxes it runs: a
	// profile in the platform's own shape, holding what its owner would rather
	// not put on a platform other people read.
	EnvLocalUserConfig = "LOCAL_USER_CONFIG_PATH"

	// EnvLocalPortMin is where the search for a box's own ports starts. These
	// are published with the same number inside and out, so whatever a person
	// starts in a box on one of them is reachable from the machine at the
	// address the box itself reports.
	EnvLocalPortMin   = "LOCAL_PORT_MIN"
	EnvLocalPortCount = "LOCAL_PORT_COUNT"
	EnvDryRun         = "DRY_RUN"
)

type Config struct {
	StateManagerURL  string
	InformerURL      string
	InformerUsername string
	InformerPassword string
	InformerTLS      bool

	ShardID string

	// BindAddress is what published ports listen on. Loopback by default: a
	// runtime's API has no authentication, and a laptop is not always on a
	// network you trust.
	BindAddress string
	// ExternalHost is what goes into status addresses. Usually the same as
	// BindAddress, but differs when the machine is reachable by a name.
	ExternalHost string
	PortMin      int
	PortMax      int

	DockerBinary string
	DryRun       bool

	// LocalUserConfigPath is where that profile lives. Empty, or a file that is
	// not there, means the machine adds nothing — which is the ordinary case.
	LocalUserConfigPath string

	// LocalPortMin and LocalPortCount describe the ports handed to every box
	// for whatever its user runs inside — a dev server, a debugger.
	//
	// Fixed here for now. They belong on the runtime, so a box could ask for
	// what it needs; until that exists every box gets the same handful, which
	// is enough to work with and cheap to take back.
	LocalPortMin   int
	LocalPortCount int
}

func Load() (Config, error) {
	c := Config{
		StateManagerURL:  os.Getenv(EnvStateManagerURL),
		InformerURL:      os.Getenv(EnvInformerURL),
		InformerUsername: os.Getenv(EnvInformerUsername),
		InformerPassword: os.Getenv(EnvInformerPassword),
		InformerTLS:      os.Getenv(EnvInformerTLS) == "true",
		ShardID:          os.Getenv(EnvShardID),
		BindAddress:      envOr(EnvBindAddress, "127.0.0.1"),
		DockerBinary:     envOr(EnvDockerBinary, "docker"),
		DryRun:           os.Getenv(EnvDryRun) == "true",
		LocalUserConfigPath: envOr(EnvLocalUserConfig,
			filepath.Join(homeDir(), ".location-agent", "user-config.json")),
	}
	c.ExternalHost = envOr(EnvExternalHost, c.BindAddress)

	var err error
	if c.PortMin, err = envInt(EnvPortMin, 31000); err != nil {
		return Config{}, err
	}
	if c.LocalPortMin, err = envInt(EnvLocalPortMin, 19080); err != nil {
		return Config{}, err
	}
	if c.LocalPortCount, err = envInt(EnvLocalPortCount, 5); err != nil {
		return Config{}, err
	}
	if c.PortMax, err = envInt(EnvPortMax, 32000); err != nil {
		return Config{}, err
	}

	var missing []string
	for name, v := range map[string]string{
		EnvStateManagerURL: c.StateManagerURL,
		EnvInformerURL:     c.InformerURL,
		EnvShardID:         c.ShardID,
	} {
		if strings.TrimSpace(v) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("required environment is missing: %s", strings.Join(missing, ", "))
	}
	if c.PortMin > c.PortMax {
		return Config{}, fmt.Errorf("%s (%d) is above %s (%d)", EnvPortMin, c.PortMin, EnvPortMax, c.PortMax)
	}
	return c, nil
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number, got %q", name, v)
	}
	return n, nil
}

// homeDir is where the agent keeps its own files. Falls back to the working
// directory when the environment has no home — a machine running the agent as a
// service may not.
func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return "."
}
