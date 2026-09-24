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
		"default":  s.DefaultFolder(),
		"can_pick": s.PickFolder != nil,
		// The settings panel offers to change it only where it can be saved.
		"can_save": s.SaveDefaultFolder != nil,
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

// DefaultFolder is the folder a new conversation gets now. Read under the
// lock, because the settings panel can change it while requests run.
func (s *Server) DefaultFolder() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.DefaultWorkspace
}

// handleSetupFolder changes the default folder for new conversations and
// saves it, so plain `ariadne ui`, a double-click and the terminal all start
// there. Checked like a folder typed into Folder…, and it grants nothing a
// conversation could not already get from there: an existing conversation
// keeps its own folder. POST and behind the token like the rest of setup.
func (s *Server) handleSetupFolder(w http.ResponseWriter, r *http.Request) {
	if s.SaveDefaultFolder == nil {
		httpError(w, http.StatusNotImplemented, "this ariadne cannot save settings")
		return
	}
	var req workspaceRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "malformed request body")
		return
	}
	p := strings.TrimSpace(req.Path)
	if p != "" {
		var err error
		if p, err = checkWorkspace(p); err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	now, notes, err := s.SaveDefaultFolder(p)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "could not save the folder: "+err.Error())
		return
	}
	s.mu.Lock()
	s.DefaultWorkspace = now
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"default": now, "notes": notes})
}
