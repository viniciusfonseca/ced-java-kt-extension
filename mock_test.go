package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

const packageDir = "dist/" + extensionID

type object map[string]any

var mockContext = object{"sessionId": "session-test", "workspaceId": "workspace-test", "generation": 1}

type mock struct {
	t      *testing.T
	cmd    *exec.Cmd
	input  io.WriteCloser
	inbox  chan object
	exited chan error
	last   int
}

func stageBinary(dir string) string {
	arch := runtime.GOARCH
	if arch == "amd64" {
		arch = "x86_64"
	}
	return filepath.Join(dir, "bin", runtime.GOOS+"-"+arch, "java-kotlin")
}

type pipePause struct {
	started chan struct{}
	release chan struct{}
}

func startMock(t *testing.T, dir string) *mock {
	t.Helper()
	return startMockWithPause(t, dir, nil)
}

func startMockWithPause(t *testing.T, dir string, pause *pipePause) *mock {
	t.Helper()
	cmd := exec.Command(stageBinary(dir))
	cmd.Dir = dir
	abs, err := filepath.Abs(cmd.Path)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Path = abs
	cmd.Env = []string{"LANG=C"}
	read, stdout, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = stdout, os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("launch packaged executable (run make package first): %v", err)
	}
	stdout.Close()
	m := &mock{t: t, cmd: cmd, input: in, inbox: make(chan object, 32), exited: make(chan error, 1)}
	stop := make(chan struct{})
	go func() {
		defer close(m.inbox)
		r := bufio.NewReader(read)
		for n := 0; ; n++ {
			if pause != nil && n == 3 {
				// Initialization, activation and documents/get are complete.
				// Peek waits for the first bytes of apply without draining its body.
				if _, err := r.Peek(1); err != nil {
					return
				}
				close(pause.started)
				select {
				case <-pause.release:
				case <-stop:
					return
				}
			}
			body, err := readFrame(r, draftWire)
			if err != nil {
				return
			}
			var msg object
			if json.Unmarshal(body, &msg) != nil || msg["jsonrpc"] != "2.0" {
				return
			}
			select {
			case m.inbox <- msg:
			case <-stop:
				return
			}
		}
	}()
	go func() { m.exited <- cmd.Wait(); close(m.exited) }()
	t.Cleanup(func() {
		close(stop)
		in.Close()
		read.Close()
		cmd.Process.Kill()
		<-m.exited
	})
	return m
}

func (m *mock) next() object {
	m.t.Helper()
	select {
	case msg, ok := <-m.inbox:
		if !ok {
			m.t.Fatal("extension closed stdout or wrote a malformed frame")
		}
		return msg
	case <-time.After(4 * time.Second):
		m.t.Fatal("timed out waiting for extension")
		return nil
	}
}

func (m *mock) awaitExit(succeeds bool) {
	m.t.Helper()
	select {
	case err := <-m.exited:
		if succeeds != (err == nil) {
			m.t.Fatalf("unexpected process exit (expected success %t): %v", succeeds, err)
		}
	case <-time.After(2 * time.Second):
		m.t.Fatal("extension did not exit")
	}
}

func (m *mock) silent() {
	m.t.Helper()
	select {
	case msg := <-m.inbox:
		m.t.Fatalf("unexpected output: %v", msg)
	case <-time.After(40 * time.Millisecond):
	}
}

func encode(msg object) []byte {
	body, err := json.Marshal(msg)
	if err != nil {
		panic(err)
	}
	return rawFrame(body)
}

func rawFrame(body []byte) []byte {
	return append(fmt.Appendf(nil, "Content-Length: %d\r\n\r\n", len(body)), body...)
}

func (m *mock) write(data []byte) {
	m.t.Helper()
	if _, err := m.input.Write(data); err != nil {
		m.t.Fatal(err)
	}
}

