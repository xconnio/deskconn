package deskconn

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
)

func EnsureCredentials() (*Credentials, error) {
	credFilePath, err := credentialsFilePath()
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(credFilePath); err != nil {
		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			return nil, fmt.Errorf("failed to create watcher: %w", err)
		}
		defer watcher.Close()

		if err := watcher.Add(filepath.Dir(credFilePath)); err != nil {
			return nil, fmt.Errorf("failed to add watcher: %w", err)
		}

		log.Println("Waiting for credentials file...")

		for event := range watcher.Events {
			if event.Name != credFilePath {
				continue
			}

			if event.Op&(fsnotify.Create|fsnotify.Write) != 0 {
				log.Println("Desktop successfully attached to cloud")
				break
			}
		}
	}

	data, err := os.ReadFile(credFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read credentials file: %w", err)
	}

	var creds Credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("failed to unmarshal credentials: %w", err)
	}

	return &creds, nil
}

func credentialsFilePath() (string, error) {
	homedir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get user home dir: %w", err)
	}

	credFilePath := filepath.Join(homedir, ".deskconn/credentials.env")

	_ = os.MkdirAll(filepath.Dir(credFilePath), 0755)

	return credFilePath, nil
}
