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
		language, env, arg string
	}{
		{"java", "CED_JDTLS", "-data"},
		{"kotlin", "CED_KOTLIN_LSP", "--stdio"},
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
			cmd := exec.Command(binary, "--language-server", test.language, root, gradleJDK)
			cmd.Env = []string{test.env + "=" + tool, "TMPDIR=" + directory, "JAVA_HOME=/launcher-jdk"}
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
	t.Setenv("CED_JDTLS", "project-script")
	if _, err := launcherExecutable("java", nil); err == nil {
		t.Fatal("expected an absolute executable path")
	}
}