func (m *mock) request(method string, params object) (string, []byte) {
	m.last++
	id := fmt.Sprintf("ced:%d", m.last)
	if params["context"] == nil {
		params["context"] = mockContext
	}
	return id, encode(object{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
}

func (m *mock) send(method string, params object) string {
	m.t.Helper()
	id, frame := m.request(method, params)
	m.write(frame)
	return id
}

func initParams() object {
	limits := object{}
	for key, maximum := range limitCeiling {
		if maximum == 0 {
			maximum = 1000
		}
		limits[key] = maximum
	}
	limits["commandTimeoutMs"] = 5000
	limits["deactivateTimeoutMs"] = 500
	return object{
		"host": object{"name": "CED", "version": "0.2.0"}, "apiVersions": []string{"1.0"},
		"extension": object{"id": extensionID, "version": version}, "grantedCapabilities": capabilities,
		"workspace": object{"uri": "file:///workspace", "trusted": true}, "hostMethods": methods,
		"limits": limits,
	}
}

func doc(uri string, revision int) object {
	return object{"documentId": "doc-1", "uri": uri, "languageId": "text", "dirty": true,
		"version": revision, "textAccessible": true, "eol": "lf"}
}

func invokeParams(command string, d object, anchor, cursor int) object {
	return object{"commandId": command, "arguments": nil, "editor": object{"document": d,
		"selection": object{"anchor": anchor, "cursor": cursor}, "selectionVersion": 9}}
}

func (m *mock) success(id string) object {
	m.t.Helper()
	msg := m.next()
	result, ok := msg["result"].(map[string]any)
	if !ok || msg["id"] != id || msg["error"] != nil {
		m.t.Fatalf("expected success for %s; got %v", id, msg)
	}
	return result
}

func (m *mock) fault(id any, code int) object {
	m.t.Helper()
	msg := m.next()
	f, ok := msg["error"].(map[string]any)
	if !ok || f["code"] != float64(code) || msg["id"] != id || msg["result"] != nil {
		m.t.Fatalf("expected error %d for %v; got %v", code, id, msg)
	}
	return f
}

func (m *mock) service(method string) (string, object) {
	m.t.Helper()
	msg := m.next()
	params, ok := msg["params"].(map[string]any)
	if !ok || msg["method"] != method {
		m.t.Fatalf("expected service %q, got %v", method, msg)
	}
	id, ok := msg["id"].(string)
	if !ok || !rpcID(id, "ext:") {
		m.t.Fatalf("bad outbound request ID in %v", msg)
	}
	return id, params
}

func (m *mock) result(id string, value object) {
	m.t.Helper()
	m.write(encode(object{"jsonrpc": "2.0", "id": id, "result": value}))
}

func (m *mock) initialize(params object) {
	m.t.Helper()
	response := m.success(m.send("ced/initialize", params))
	identity := response["extension"].(map[string]any)
	if response["apiVersion"] != "1.0" || identity["id"] != extensionID || identity["version"] != version ||
		!slices.Equal(response["requiredHostMethods"].([]any), []any{getMethod, applyMethod}) {
		m.t.Fatalf("bad handshake: %v", response)
	}
}

func (m *mock) activate() {
	m.t.Helper()
	m.success(m.send("ced/activate", object{"reason": object{"event": "onCommand:" + sortID}}))
}

func TestMockLifecycleAndFraming(t *testing.T) {
	m := startMock(t, packageDir)
	d := doc("file:///workspace/Main.java", 1)
	m.fault(m.send("commands/invoke", invokeParams(sortID, d, 0, 0)), -32007)
	m.initialize(initParams())
	m.fault(m.send("commands/invoke", invokeParams(sortID, d, 0, 0)), -32007)
	m.fault(m.send("ced/initialize", initParams()), -32010)
	m.activate()
	id, fragmented := m.request("unknown.é🙂", object{})
	for _, b := range fragmented {
		m.write([]byte{b})
	}
	m.fault(id, -32601)
	unknown, first := m.request("commands/invoke", invokeParams("unknown", d, 0, 0))
	bad := invokeParams(sortID, d, 0, 0)
	bad["arguments"] = []any{}
	invalidID, second := m.request("commands/invoke", bad)
	m.write(append(first, second...))
	m.fault(unknown, -32601)
	m.fault(invalidID, -32602)
	m.fault(m.send("commands/invoke", invokeParams(sortID, doc("file:///workspace/Other.txt", 1), 0, 0)), -32602)
	m.success(m.send("ced/deactivate", object{"reason": "shutdown"}))
	m.awaitExit(true)
}

func TestMockHandshakeRejections(t *testing.T) {
	for _, tc := range []struct {
		key   string
		value any
		code  int
	}{
		{"apiVersions", []string{"2.0"}, -32008},
		{"hostMethods", []string{getMethod}, -32008},
		{"grantedCapabilities", []string{"documents.read"}, -32001},
		{"extension", object{"id": extensionID, "version": "9.9.9"}, -32010},
		{"limits", object{"maxDocumentBytes": 1}, -32602},
	} {
		t.Run(tc.key, func(t *testing.T) {
			m := startMock(t, packageDir)
			params := initParams()
			params[tc.key] = tc.value
			m.fault(m.send("ced/initialize", params), tc.code)
			m.fault(m.send("ced/activate", object{"reason": object{"event": "onCommand:" + sortID}}), -32007)
		})
	}
}

func TestMockProtocolRejections(t *testing.T) {
	m := startMock(t, packageDir)
	for _, body := range [][]byte{
		[]byte(`{"x":"a","\u0078":"b"}`),
		[]byte(`{"x":`),
		[]byte(`{"x":"\uD800"}`),
		[]byte(`{"x":1e999}`),
		{'"', 0xff, '"'},
		[]byte(strings.Repeat("[", 65) + "null" + strings.Repeat("]", 65)),
	} {
		m.write(rawFrame(body))
		m.fault(nil, -32700)
	}
	m.write(rawFrame([]byte(`[]`)))
	m.fault(nil, -32600)
	m.write(encode(object{"jsonrpc": "2.0", "id": 3, "method": "bad", "params": object{}}))
	m.fault(nil, -32600)
	m.initialize(initParams())
	for _, header := range []string{
		strings.Repeat("a", 1025), "Content-Length: 16777217\r\n\r\n",
		"Content-Length: -1\r\n\r\n", "Content-Length: +1\r\n\r\n", "Content-Length: 0\r\n\r\n",
		"Content-Length: 1\r\nContent-Length: 1\r\n\r\n",
	} {
		t.Run(header[:min(len(header), 45)], func(t *testing.T) {
			bad := startMock(t, packageDir)
			bad.write([]byte(header))
			bad.awaitExit(false)
		})
	}
}

func TestMockSnapshotAndAtomicEdit(t *testing.T) {
	m := startMock(t, packageDir)
	m.initialize(initParams())
	m.activate()
	filename := filepath.Join(t.TempDir(), "Main.java")
	disk := []byte("class OnDisk {}\n")
	if err := os.WriteFile(filename, disk, 0600); err != nil {
		t.Fatal(err)
	}
	d := doc("file://"+filename, 7)
	text := "/*é🙂*/\n\nimport z.B;\nimport a.A;\nimport static z.B.x;\nimport static a.A.y;\n"
	command := m.send("commands/invoke", invokeParams(sortID, d, 8, 2))
	get, query := m.service(getMethod)
	if query["documentId"] != "doc-1" {
		t.Fatalf("wrong captured document: %v", query)
	}
	busy, pending := m.request("commands/invoke", invokeParams(sortID, d, 2, 8))
	noise := encode(object{"jsonrpc": "2.0", "method": "events/publish", "params": object{"context": mockContext}})
	early := encode(object{"jsonrpc": "2.0", "id": "ext:2", "result": object{}})
	snapshot := encode(object{"jsonrpc": "2.0", "id": get, "result": object{"document": d, "text": text}})
	m.write(bytes.Join([][]byte{pending, noise, early, snapshot}, nil))
	m.fault(busy, -32006)
	applyID, transaction := m.service(applyMethod)
	if transaction["expectedVersion"] != float64(7) || transaction["expectedSelectionVersion"] != float64(9) ||
		transaction["documentId"] != "doc-1" {
		t.Fatalf("wrong preconditions: %v", transaction)
	}
	rawEdits, _ := json.Marshal(transaction["edits"])
	var edits []edit
	if err := json.Unmarshal(rawEdits, &edits); err != nil || len(edits) != 2 || edits[0].Start != len("/*é🙂*/\n\n") {
		t.Fatalf("invalid edit ranges: %s", rawEdits)
	}
	want := "/*é🙂*/\n\nimport a.A;\nimport z.B;\nimport static a.A.y;\nimport static z.B.x;\n"
	if result := apply(text, edits); result != want {
		t.Fatalf("wrong edit result: %q", result)
	}
	m.result(applyID, object{"documentId": "doc-1", "version": 8, "changed": true})
	if result := m.success(command); result["version"] != float64(8) || result["changed"] != true {
		t.Fatalf("wrong command result: %v", result)
	}
	if content, err := os.ReadFile(filename); err != nil || !bytes.Equal(content, disk) {
		t.Fatalf("command modified file on disk: %q, %v", content, err)
	}
	d["version"] = 8
	command = m.send("commands/invoke", invokeParams(sortID, d, 0, 0))
	get, _ = m.service(getMethod)
	m.result(get, object{"document": d, "text": want})
	if result := m.success(command); result["changed"] != false || result["version"] != float64(8) {
		t.Fatalf("wrong no-op: %v", result)
	}
	m.silent()
}

func TestMockStaleAndCancelled(t *testing.T) {
	for _, mode := range []string{"revision", "moved selection", "closed", "renamed", "invalid boundary", "replaced document", "broker conflict", "timeout during apply", "cancel during apply", "disconnect after commit", "workspace change"} {
		t.Run(mode, func(t *testing.T) {
			m := startMock(t, packageDir)
			params := initParams()
			if mode == "timeout during apply" {
				params["limits"].(object)["serviceTimeoutMs"] = 100
			}
			m.initialize(params)
			m.activate()
			d := doc("file:///workspace/Main.kt", 1)
			start := 0
			if mode == "invalid boundary" {
				start = 3
			}
			command := m.send("commands/invoke", invokeParams(sortID, d, start, start))
			get, _ := m.service(getMethod)
			if mode == "closed" {
				m.write(encode(object{"jsonrpc": "2.0", "id": get, "error": object{"code": -32011,
					"message": "Unavailable", "data": object{"reason": "Unavailable"}}}))
				m.fault(command, -32011)
				m.silent()
				return
			}
			if mode == "workspace change" {
				ctx := object{"sessionId": "session-test", "workspaceId": "new-workspace", "generation": 2}
				switchID := m.send("commands/invoke", object{"context": ctx})
				m.fault(command, -32004)
				m.fault(switchID, -32003)
				m.awaitExit(true)
				return
			}
			if mode == "revision" {
				d["version"] = 2
			}
			if mode == "renamed" {
				d["uri"] = "file:///workspace/Other.kt"
			}
			if mode == "replaced document" {
				d["documentId"] = "doc-2"
			}
			m.result(get, object{"document": d, "text": "//é🙂\n\nimport z.B\nimport a.A\n"})
			switch mode {
			case "revision":
				f := m.fault(command, -32002)
				if f["data"].(map[string]any)["currentVersion"] != float64(2) {
					t.Fatalf("missing current revision: %v", f)
				}
			case "renamed":
				m.fault(command, -32012)
			case "invalid boundary":
				m.fault(command, -32602)
			case "replaced document":
				m.fault(command, -32603)
			default:
				applyID, _ := m.service(applyMethod)
				switch mode {
				case "moved selection", "broker conflict":
					code, reason := -32012, "StaleEditorContext"
					if mode == "broker conflict" {
						code, reason = -32002, "StaleDocument"
					}
					m.write(encode(object{"jsonrpc": "2.0", "id": applyID, "error": object{
						"code": code, "message": "Changed", "data": object{"reason": reason, "currentVersion": 2},
					}}))
					if f := m.fault(command, code); f["data"].(map[string]any)["reason"] != reason {
						t.Fatalf("lost host reason: %v", f)
					}
				case "cancel during apply", "timeout during apply":
					code := -32009
					if mode == "cancel during apply" {
						code = -32004
						m.write(encode(object{"jsonrpc": "2.0", "method": "ced/cancel",
							"params": object{"context": mockContext, "requestId": command}}))
					}
					cancel := m.next()
					if cancel["method"] != "ced/cancel" || cancel["params"].(map[string]any)["requestId"] != applyID {
						t.Fatalf("wrong descendant cancellation: %v", cancel)
					}
					if f := m.fault(command, code); !strings.Contains(f["message"].(string), "outcome is unknown") {
						t.Fatalf("missing unknown outcome: %v", f)
					}
					m.result(applyID, object{"documentId": "doc-1", "version": 2, "changed": true})
				case "disconnect after commit":
					m.input.Close() // The mock committed the transaction, then lost the response.
					m.awaitExit(true)
					if msg, ok := <-m.inbox; ok {
						t.Fatalf("unexpected output after disconnect: %v", msg)
					}
					return
				}
			}
			m.silent()
		})
	}
}

func TestMockInsertAndQuotas(t *testing.T) {
	for _, tc := range []struct {
		name, filename, text  string
		bytes, edits, failure int
	}{
		{"Java", "Main.java", "", 200, 1, 0},
		{"Kotlin file", "main.kt", "", 200, 1, 0},
		{"Kotlin script", "main.kts", "", 200, 1, 0},
		{"Require empty", "main.kt", "fun foo() {}", 200, 1, -32602},
		{"Growth limit", "Main.java", "", 8, 1, -32005},
		{"Edit count limit", "main.kt", "import z.B\nimport a.A\n\nimport z.C\nimport a.C\n", 200, 1, -32005},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := startMock(t, packageDir)
			params := initParams()
			limits := params["limits"].(object)
			limits["maxDocumentBytes"], limits["maxEditsPerTransaction"] = tc.bytes, tc.edits
			m.initialize(params)
			m.activate()
			d := doc("file:///workspace/"+tc.filename, 1)
			command := mainID
			if tc.name == "Edit count limit" {
				command = sortID
			}
			id := m.send("commands/invoke", invokeParams(command, d, 0, 0))
			get, _ := m.service(getMethod)
			m.result(get, object{"document": d, "text": tc.text})
			if tc.failure != 0 {
				m.fault(id, tc.failure)
				m.silent()
				return
			}
			applyID, params := m.service(applyMethod)
			edits := params["edits"].([]any)
			item := edits[0].(map[string]any)
			if len(edits) != 1 || item["start"] != float64(0) || item["end"] != float64(0) ||
				!strings.Contains(item["text"].(string), "main(") {
				t.Fatalf("wrong insert: %v", edits)
			}
			m.result(applyID, object{"documentId": "doc-1", "version": 2, "changed": true})
			// Cancellation after a host-confirmed result cannot undo the edit.
			m.write(encode(object{"jsonrpc": "2.0", "method": "ced/cancel",
				"params": object{"context": mockContext, "requestId": id}}))
			m.success(id)
			m.silent()
		})
	}
}

