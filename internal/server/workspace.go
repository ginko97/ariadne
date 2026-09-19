package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// checkWorkspace turns a folder the page sent into the path a conversation
// will record, or says why it cannot be one.
//
// Absolute only: a relative path would be resolved against wherever the
// server happened to start, which the person picking it cannot see. And it
// must already exist as a directory — the page chooses a folder, it does not
// create one somewhere a typo put it.
func checkWorkspace(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("folder is empty")
	}
	if !filepath.IsAbs(p) {
		return "", errors.New("use a full path, such as C:\\Users\\you\\project or /home/you/project")
	}
	p = filepath.Clean(p)
	fi, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", errors.New("that folder does not exist")
		}
		return "", errors.New("that folder cannot be read")
	}
	if !fi.IsDir() {
		return "", errors.New("that is a file, not a folder")
	}
	return p, nil
}

type workspaceRequest struct {
	Path string `json:"path"`
}

// handleWorkspace reports the folder a new conversation gets when the page
// sends none, so the page can show it before the first message.
func (s *Server) handleWorkspace(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"default":  s.DefaultWorkspace,
		"can_pick": s.PickFolder != nil,
	})
}

// handleWorkspaceCheck validates a typed folder before it is used, so a typo
// is reported when it is typed rather than after a message has been written.
func (s *Server) handleWorkspaceCheck(w http.ResponseWriter, r *http.Request) {
	var req workspaceRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "malformed request body")
		return
	}
	p, err := checkWorkspace(req.Path)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"path": p})
}

// handleWorkspacePick opens the operating system's folder dialog on this
// machine and returns what was chosen.
//
// The dialog runs in this process, not the browser, because a browser never
// gives a page the real path of a folder: <input webkitdirectory> yields file
// names, which os.Root cannot use. POST and behind the CSRF token like every
// other endpoint that does something: a page that could open a dialog on the
// desktop could at least pester somebody with them.
//
// One at a time. A second click while a dialog is open would stack another
// behind it.
func (s *Server) handleWorkspacePick(w http.ResponseWriter, r *http.Request) {
	if s.PickFolder == nil {
		httpError(w, http.StatusNotImplemented, "no folder dialog on this machine; type the path instead")
		return
	}
	if !s.picking.TryLock() {
		httpError(w, http.StatusConflict, "a folder dialog is already open")
		return
	}
	defer s.picking.Unlock()

	p, err := s.PickFolder(r.Context())
	if err != nil {
		httpError(w, http.StatusInternalServerError, "the folder dialog failed: "+err.Error()+"; type the path instead")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if p == "" {
		_ = json.NewEncoder(w).Encode(map[string]any{"cancelled": true})
		return
	}
	if p, err = checkWorkspace(p); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"path": p})
}
