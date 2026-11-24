package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func launcherPath() []string {
	var paths []string
	for _, directory := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.IsAbs(directory) {
			paths = append(paths, directory)
		}
	}
	return append(paths, "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin")
}

func launcherExecutable(language, configuredPath string, paths []string) (string, error) {
	var key string
	var names []string
	switch language {
	case "java":
		key, names = "java.jdtlsPath", []string{"jdtls"}
	case "kotlin":
		key, names = "kotlin.lspPath", []string{"kotlin-lsp", "intellij-server"}
	default:
		return "", fmt.Errorf("unknown language server %q", language)
	}
	if configuredPath != "" {
		if strings.HasPrefix(configuredPath, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("%s: %w", key, err)
			}
			configuredPath = filepath.Join(home, configuredPath[2:])
		}
		if !filepath.IsAbs(configuredPath) {
			return "", fmt.Errorf("%s must be an absolute executable path or start with ~/", key)
		}
		executable, err := exec.LookPath(configuredPath)
		if err != nil {
			return "", fmt.Errorf("%s: %w", key, err)
		}
		return executable, nil
	}
	for _, directory := range paths {
		for _, name := range names {
			if path, err := exec.LookPath(filepath.Join(directory, name)); err == nil {
				return path, nil
			}
		}
	}
	return "", fmt.Errorf("install %s or set %s in settings.json to its absolute executable path", names[0], key)
}

func languageServer(language, root, gradleJavaHome, configuredPath string) error {
	if !filepath.IsAbs(root) {
		return fmt.Errorf("the project directory must be absolute")
	}
	paths := launcherPath()
	executable, err := launcherExecutable(language, configuredPath, paths)
	if err != nil {
		return err
	}
	state, err := os.MkdirTemp("", "ced-"+language+"-lsp-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(state)
	args := []string{"--stdio", "--system-path=" + state}
	if language == "java" {
		args = []string{"-data", state, "--jvm-arg=-Xms128m"}
	}
	server := exec.Command(executable, args...)
	server.Dir = root
	server.Env = append(os.Environ(), "PATH="+strings.Join(paths, string(os.PathListSeparator)))
	if language == "kotlin" && gradleJavaHome != "" {
		if !filepath.IsAbs(gradleJavaHome) {
			return fmt.Errorf("the Gradle JDK directory must be absolute")
		}
		server.Env = append(server.Env, "JAVA_HOME="+gradleJavaHome)
	}
	server.Stdin = os.Stdin
	server.Stdout = os.Stdout
	server.Stderr = os.Stderr
	return server.Run()
}
