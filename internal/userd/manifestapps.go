package userd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/relay"
	"github.com/xconnio/xconn-core/bindings/go"
)

// A manifest app's UI is YAML that clients render (see https://github.com/xconnio/app-engine).
// It has no procedures of its own: it calls procedures other apps serve. Owners and admins
// register manifest apps, which are stored in the manifests directory and listed, enabled
// and disabled like any other app.

const (
	manifestRoot      = "app.yaml"
	manifestMaxFiles  = 64
	manifestMaxBytes  = 256 << 10
	manifestMaxNodes  = 10000
	manifestDirectory = "manifests"
)

var manifestIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

const procedureNamespace = "io.xconn."

var errManifestTooLarge = fmt.Errorf("an app can have at most %d YAML mappings with its includes expanded",
	manifestMaxNodes)

// appEngineDirectory is where app-engine's predefined blocks are, for includes such as
// app-engine/blocks/toggle.yaml. Clients expand those, so deskconn-userd leaves them as they are.
const appEngineDirectory = "app-engine/"

// manifestApp is a registered manifest app.
type manifestApp struct {
	app *app
	// files are its YAML files by slash-separated path, app.yaml at the root.
	files map[string]string
	// resolved is app.yaml with screens resolved: a map of screen id to the screen, each
	// include replaced by its file's contents under block.
	resolved map[string]any
}

// manifestHeader is the part of app.yaml deskconn-userd needs.
type manifestHeader struct {
	App struct {
		ID   string `yaml:"id"`
		Name string `yaml:"name"`
	} `yaml:"app"`
	API struct {
		KeyExchange string `yaml:"key_exchange"`
	} `yaml:"api"`
	Requires    []string `yaml:"requires"`
	EntryScreen string   `yaml:"entry_screen"`
	Screens     []string `yaml:"screens"`
}

// resolveManifest validates files and resolves them to a manifestApp.
func resolveManifest(files map[string]string) (*manifestApp, error) {
	if len(files) > manifestMaxFiles {
		return nil, fmt.Errorf("an app can have at most %d files", manifestMaxFiles)
	}
	size := 0
	for name, content := range files {
		if err := checkManifestPath(name); err != nil {
			return nil, err
		}
		size += len(content)
	}
	if size > manifestMaxBytes {
		return nil, fmt.Errorf("an app can have at most %d KiB of YAML", manifestMaxBytes>>10)
	}

	r := &manifestResolver{files: files}
	root, err := r.load(manifestRoot)
	if err != nil {
		return nil, err
	}
	var header manifestHeader
	if err := decodeManifest(files[manifestRoot], &header); err != nil {
		return nil, fmt.Errorf("%s: %w", manifestRoot, err)
	}

	var problems []error
	problem := func(format string, args ...any) { problems = append(problems, fmt.Errorf(format, args...)) }

	if !manifestIDPattern.MatchString(header.App.ID) {
		problem("app.id must match %s", manifestIDPattern)
	}
	if header.App.Name == "" {
		problem("app.name is missing")
	}
	if err := checkReachable(header.API.KeyExchange); err != nil {
		problem("api.key_exchange %w", err)
	}

	screens := map[string]any{}
	for _, screenPath := range header.Screens {
		screen, err := r.load(path.Clean(screenPath))
		if err != nil {
			problems = append(problems, err)
			continue
		}
		id, _ := screen["id"].(string)
		if id == "" {
			problem("%s: id is missing", screenPath)
			continue
		}
		if _, ok := screens[id]; ok {
			problem("%s: screen id %s is used twice", screenPath, id)
			continue
		}
		screens[id] = screen
	}
	if _, ok := screens[header.EntryScreen]; !ok {
		problem("entry_screen %q is not a screen", header.EntryScreen)
	}
	if r.nodes > manifestMaxNodes {
		return nil, errManifestTooLarge
	}
	root["screens"] = screens

	problems = append(problems, r.problems...)
	walkManifest(root, func(node map[string]any) {
		if call, ok := node["call"]; ok {
			if _, ok := call.(string); !ok {
				problem("call must be a procedure name, not %v", call)
			}
		}
		for key, value := range node {
			values, ok := value.([]any)
			if !ok {
				values = []any{value}
			}
			for _, v := range values {
				procedure, ok := v.(string)
				if !ok || !strings.HasPrefix(procedure, procedureNamespace) {
					continue
				}
				if err := checkReachable(procedure); err != nil {
					problem("%s %w", key, err)
				}
			}
		}
	})
	if len(problems) > 0 {
		return nil, errors.Join(problems...)
	}

	return &manifestApp{
		app: &app{
			ID:          header.App.ID,
			Name:        header.App.Name,
			KeyExchange: header.API.KeyExchange,
			Requires:    header.Requires,
		},
		files:    files,
		resolved: root,
	}, nil
}

