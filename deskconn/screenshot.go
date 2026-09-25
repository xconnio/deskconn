package deskconn

import (
	"os"
	"path/filepath"

	"github.com/godbus/dbus/v5"
	"gopkg.in/yaml.v3"

	"github.com/xconnio/deskconn/common"
)

// EnableScreenshot/DisableScreenshot/ScreenshotEnabled are purely local
// machine settings (config.yml read/write, no device/network round trip),
// so the CLI (`deskconn screenshot enable/disable`) calls them directly.
func EnableScreenshot(cfgDirectory string) error {
	return updateScreenshotConfig(cfgDirectory, true)
}

func DisableScreenshot(cfgDirectory string) error {
	return updateScreenshotConfig(cfgDirectory, false)
}

func updateScreenshotConfig(cfgDirectory string, enabled bool) error {
	cfgPath := filepath.Join(cfgDirectory, "config.yml")

	var config common.Config
	data, err := os.ReadFile(cfgPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	if err := yaml.Unmarshal(data, &config); err != nil {
		return err
	}

	config.Screenshot.Enabled = enabled

	b, err := yaml.Marshal(config)
	if err != nil {
		return err
	}

	return os.WriteFile(cfgPath, b, 0600)
}

// RevokeScreenshotPermission clears the screenshot entry from the portal
// permission store. Called directly by the CLI when the device rejects a
// permission-check call.
func RevokeScreenshotPermission(conn *dbus.Conn) error {
	obj := conn.Object("org.freedesktop.impl.portal.PermissionStore",
		"/org/freedesktop/impl/portal/PermissionStore")

	return obj.Call("org.freedesktop.impl.portal.PermissionStore.Delete", 0, "screenshot", "screenshot").Err
}
