package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Detection describes a locally installed Agent CLI. Source is either PATH or
// a well-known per-user/system installation directory; Taskian never scans a drive.
type Detection struct {
	Type   string `json:"type"`
	Path   string `json:"path"`
	Source string `json:"source"`
}

// ResolveCommand keeps an explicitly working command and otherwise searches the
// standard command names and well-known installation directories for its type.
func ResolveCommand(agentType, configured string) (string, bool) {
	if path, err := exec.LookPath(configured); err == nil {
		return path, true
	}
	for _, found := range Detect() {
		if found.Type == strings.ToLower(agentType) {
			return found.Path, true
		}
	}
	return configured, false
}

func Detect() []Detection {
	seen := map[string]bool{}
	var result []Detection
	for _, spec := range []struct {
		typeName string
		names    []string
	}{
		{typeName: "codex", names: executableNames("codex")},
		{typeName: "cursor", names: executableNames("agent")},
	} {
		for _, name := range spec.names {
			if path, err := exec.LookPath(name); err == nil {
				result = appendDetection(result, seen, spec.typeName, path, "PATH")
			}
		}
		for _, dir := range knownBinDirs() {
			for _, name := range spec.names {
				path := filepath.Join(dir, name)
				if info, err := os.Stat(path); err == nil && runnable(info) {
					result = appendDetection(result, seen, spec.typeName, path, "known-path")
				}
			}
		}
	}
	for _, special := range specialExecutables() {
		if info, err := os.Stat(special.Path); err == nil && runnable(info) {
			result = appendDetection(result, seen, special.Type, special.Path, "known-path")
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Type == result[j].Type {
			return result[i].Path < result[j].Path
		}
		return result[i].Type < result[j].Type
	})
	return result
}

func specialExecutables() []Detection {
	if runtime.GOOS != "windows" {
		return nil
	}
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		return nil
	}
	var result []Detection
	matches, _ := filepath.Glob(filepath.Join(base, "OpenAI", "Codex", "bin", "*", "codex.exe"))
	for _, path := range matches {
		result = append(result, Detection{Type: "codex", Path: path})
	}
	return result
}

func appendDetection(items []Detection, seen map[string]bool, kind, path, source string) []Detection {
	absolute, err := filepath.Abs(path)
	if err == nil {
		path = absolute
	}
	key := kind + "\x00" + strings.ToLower(filepath.Clean(path))
	if seen[key] {
		return items
	}
	seen[key] = true
	return append(items, Detection{Type: kind, Path: filepath.Clean(path), Source: source})
}

func executableNames(base string) []string {
	if runtime.GOOS == "windows" {
		return []string{base + ".exe", base + ".cmd", base + ".bat", base}
	}
	return []string{base}
}

func runnable(info os.FileInfo) bool {
	if info.IsDir() {
		return false
	}
	return runtime.GOOS == "windows" || info.Mode()&0o111 != 0
}

func knownBinDirs() []string {
	home, _ := os.UserHomeDir()
	dirs := []string{
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, "bin"),
		filepath.Join(home, ".npm-global", "bin"),
	}
	switch runtime.GOOS {
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			dirs = append(dirs, filepath.Join(appData, "npm"))
		}
		if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
			dirs = append(dirs,
				filepath.Join(localAppData, "cursor-agent"),
				filepath.Join(localAppData, "Programs", "cursor", "resources", "app", "bin"),
			)
		}
	case "darwin":
		dirs = append(dirs, "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin")
	default:
		dirs = append(dirs, "/usr/local/bin", "/usr/bin", "/snap/bin")
	}
	return dirs
}
