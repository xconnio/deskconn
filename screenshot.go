package deskconn

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/godbus/dbus/v5"
	"gopkg.in/yaml.v3"
)

func EnableScreenshot(cfgDirectory string) error {
	return updateScreenshotConfig(cfgDirectory, true)
}

func DisableScreenshot(cfgDirectory string) error {
	return updateScreenshotConfig(cfgDirectory, false)
}

func ScreenshotEnabled(cfgDirectory string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(cfgDirectory, "config.yml"))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return false, err
	}

	return config.Screenshot.Enabled, nil
}

func updateScreenshotConfig(cfgDirectory string, enabled bool) error {
	cfgPath := filepath.Join(cfgDirectory, "config.yml")

	var config Config
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
// permission store.
func RevokeScreenshotPermission(conn *dbus.Conn) error {
	obj := conn.Object("org.freedesktop.impl.portal.PermissionStore",
		"/org/freedesktop/impl/portal/PermissionStore")

	return obj.Call("org.freedesktop.impl.portal.PermissionStore.Delete", 0, "screenshot", "screenshot").Err
}

func (s *Screen) Screenshot() ([]byte, error) {
	ok, err := ScreenshotEnabled(s.cfgDirectory)
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, fmt.Errorf("screenshot not enabled, run 'desk screenshot enable'")
	}

	return CaptureScreenshot(s.sessionBus)
}

// CaptureScreenshot performs the actual portal call. It is intended to be
// invoked from the foreground helper process.
func CaptureScreenshot(conn *dbus.Conn) ([]byte, error) {
	matchOptions := []dbus.MatchOption{
		dbus.WithMatchInterface("org.freedesktop.portal.Request"),
		dbus.WithMatchMember("Response"),
	}

	if err := conn.AddMatchSignal(matchOptions...); err != nil {
		return nil, err
	}
	defer conn.RemoveMatchSignal(matchOptions...) //nolint:errcheck

	ch := make(chan *dbus.Signal, 10)
	conn.Signal(ch)
	defer conn.RemoveSignal(ch)

	portal := conn.Object("org.freedesktop.portal.Desktop", "/org/freedesktop/portal/desktop")

	token := fmt.Sprintf("h%d", time.Now().UnixNano())

	opts := map[string]dbus.Variant{
		"handle_token": dbus.MakeVariant(token),
		"interactive":  dbus.MakeVariant(false),
	}

	var handle dbus.ObjectPath

	if err := portal.Call("org.freedesktop.portal.Screenshot.Screenshot", 0, "", opts).Store(&handle); err != nil {
		return nil, fmt.Errorf("screenshot portal: %w", err)
	}

	results, err := waitForPortalResponse(ch, handle, 5*time.Second)
	if err != nil {
		return nil, err
	}

	uriVar, ok := results["uri"]
	if !ok {
		return nil, fmt.Errorf("no uri in screenshot response")
	}

	uri, ok := uriVar.Value().(string)
	if !ok {
		return nil, fmt.Errorf("unexpected uri type %T", uriVar.Value())
	}

	path := fileURIToPath(uri)
	if path == "" {
		return nil, fmt.Errorf("could not resolve screenshot path from URI: %s", uri)
	}

	defer os.Remove(path)

	return os.ReadFile(path)
}

func waitForPortalResponse(ch chan *dbus.Signal, handle dbus.ObjectPath,
	timeout time.Duration) (map[string]dbus.Variant, error) {
	timer := time.After(timeout)

	for {
		select {
		case sig := <-ch:
			if sig == nil {
				continue
			}

			if sig.Path != handle || len(sig.Body) < 2 {
				continue
			}

			respCode, ok := sig.Body[0].(uint32)
			if !ok {
				return nil, fmt.Errorf("unexpected response code type %T", sig.Body[0])
			}

			if err := portalResponseError(respCode); err != nil {
				return nil, err
			}

			results, ok := sig.Body[1].(map[string]dbus.Variant)
			if !ok {
				return nil, fmt.Errorf("unexpected response type %T", sig.Body[1])
			}

			return results, nil

		case <-timer:
			return nil, fmt.Errorf("timed out waiting for portal response after %s", timeout)
		}
	}
}

func portalResponseError(respCode uint32) error {
	switch respCode {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("screenshot cancelled")
	case 2:
		return fmt.Errorf("screenshot failed, run 'desk screenshot enable' to grant permission")
	default:
		return fmt.Errorf("screenshot failed")
	}
}
