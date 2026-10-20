package common

import (
	"io/fs"
	"os"
	"path"
)

// ReadManifestFiles returns a manifest app's YAML files under directory by slash-separated
// path, as ProcedureManifestRegister takes them. Symlinks can't lead outside directory.
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
	return files, err
}
