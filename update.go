package main

import "runtime"

const updateBaseURL = "https://github.com/smnhffmnn/mux/releases/latest/download/"

// selfUpdateURL returns the download URL for the current platform, or "" if unsupported.
func selfUpdateURL() string {
	switch runtime.GOOS {
	case "windows":
		return updateBaseURL + "mux.exe"
	case "linux":
		return updateBaseURL + "mux-linux"
	default:
		// macOS: self-update inside .app bundle is unreliable (code signing, Gatekeeper)
		return ""
	}
}
