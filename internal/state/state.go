// Package state owns agent names, private paths and persistent configuration.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

const DefaultAgent = "personal"

var agentPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,32}$`)

type Paths struct {
	Root, Agent, Dir, Run, Logs, Socket, Lock, Instance, Log, Config string
}

func Resolve(root, name string) (Paths, error) {
	if !agentPattern.MatchString(name) {
		return Paths{}, fmt.Errorf("agent name: 1-32 ASCII letters/digits/_/-")
	}
	if root == "" {
		return Paths{}, fmt.Errorf("state-dir must not be empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return Paths{}, err
	}
	dir := filepath.Join(abs, "agents", name)
	run := filepath.Join(dir, "run")
	logs := filepath.Join(dir, "logs")
	p := Paths{Root: abs, Agent: name, Dir: dir, Run: run, Logs: logs,
		Socket: filepath.Join(run, "daemon.sock"), Lock: filepath.Join(run, "daemon.lock"),
		Instance: filepath.Join(run, "instance.json"), Log: filepath.Join(logs, "daemon.log"),
		Config: filepath.Join(dir, "agent.json")}
	if len(p.Socket) >= 104 {
		return Paths{}, fmt.Errorf("socket path too long; use a shorter --state-dir or agent name")
	}
	return p, nil
}

// Check is read-only. Missing directories are allowed; existing ones must be private.
func (p Paths) Check() error {
	for _, dir := range p.directories() {
		if err := checkDir(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (p Paths) Ensure() error {
	for _, dir := range p.directories() {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		if err := checkDir(dir); err != nil {
			return err
		}
	}
	return nil
}

func (p Paths) directories() []string {
	return []string{p.Root, filepath.Join(p.Root, "agents"), p.Dir, p.Run, p.Logs}
}

func checkDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("expected private directory (0700), not a symlink: %s", path)
	}
	return nil
}

// OpenPrivateFile refuses symlinks and special/public files. Its parent must be private.
func OpenPrivateFile(path string, flags int) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return nil, fmt.Errorf("expected private regular file (0600): %s", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return os.OpenFile(path, flags, 0600)
}

type Config struct {
	Version   int    `json:"version"`
	Agent     string `json:"agent"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
}

// LoadConfig returns defaults when absent; only start/serve initialize the file.
func (p Paths) LoadConfig(defaults Config) (Config, error) {
	f, err := OpenPrivateFile(p.Config, os.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return defaults, nil
	}
	if err != nil {
		return Config{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Config{}, err
	}
	if info.Size() > 16<<10 {
		return Config{}, fmt.Errorf("agent.json exceeds 16 KiB")
	}
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("invalid agent.json: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF || cfg.Version != 1 || cfg.Agent != p.Agent {
		return cfg, fmt.Errorf("invalid agent.json: expected version 1 and agent %q", p.Agent)
	}
	return cfg, nil
}

// InitConfig publishes a complete default file without overwriting another starter.
// Explicit CLI overrides are deliberately not written to this file.
func (p Paths) InitConfig(defaults Config) error {
	tmp, err := writeTempJSON(p.Dir, defaults)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if err := os.Link(tmp, p.Config); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return nil
}

func WriteJSON(path string, value any) error {
	tmp, err := writeTempJSON(filepath.Dir(path), value)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	return os.Rename(tmp, path)
}

func writeTempJSON(dir string, value any) (name string, err error) {
	f, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return "", err
	}
	defer func() {
		_ = f.Close()
		if err != nil {
			_ = os.Remove(f.Name())
		}
	}()
	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	if err = encoder.Encode(value); err != nil {
		return "", err
	}
	if err = f.Sync(); err != nil {
		return "", err
	}
	err = f.Close()
	return f.Name(), err
}
