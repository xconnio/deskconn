package connectivity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Credentials is the device identity issued by the cloud attachment service.
type Credentials struct {
	Realm      string `json:"realm"`
	AuthID     string `json:"authid"`
	PublicKey  string `json:"public_key"`
	PrivateKey string `json:"private_key"`
}

func EnsureCredentials(ctx context.Context, path string) (*Credentials, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if path == "" {
		return nil, fmt.Errorf("credentials path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, err := os.ReadFile(path)
		if err == nil {
			var credentials Credentials
			if err = json.Unmarshal(data, &credentials); err != nil {
				return nil, fmt.Errorf("decode credentials: %w", err)
			}
			if credentials.Realm == "" || credentials.AuthID == "" || credentials.PrivateKey == "" {
				return nil, fmt.Errorf("credentials require realm, authid and private key")
			}
			return &credentials, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func writePrivateJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".xconn-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