func TestMockStop(t *testing.T) {
	for _, mode := range []string{"deactivate", "EOF", "SIGTERM"} {
		t.Run(mode, func(t *testing.T) {
			m := startMock(t, packageDir)
			m.initialize(initParams())
			m.activate()
			id := m.send("commands/invoke", invokeParams(mainID, doc("file:///workspace/Main.java", 1), 0, 0))
			m.service(getMethod)
			switch mode {
			case "deactivate":
				shutdown := m.send("ced/deactivate", object{"reason": "disable"})
				m.fault(id, -32004)
				m.success(shutdown)
			case "EOF":
				m.input.Close()
			case "SIGTERM":
				if err := m.cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			}
			m.awaitExit(mode != "SIGTERM")
			if msg, ok := <-m.inbox; ok {
				t.Fatalf("unexpected output on stop: %v", msg)
			}
		})
	}
}

func TestMockSlowOutput(t *testing.T) {
	for _, mode := range []string{"cancellation", "queue saturation", "deactivation"} {
		t.Run(mode, func(t *testing.T) {
			pause := &pipePause{started: make(chan struct{}), release: make(chan struct{})}
			m := startMockWithPause(t, packageDir, pause)
			m.initialize(initParams())
			m.activate()
			d := doc("file:///workspace/Main.kt", 1)
			id := m.send("commands/invoke", invokeParams(sortID, d, 0, 0))
			get, _ := m.service(getMethod)
			text := "import z." + strings.Repeat("Z", 1<<20) + "\nimport a.A\n"
			m.result(get, object{"document": d, "text": text})
			select {
			case <-pause.started:
			case <-time.After(2 * time.Second):
				t.Fatal("apply did not reach the output pipe")
			}
			switch mode {
			case "cancellation":
				m.write(encode(object{"jsonrpc": "2.0", "method": "ced/cancel",
					"params": object{"context": mockContext, "requestId": id}}))
				close(pause.release)
				applyID, _ := m.service(applyMethod)
				cancel := m.next()
				if cancel["method"] != "ced/cancel" ||
					cancel["params"].(map[string]any)["requestId"] != applyID {
					t.Fatalf("wrong cancellation after output resumed: %v", cancel)
				}
				m.fault(id, -32004)
				m.silent()
			case "queue saturation":
				var burst []byte
				for n := 0; n < 8; n++ {
					_, frame := m.request("unknown", object{})
					burst = append(burst, frame...)
				}
				m.write(burst)
				m.awaitExit(false)
			case "deactivation":
				m.send("ced/deactivate", object{"reason": "shutdown"})
				m.awaitExit(false)
			}
		})
	}
}

