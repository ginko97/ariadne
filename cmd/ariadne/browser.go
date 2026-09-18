package main

import (
	"os/exec"
	"runtime"
)

// browserCommand is the argv that opens url in the default browser on goos.
// A pure function so the choice is testable without launching anything.
func browserCommand(goos, url string) []string {
	switch goos {
	case "windows":
		// rundll32 rather than `cmd /c start`: start treats & in a URL as a
		// command separator, and this needs no shell at all.
		return []string{"rundll32", "url.dll,FileProtocolHandler", url}
	case "darwin":
		return []string{"open", url}
	default:
		return []string{"xdg-open", url}
	}
}

// openBrowser starts the browser and does not wait for it. The URL is always
// printed as well, so a machine with no browser, or one that fails to start,
// loses nothing.
func openBrowser(url string) error {
	argv := browserCommand(runtime.GOOS, url)
	return exec.Command(argv[0], argv[1:]...).Start()
}
