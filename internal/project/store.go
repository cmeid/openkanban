package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/techdufus/openkanban/internal/config"
)

var (
	ErrProjectNotFound   = errors.New("project not found")
	ErrDuplicatePath     = errors.New("project with this repository path already exists")
	ErrTicketHasWorktree = errors.New("ticket has an active worktree and cannot change projects")
)

type ProjectRegistry struct {
	Projects map[string]*Project `json:"projects"`
}

func newRegistry() *ProjectRegistry {
	return &ProjectRegistry{
		Projects: make(map[string]*Project),
	}
}

func registryPath() (string, error) {
	dir, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "projects.json"), nil
}

func LoadRegistry() (*ProjectRegistry, error) {
	path, err := registryPath()
	if err != nil {
		return newRegistry(), nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return newRegistry(), nil
		}
		return nil, err
	}

	var reg ProjectRegistry
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, err
	}

	if reg.Projects == nil {
		reg.Projects = make(map[string]*Project)
	}

	return &reg, nil
}

// ReloadFromDisk re-reads projects.json and replaces the contents of
// r in place. Existing pointers to r remain valid. Project pointers
// inside r.Projects are replaced wholesale, so callers should re-fetch
// any project pointer they previously cached if they care about
// post-reload mutations.
//
// Safe to call from a Bubble Tea Update goroutine.
func (r *ProjectRegistry) ReloadFromDisk() error {
	fresh, err := LoadRegistry()
	if err != nil {
		return err
	}
	*r = *fresh
	return nil
}

func (r *ProjectRegistry) Save() error {
	path, err := registryPath()
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}

	// Unique tmp filename so concurrent writers from sibling
	// openkanban processes (each maintains its own registry view)
	// don't race on a shared "<path>.tmp" and consume each other's
	// rename source. See SaveTicket for the same pattern.
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

func (r *ProjectRegistry) Add(p *Project) error {
	for _, existing := range r.Projects {
		if existing.RepoPath == p.RepoPath {
			return ErrDuplicatePath
		}
	}
	r.Projects[p.ID] = p
	return r.Save()
}

func (r *ProjectRegistry) Get(id string) (*Project, error) {
	p, ok := r.Projects[id]
	if !ok {
		return nil, ErrProjectNotFound
	}
	return p, nil
}

func (r *ProjectRegistry) FindByPath(repoPath string) (*Project, error) {
	for _, p := range r.Projects {
		if p.RepoPath == repoPath {
			return p, nil
		}
	}
	return nil, ErrProjectNotFound
}

func (r *ProjectRegistry) Update(p *Project) error {
	if _, ok := r.Projects[p.ID]; !ok {
		return ErrProjectNotFound
	}
	p.Touch()
	r.Projects[p.ID] = p
	return r.Save()
}

func (r *ProjectRegistry) Delete(id string) error {
	if _, ok := r.Projects[id]; !ok {
		return ErrProjectNotFound
	}
	delete(r.Projects, id)
	return r.Save()
}

func (r *ProjectRegistry) List() []*Project {
	result := make([]*Project, 0, len(r.Projects))
	for _, p := range r.Projects {
		result = append(result, p)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result
}