// checkReachable reports whether remote clients may call procedure: it must be under
// ProcedurePrefix or an app prefix, and not for the machine's owner and admins only.
func checkReachable(procedure string) error {
	prefixes := append([]string{common.ProcedurePrefix}, relay.AppPrefixes()...)
	if strings.HasPrefix(procedure, common.ProcedureAdminPrefix) {
		return fmt.Errorf("%s is only for the machine's owner and admins", procedure)
	}
	if !slices.ContainsFunc(prefixes, func(prefix string) bool { return strings.HasPrefix(procedure, prefix) }) {
		return fmt.Errorf("%q must be under one of %s", procedure, strings.Join(prefixes, ", "))
	}
	return nil
}

// checkManifestPath reports whether name is a YAML file path inside the app's directory.
func checkManifestPath(name string) error {
	if name != path.Clean(name) || path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
		return fmt.Errorf("%s: files must be inside the app's directory", name)
	}
	if path.Ext(name) != ".yaml" {
		return fmt.Errorf("%s: only .yaml files are allowed", name)
	}
	return nil
}

func decodeManifest(content string, out any) error {
	return yaml.Unmarshal([]byte(content), out)
}

// manifestResolver loads an app's files, replacing each include of the app's own files with
// its contents.
type manifestResolver struct {
	files    map[string]string
	loading  []string
	problems []error
	nodes    int
}

// load returns the mapping in file name, with its includes resolved.
func (r *manifestResolver) load(name string) (map[string]any, error) {
	if err := checkManifestPath(name); err != nil {
		return nil, err
	}
	content, ok := r.files[name]
	if !ok {
		return nil, fmt.Errorf("%s doesn't exist", name)
	}
	if slices.Contains(r.loading, name) {
		return nil, fmt.Errorf("%s includes itself", name)
	}
	var node map[string]any
	if err := decodeManifest(content, &node); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if node == nil {
		return nil, fmt.Errorf("%s is empty", name)
	}
	walkManifest(node, func(map[string]any) { r.nodes++ })
	if r.nodes > manifestMaxNodes {
		return nil, errManifestTooLarge
	}

	r.loading = append(r.loading, name)
	defer func() { r.loading = r.loading[:len(r.loading)-1] }()
	walkManifest(node, func(m map[string]any) {
		include, ok := m["include"]
		if !ok {
			return
		}
		target, ok := include.(string)
		if !ok {
			r.problems = append(r.problems, fmt.Errorf("%s: include must be a path", name))
			return
		}
		if strings.HasPrefix(target, appEngineDirectory) {
			return
		}
		delete(m, "include")
		block, err := r.load(path.Join(path.Dir(name), target))
		if errors.Is(err, errManifestTooLarge) {
			return
		}
		if err != nil {
			r.problems = append(r.problems, fmt.Errorf("%s: %w", name, err))
			return
		}
		m["block"] = block
	})
	return node, nil
}

// walkManifest calls visit on every mapping in node, parents before their children.
func walkManifest(node any, visit func(map[string]any)) {
	switch n := node.(type) {
	case map[string]any:
		visit(n)
		for _, child := range n {
			walkManifest(child, visit)
		}
	case []any:
		for _, child := range n {
			walkManifest(child, visit)
		}
	}
}

// loadManifests returns the manifest apps stored in directory, skipping invalid ones.
func loadManifests(directory string) map[string]*manifestApp {
	apps := map[string]*manifestApp{}
	entries, err := os.ReadDir(directory)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("manifests: failed to read %s: %v", directory, err)
		}
		return apps
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		files, err := common.ReadManifestFiles(filepath.Join(directory, entry.Name()))
		if err != nil {
			log.Printf("manifests: failed to read %s: %v", entry.Name(), err)
			continue
		}
		m, err := resolveManifest(files)
		if err != nil {
			log.Printf("manifests: skipping %s: %v", entry.Name(), err)
			continue
		}
		apps[m.app.ID] = m
	}
	return apps
}

