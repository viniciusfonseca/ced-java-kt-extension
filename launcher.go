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

func launcherExecutable(language string, paths []string) (string, error) {
	var key string
	var names []string
	switch language {
	case "java":
		key, names = "CED_JDTLS", []string{"jdtls"}
	case "kotlin":
		key, names = "CED_KOTLIN_LSP", []string{"kotlin-lsp", "intellij-server"}
	default:
		return "", fmt.Errorf("unknown language server %q", language)
	}
	if path := os.Getenv(key); path != "" {
		if !filepath.IsAbs(path) {
			return "", fmt.Errorf("%s must be an absolute executable path", key)
		}
		return exec.LookPath(path)
	}
	for _, directory := range paths {
		for _, name := range names {
			if path, err := exec.LookPath(filepath.Join(directory, name)); err == nil {
				return path, nil
			}
		}
	}
	return "", fmt.Errorf("install %s or set %s to its absolute executable path", names[0], key)
}

func languageServer(language, root, gradleJavaHome string) error {
	if !filepath.IsAbs(root) {
		return fmt.Errorf("the project directory must be absolute")
	}
	paths := launcherPath()
	executable, err := launcherExecutable(language, paths)
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
