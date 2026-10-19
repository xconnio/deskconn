package common

import (
	_ "image/gif"
	_ "image/png"
	"time"
)

type FileEntry struct {
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	Type       string    `json:"type"`
	Mode       string    `json:"mode"`
	Size       int64     `json:"size"`
	Hidden     bool      `json:"hidden"`
	ModTime    time.Time `json:"mod_time"`
	IsDir      bool      `json:"is_dir"`
	IsSymlink  bool      `json:"is_symlink"`
	LinkTarget string    `json:"link_target,omitempty"`
	ItemCount  *int      `json:"item_count,omitempty"`
	Thumbnail  string    `json:"thumbnail,omitempty"`
}

type FileBrowseResult struct {
	Path       string      `json:"path"`
	HomePath   string      `json:"home_path"`
	ParentPath string      `json:"parent_path,omitempty"`
	Type       string      `json:"type"`
	Mode       string      `json:"mode"`
	Size       int64       `json:"size"`
	ModTime    time.Time   `json:"mod_time"`
	IsDir      bool        `json:"is_dir"`
	IsSymlink  bool        `json:"is_symlink"`
	LinkTarget string      `json:"link_target,omitempty"`
	Entries    []FileEntry `json:"entries,omitempty"`
	NextCursor string      `json:"next_cursor,omitempty"`
	HasMore    bool        `json:"has_more"`
}
