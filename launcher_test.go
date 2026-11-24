package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackagedLanguageServerLauncher(t *testing.T) {
	binary, err := filepath.Abs(stageBinary(packageDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		language, arg string
	}{
		{"java", "-data"},
		{"kotlin", "--stdio"},
	} {
		t.Run(test.language, func(t *testing.T) {
			directory := t.TempDir()
			root := filepath.Join(directory, "project with spaces")
			tool := filepath.Join(directory, "mock language server")
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			const echoProtocol = "#!/bin/sh\nprintf '%s\\n%s\\n%s\\n' \"$PWD\" \"$1\" \"$JAVA_HOME\" >&2\nexec /bin/cat\n"
			if err := os.WriteFile(tool, []byte(echoProtocol), 0700); err != nil {
				t.Fatal(err)
			}
			gradleJDK := filepath.Join(directory, "Gradle JDK")
			configuredPath := tool
			if test.language == "kotlin" {
				configuredPath = "~/" + filepath.Base(tool)
			}
			cmd := exec.Command(binary, "--language-server", test.language, root, gradleJDK, configuredPath)
			cmd.Env = []string{"HOME=" + directory, "TMPDIR=" + directory, "JAVA_HOME=/launcher-jdk",
				"CED_JDTLS=/legacy-java", "CED_KOTLIN_LSP=/legacy-kotlin"}
			protocol := "Content-Length: 2\r\n\r\n{}"
			cmd.Stdin = strings.NewReader(protocol)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			stdout, err := cmd.Output()
			if err != nil {
				t.Fatalf("%v: %s", err, &stderr)
			}
			realRoot, err := filepath.EvalSymlinks(root)
			if err != nil {
				t.Fatal(err)
			}
			expectedJDK := "/launcher-jdk"
			if test.language == "kotlin" {
				expectedJDK = gradleJDK
			}
			if string(stdout) != protocol || stderr.String() != fmt.Sprintf("%s\n%s\n%s\n", realRoot, test.arg, expectedJDK) {
				t.Fatalf("unexpected launcher streams: stdout %q stderr %q", stdout, &stderr)
			}
			for _, prefix := range []string{"ced-java-lsp-", "ced-kotlin-lsp-"} {
				if directories, _ := filepath.Glob(filepath.Join(directory, prefix+"*")); len(directories) != 0 {
					t.Fatalf("launcher left temporary state: %v", directories)
				}
			}
		})
	}
}

func TestLauncherRejectsRelativeExecutable(t *testing.T) {
	if _, err := launcherExecutable("java", "project-script", nil); err == nil || !strings.Contains(err.Error(), "java.jdtlsPath") {
		t.Fatalf("expected an error naming java.jdtlsPath, got %v", err)
	}
}

func TestLauncherDiscoversExecutableWithoutOverride(t *testing.T) {
	directory := t.TempDir()
	tool := filepath.Join(directory, "kotlin-lsp")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CED_KOTLIN_LSP", "/legacy-kotlin")
	if executable, err := launcherExecutable("kotlin", "", []string{directory}); err != nil || executable != tool {
		t.Fatalf("expected %s, got %s (%v)", tool, executable, err)
	}
}
