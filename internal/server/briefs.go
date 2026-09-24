package server

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

const (
	// maxBriefsListed is how many briefs the panel lists, newest first. The
	// path box still opens anything the list leaves out.
	maxBriefsListed = 50
	// maxBriefsVisited bounds the walk. A conversation's folder can be
	// somebody's whole Documents; the list is a convenience and must come back
	// in well under a second, not after reading every file on the disk.
	maxBriefsVisited = 5000
	// blankCheckBytes: files smaller than this are read to leave out ones
	// holding nothing but whitespace.
	blankCheckBytes = 4096
)

type briefsRequest struct {
	Workspace string `json:"workspace,omitempty"`
}

type briefEntry struct {
	Path     string    `json:"path"` // slash-separated, relative to the folder
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

type briefsResponse struct {
	Folder string       `json:"folder"`
	Briefs []briefEntry `json:"briefs"`
	// Partial: the folder held more than the walk looks at, or more briefs
	// than are listed, so a brief can exist and not be shown.
	Partial bool `json:"partial,omitempty"`
}

// listBriefs returns the .md files under workspace, subfolders included,
// newest first.
//
// Walked through os.Root, the same as reading a brief, so the list cannot
// name anything outside the folder: a link is not followed and not listed,
// whatever it points at. Folders whose names start with "." are skipped —
// .git holds no briefs and can hold thousands of files.
func listBriefs(workspace string) (briefsResponse, error) {
	ws, err := checkWorkspace(workspace)
	if err != nil {
		return briefsResponse{}, err
	}
	root, err := os.OpenRoot(ws)
	if err != nil {
		return briefsResponse{}, err
	}
	defer root.Close()

	out, err := walkBriefs(root.FS())
	out.Folder = ws
	return out, err
}

// walkBriefs is listBriefs over any file system, so a link — which Windows
// will not let an unprivileged test create — can be tested with fstest.
// Only regular files are listed: a link to a .md file elsewhere would be a
// name the list offers and readWorkspaceBrief then refuses.
func walkBriefs(fsys fs.FS) (briefsResponse, error) {
	out := briefsResponse{Briefs: []briefEntry{}}
	visited := 0
	errStop := errors.New("stop")
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subfolder is not a reason to list nothing.
			if d != nil && d.IsDir() && p != "." {
				return fs.SkipDir
			}
			return nil
		}
		if visited++; visited > maxBriefsVisited {
			out.Partial = true
			return errStop
		}
		if d.IsDir() {
			if p != "." && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !strings.EqualFold(path.Ext(p), ".md") {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if fi.Size() == 0 || fi.Size() > maxBriefBytes {
			return nil
		}
		// A file of only spaces and newlines would be listed and then
		// refused as empty. Small files are read to find out; a blank one
		// is never large, and reading 4 KB costs nothing.
		if fi.Size() < blankCheckBytes {
			if data, err := fs.ReadFile(fsys, p); err != nil || strings.TrimSpace(string(data)) == "" {
				return nil
			}
		}
		out.Briefs = append(out.Briefs, briefEntry{Path: p, Size: fi.Size(), Modified: fi.ModTime()})
		return nil
	})
	if err != nil && !errors.Is(err, errStop) {
		return briefsResponse{}, err
	}

	sort.SliceStable(out.Briefs, func(i, j int) bool {
		return out.Briefs[i].Modified.After(out.Briefs[j].Modified)
	})
	if len(out.Briefs) > maxBriefsListed {
		out.Briefs, out.Partial = out.Briefs[:maxBriefsListed], true
	}
	return out, nil
}

// handleBriefs lists the briefs in the folder a new conversation would use,
// so starting one is a click rather than a remembered path. POST and behind
// the token like POST /api/brief: file names in somebody's folder are not
// for any page that can reach localhost.
func (s *Server) handleBriefs(w http.ResponseWriter, r *http.Request) {
	var req briefsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "malformed request body")
		return
	}
	ws := req.Workspace
	if ws == "" {
		ws = s.DefaultFolder()
	}
	out, err := listBriefs(ws)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}