func TestStagedPackageMoves(t *testing.T) {
	copyTo := t.TempDir()
	bytes := int64(0)
	count := 0
	err := filepath.WalkDir(packageDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		count++
		relative, err := filepath.Rel(packageDir, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(copyTo, relative)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0755)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("unexpected file type: %s", relative)
		}
		stat, err := entry.Info()
		if err != nil {
			return err
		}
		bytes += stat.Size()
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(destination, data, stat.Mode().Perm())
	})
	if err != nil || count > 2000 || bytes > 64<<20 {
		t.Fatalf("invalid staged assets: %d files, %d bytes, %v", count, bytes, err)
	}
	raw, err := os.ReadFile(filepath.Join(copyTo, "ced-extension.json"))
	if err != nil || len(raw) > 256<<10 || !validJSON(raw, 32) {
		t.Fatalf("invalid staged manifest: %v", err)
	}
	var manifest struct {
		ID, Version, Kind string
		ManifestVersion   int
		Entrypoints       map[string]string
		ActivationEvents  []string
		Capabilities      []string
		Untrusted         *bool `json:"untrustedWorkspaces"`
		Contributes       struct {
			Commands        []struct{ ID string }
			LanguageServers []string
		}
	}
	if err := json.Unmarshal(raw, &manifest); err != nil || manifest.ID != extensionID ||
		manifest.Version != version || manifest.Kind != "executable" || manifest.ManifestVersion != 1 ||
		!slices.Equal(manifest.Capabilities, capabilities) || manifest.Untrusted == nil || *manifest.Untrusted ||
		!slices.Equal(manifest.Contributes.LanguageServers, []string{"java", "kotlin"}) || len(manifest.Contributes.Commands) != 2 || len(manifest.ActivationEvents) != 2 || len(manifest.Entrypoints) != 4 {
		t.Fatalf("invalid manifest: %+v, %v", manifest, err)
	}
	for _, command := range manifest.Contributes.Commands {
		if !slices.Contains(manifest.ActivationEvents, "onCommand:"+command.ID) {
			t.Fatalf("missing activation for %s", command.ID)
		}
	}
	for _, platform := range []string{"darwin-arm64", "darwin-x86_64", "linux-arm64", "linux-x86_64"} {
		want := filepath.ToSlash(filepath.Join("bin", platform, "java-kotlin"))
		if manifest.Entrypoints[platform] != want {
			t.Fatalf("wrong entrypoint for %s: %q", platform, manifest.Entrypoints[platform])
		}
		info, err := os.Stat(filepath.Join(copyTo, want))
		if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
			t.Fatalf("entrypoint %s is unavailable or non-executable: %v", want, err)
		}
	}
	m := startMock(t, copyTo)
	m.initialize(initParams())
	m.activate()
	m.success(m.send("ced/deactivate", object{"reason": "shutdown"}))
	m.awaitExit(true)
}