// storeManifest replaces directory's files with files.
func storeManifest(directory string, files map[string]string) error {
	tmp := directory + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	for name, content := range files {
		target := filepath.Join(tmp, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(target, []byte(content), 0600); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(directory); err != nil {
		return err
	}
	return os.Rename(tmp, directory)
}

// handleManifestRegister registers a manifest app, or replaces the one with the same id.
// Its argument is the app's files as JSON, {"<path>": "<yaml>"}, app.yaml at the root. It
// returns the app's id. Only the machine's owner and admins may call it (see
// ProcedureManifestRegister).
func (c *capabilities) handleManifestRegister(_ context.Context, inv *xconn.Invocation) *xconn.Result {
	if !isManager(inv) {
		return xconn.NewResultError(common.ErrNotAuthorized, "only the machine's owner and admins may register apps")
	}
	payload, err := inv.ArgBytes(0)
	if err != nil {
		return xconn.NewResultError(common.ErrInvalidArgument, err.Error())
	}
	var files map[string]string
	if err := json.Unmarshal(payload, &files); err != nil {
		return xconn.NewResultError(common.ErrInvalidArgument, err.Error())
	}
	m, err := resolveManifest(files)
	if err != nil {
		return xconn.NewResultError(common.ErrInvalidArgument, err.Error())
	}

	c.Lock()
	defer c.Unlock()
	if _, ok := c.manifests[m.app.ID]; !ok && slices.ContainsFunc(c.apps(), func(a *app) bool {
		return a.ID == m.app.ID
	}) {
		return xconn.NewResultError(common.ErrInvalidArgument, fmt.Sprintf("app %s is already registered",
			m.app.ID))
	}
	if err := storeManifest(filepath.Join(c.manifestsPath, m.app.ID), files); err != nil {
		return xconn.NewResultError(common.ErrOperationFailed, err.Error())
	}
	c.manifests[m.app.ID] = m
	c.publish()
	log.Printf("Registered manifest app %s", m.app.ID)
	return xconn.NewResult(m.app.ID)
}

// handleManifestUnregister removes the manifest app with the given id. Only the machine's
// owner and admins may call it (see ProcedureManifestUnregister).
func (c *capabilities) handleManifestUnregister(_ context.Context, inv *xconn.Invocation) *xconn.Result {
	if !isManager(inv) {
		return xconn.NewResultError(common.ErrNotAuthorized, "only the machine's owner and admins may unregister apps")
	}
	id, err := inv.ArgString(0)
	if err != nil {
		return xconn.NewResultError(common.ErrInvalidArgument, err.Error())
	}

	c.Lock()
	defer c.Unlock()
	if _, ok := c.manifests[id]; !ok {
		return xconn.NewResultError(common.ErrInvalidArgument, "manifest app not found: "+id)
	}
	if err := os.RemoveAll(filepath.Join(c.manifestsPath, id)); err != nil {
		return xconn.NewResultError(common.ErrOperationFailed, err.Error())
	}
	delete(c.manifests, id)
	if _, ok := c.states[id]; ok {
		delete(c.states, id)
		if err := c.save(); err != nil {
			return xconn.NewResultError(common.ErrOperationFailed, err.Error())
		}
	}
	c.publish()
	log.Printf("Unregistered manifest app %s", id)
	return xconn.NewResult()
}

// handleManifestGet returns an enabled manifest app's resolved app.yaml.
func (c *capabilities) handleManifestGet(_ context.Context, inv *xconn.Invocation) *xconn.Result {
	id, err := inv.ArgString(0)
	if err != nil {
		return xconn.NewResultError(common.ErrInvalidArgument, err.Error())
	}

	c.Lock()
	defer c.Unlock()
	m, ok := c.manifests[id]
	if !ok {
		return xconn.NewResultError(common.ErrInvalidArgument, "manifest app not found: "+id)
	}
	if !c.isEnabled(id) {
		return xconn.NewResultError(common.ErrInvalidArgument, disabledMessage(id))
	}
	return xconn.NewResult(m.resolved)
}
