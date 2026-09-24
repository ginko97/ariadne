package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// workspaceEnv names the saved default folder for new conversations: set in
// the settings panel, stored in config.env, read at every start.
const workspaceEnv = "ARIADNE_WORKSPACE"

// homeWorkspace is the home directory's own workspace, the default when no
// folder has been saved. main sets it; the value here is what tests see.
var homeWorkspace = "workspace"

// savedWorkspace is the folder ARIADNE_WORKSPACE names, or "" with a reason
// when it names none that can be used. A folder that has been moved or
// deleted since it was saved is not created again: the conversation would
// start somewhere empty that nobody chose, so ariadne says so and uses the
// home workspace.
func savedWorkspace(getenv func(string) string) (folder, warning string) {
	p := getenv(workspaceEnv)
	if p == "" {
		return "", ""
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Sprintf("%s=%s is not a full path; using %s", workspaceEnv, p, homeWorkspace)
	}
	fi, err := os.Stat(p)
	if err != nil || !fi.IsDir() {
		return "", fmt.Sprintf("%s=%s is not a folder that exists; using %s", workspaceEnv, p, homeWorkspace)
	}
	return filepath.Clean(p), ""
}

// saveDefaultFolder stores folder as the default for new conversations, or
// clears it when folder is "", and returns the default now in effect. The
// server has already checked that folder exists. A value set in the shell
// outranks config.env and is reported, as for every other setting.
func saveDefaultFolder(folder string) (string, []string, error) {
	vals := map[string]string{workspaceEnv: folder}
	if err := writeConfigEnv(configEnvFile, vals); err != nil {
		return "", nil, err
	}
	notes := applySettings(vals)
	effective, _ := savedWorkspace(os.Getenv)
	if effective == "" {
		effective = homeWorkspace
	}
	if abs, err := filepath.Abs(effective); err == nil {
		effective = abs
	}
	return effective, notes, nil
}

// folderSaver is what the settings panel saves the default folder with.
// Started with -workspace, the folder is still saved for the next start, but
// this session keeps the flag's folder and says so: a restart with the same
// flag would use it too, and settings must do what a restart would — the
// same rule a folder set in the shell follows.
func folderSaver(flagFolder string) func(string) (string, []string, error) {
	if flagFolder == "" {
		return saveDefaultFolder
	}
	return func(folder string) (string, []string, error) {
		_, notes, err := saveDefaultFolder(folder)
		if err != nil {
			return "", nil, err
		}
		return flagFolder, append(notes, "Saved for the next start. This session was started with -workspace "+
			flagFolder+" and keeps it; start without -workspace to use the saved folder."), nil
	}
}
