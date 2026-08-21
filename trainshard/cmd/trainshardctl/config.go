package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"trainshard/internal/domain/shared/vo"
	"trainshard/internal/infrastructure/adapters/hosts"
)

type config struct {
	actor           vo.Address
	secret          []byte
	privateKey      string
	keyringDir      string
	keyringBackend  string
	keyringPassword string
	keyName         string
	chainGRPC       string
	chainID         string
	chainSeed       string
	directory       hosts.Directory
	timeout         time.Duration

	pollInterval time.Duration
	settleWindow time.Duration
}

func load() (config, error) {
	cfg := config{
		actor:           vo.Address(env("ACTOR", "")),
		secret:          []byte(env("SHARED_SECRET", "")),
		privateKey:      env("PRIVATE_KEY", ""),
		keyringDir:      env("KEYRING_DIR", ""),
		keyringBackend:  env("KEYRING_BACKEND", "file"),
		keyringPassword: env("KEYRING_PASSWORD", ""),
		keyName:         env("KEY_NAME", ""),
		chainGRPC:       env("CHAIN_GRPC", ""),
		chainID:         env("CHAIN_ID", "prod-sim"),
		chainSeed:       env("CHAIN_SEED", ""),
		pollInterval:    10 * time.Second,
		settleWindow:    2 * time.Minute,
	}

	directory, err := loadDirectory(env("HOSTS", ""))
	if err != nil {
		return config{}, err
	}
	cfg.directory = directory

	timeout, err := time.ParseDuration(env("TIMEOUT", "10m"))
	if err != nil || timeout <= 0 {
		return config{}, fmt.Errorf("TRAINSHARDCTL_TIMEOUT must be a positive duration, such as 10m")
	}
	cfg.timeout = timeout

	signed := cfg.privateKey != "" || cfg.keyName != ""
	switch {
	case !signed && len(cfg.secret) == 0:
		return config{}, fmt.Errorf("TRAINSHARDCTL_PRIVATE_KEY is required, it is the key the shard was created from")
	case !signed && cfg.actor == "":
		return config{}, fmt.Errorf("TRAINSHARDCTL_ACTOR is required with a shared secret, it is the address the shard was created from")
	case len(cfg.directory) == 0:
		return config{}, fmt.Errorf("TRAINSHARDCTL_HOSTS is required, it is a json file of participant to host url")
	case cfg.chainGRPC == "" && cfg.chainSeed == "":
		return config{}, fmt.Errorf("TRAINSHARDCTL_CHAIN_GRPC is required, it is the chain that says what the shard reserves")
	case cfg.chainGRPC != "" && !signed:
		return config{}, fmt.Errorf("a shared secret signs nothing the chain accepts: set TRAINSHARDCTL_PRIVATE_KEY to drive a run against a real chain")
	}
	return cfg, nil
}

func loadDirectory(path string) (hosts.Directory, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("hosts file: %w", err)
	}

	urls := map[string]string{}
	if err := json.Unmarshal(raw, &urls); err != nil {
		return nil, fmt.Errorf("hosts file %q: %w", path, err)
	}

	directory := make(hosts.Directory, len(urls))
	for participant, url := range urls {
		address, err := vo.ParseAddress(participant)
		if err != nil {
			return nil, err
		}
		directory[vo.Participant(address)] = strings.TrimSuffix(url, "/")
	}
	return directory, nil
}

func env(name, fallback string) string {
	for _, prefix := range []string{"TRAINSHARDCTL_", "TRAINSHARD_"} {
		if value, found := os.LookupEnv(prefix + name); found {
			return value
		}
	}
	return fallback
}
