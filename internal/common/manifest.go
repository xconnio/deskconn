package common

import (
	"encoding/base64"
	"errors"
	"io/fs"
	"os"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// ManifestPNGPrefix starts a manifest app's PNG file as ReadManifestFiles returns it: the
// image as a data URI, so it travels in JSON like the YAML files.
const ManifestPNGPrefix = "data:image/png;base64,"

// A manifest icon is one of three kinds, which clients tell apart this way: an https://
// URL; a path ending in .png, one of the app's PNG files by its path from the app's
// directory, which clients fetch with ProcedureManifestFile; or anything else, an icon name
// clients may know.

// IsManifestIconFile reports whether icon is one of the app's PNG files.
func IsManifestIconFile(icon string) bool {
	return !strings.Contains(icon, "://") && path.Ext(icon) == ".png"
}

// ManifestIcons calls visit with each icon in a manifest's YAML: every icon field's value,
// or each state's for a toggle's {on, off}.
func ManifestIcons(node any, visit func(icon string)) {
	switch n := node.(type) {
	case map[string]any:
		switch icon := n["icon"].(type) {
		case string:
			visit(icon)
		case map[string]any:
			for _, state := range icon {
				if s, ok := state.(string); ok {
					visit(s)
				}
			}
		}
		for _, child := range n {
			ManifestIcons(child, visit)
		}
	case []any:
		for _, child := range n {
			ManifestIcons(child, visit)
		}
	}
}

// ReadManifestFiles returns a manifest app's files under directory by slash-separated path,
// as ProcedureManifestRegister takes them: every YAML file as text, and the PNG files their
// icons name as data URIs (see ManifestPNGPrefix). Other files, including PNGs no YAML
// names, are left out; of the rest, deskconn keeps those the app uses (a draft screen
// nothing includes yet can name others). Symlinks can't lead outside directory.
func ReadManifestFiles(directory string) (map[string]string, error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()

	files := map[string]string{}
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || path.Ext(name) != ".yaml" {
			return err
		}
		data, err := fs.ReadFile(root.FS(), name)
		if err != nil {
			return err
		}
		files[name] = string(data)
		return nil
	})
	if err != nil {
		return nil, err
	}

	icons := map[string]bool{}
	for _, content := range files {
		var node any
		// deskconn reports YAML that doesn't parse when the app is registered.
		if yaml.Unmarshal([]byte(content), &node) == nil {
			ManifestIcons(node, func(icon string) {
				if IsManifestIconFile(icon) {
					icons[icon] = true
				}
			})
		}
	}
	for icon := range icons {
		if !fs.ValidPath(icon) {
			continue // deskconn reports it missing.
		}
		data, err := fs.ReadFile(root.FS(), icon)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		files[icon] = ManifestPNGPrefix + base64.StdEncoding.EncodeToString(data)
	}
	return files, nil
}
