package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	extensionID = "viniciusfonseca.java-kotlin"
	version     = "0.2.2"
	sortID      = extensionID + ".sortImports"
	mainID      = extensionID + ".insertMain"
	getMethod   = "documents/get"
	applyMethod = "documents/applyEdits"
)

var (
	capabilities = []string{"documents.read", "documents.edit"}
	methods      = []string{getMethod, applyMethod}
	versionRE    = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	limitCeiling = map[string]int{
		"maxHeaderBytes": 1024, "maxMessageBytes": 16 << 20, "maxJsonDepth": 64,
		"maxQueuedBytes": 16 << 20, "maxPendingRequests": 64, "maxDocumentBytes": 2 << 20,
		"maxEditsPerTransaction": 1000, "maxSubscriptions": 32, "maxCommandArgumentsBytes": 65536,
		"maxCommandResultBytes": 65536, "maxStateBytes": 262144, "maxPresentationBytes": 512,
		"maxLogMessageBytes": 8192, "maxLogBytesPerSecond": 65536, "maxMessagesPer10Seconds": 5,
		"initializeTimeoutMs": 0, "activateTimeoutMs": 0, "commandTimeoutMs": 0,
		"serviceTimeoutMs": 0, "deactivateTimeoutMs": 0,
	}
)

type session struct {
	SessionID   string `json:"sessionId"`
	WorkspaceID string `json:"workspaceId"`
	Generation  uint64 `json:"generation"`
}

type document struct {
	ID, URI        string
	Version        uint64
	TextAccessible bool
}

type selection struct{ anchor, cursor uint64 }

type editor struct {
	document         document
	selection        selection
	selectionVersion uint64
}

type invocation struct {
	id, command, extension, name string
	editor                       editor
	requestID, method            string
	cancel                       chan struct{}
	deadline                     time.Time
}

type stage int

const (
	waiting stage = iota
	initialized
	active
	stopping
)

type peer struct {
	out     *output
	wire    wireLimits
	state   stage
	context session
	limits  map[string]int
	job     *invocation
	timer   *time.Timer
	counter uint64
	lastID  string
	failure error
	logged  int
}

type rpcFault struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func fault(code int, reason, message string) *rpcFault {
	f := &rpcFault{Code: code, Message: message}
	if reason != "" {
		f.Data = map[string]any{"reason": reason}
	}
	return f
}

func invalid(message string) *rpcFault { return fault(-32602, "", message) }

func validID(id string) bool { return id != "" && len(id) <= 128 && !strings.ContainsRune(id, 0) }

func safe(n uint64) bool { return n <= (1<<53)-1 }

func parseContext(parent fields) (session, bool) {
	var ctx session
	var raw fields
	ok := decode(parent, "context", &raw) &&
		decode(raw, "sessionId", &ctx.SessionID) && validID(ctx.SessionID) &&
		decode(raw, "workspaceId", &ctx.WorkspaceID) && validID(ctx.WorkspaceID) &&
		decode(raw, "generation", &ctx.Generation) && safe(ctx.Generation)
	return ctx, ok
}

func parseDocument(parent fields) (document, bool) {
	var doc document
	var raw fields
	var language, eol string
	var dirty bool
	if !decode(parent, "document", &raw) || !decode(raw, "documentId", &doc.ID) || !validID(doc.ID) ||
		!decode(raw, "version", &doc.Version) || doc.Version == 0 || !safe(doc.Version) ||
		!decode(raw, "languageId", &language) || len(language) > 128 || strings.ContainsRune(language, 0) ||
		!decode(raw, "dirty", &dirty) || !decode(raw, "textAccessible", &doc.TextAccessible) ||
		!decode(raw, "eol", &eol) || (eol != "lf" && eol != "crlf") {
		return document{}, false
	}
	if !bytes.Equal(raw["uri"], []byte("null")) {
		if !decode(raw, "uri", &doc.URI) {
			return document{}, false
		}
		u, err := url.Parse(doc.URI)
		if err != nil || !u.IsAbs() {
			return document{}, false
		}
	}
	return doc, true
}

type incoming struct {
	body []byte
	err  error
}

func (p *peer) run(reader io.Reader) error {
	// Hand the next read its limits only after dispatching the previous frame.
	reads := make(chan wireLimits, 1)
	messages := make(chan incoming)
	go func() {
		r := bufio.NewReader(reader)
		for limits := range reads {
			body, err := readFrame(r, limits)
			messages <- incoming{body, err}
			if err != nil && err != errJSON {
				return
			}
		}
	}()
	reads <- p.wire
	for p.failure == nil && p.state != stopping {
		var timeout <-chan time.Time
		if p.timer != nil {
			timeout = p.timer.C
		}
		select {
		case err := <-p.out.done:
			return err
		case <-timeout:
			p.cancelJob(true, true)
		case in := <-messages:
			switch in.err {
			case io.EOF, io.ErrUnexpectedEOF:
				return nil
			case errJSON:
				p.reply("", nil, fault(-32700, "", "Invalid JSON."))
			case nil:
				p.dispatch(in.body)
			default:
				return in.err
			}
			if p.state != stopping {
				reads <- p.wire
			}
		}
	}
	if p.failure != nil {
		return p.failure
	}
	select {
	case err := <-p.out.done:
		return err
	case <-time.After(p.duration("deactivateTimeoutMs")):
		return errors.New("deactivation output timed out")
	}
}

func (p *peer) diagnose(message string) {
	if p.logged < 8 {
		fmt.Fprintln(os.Stderr, message)
		p.logged++
	}
}

func (p *peer) dispatch(body []byte) {
	var msg fields
	var protocol, id, method string
	if json.Unmarshal(body, &msg) != nil {
		p.reply("", nil, fault(-32600, "", "Invalid request envelope."))
		return
	}
	if _, hasID := msg["id"]; hasID {
		decode(msg, "id", &id)
	}
	_, hasMethod := msg["method"]
	_, hasResult := msg["result"]
	_, hasError := msg["error"]
	_, hasID := msg["id"]
	validProtocol := decode(msg, "jsonrpc", &protocol) && protocol == "2.0"
	if !hasMethod && (hasResult || hasError) {
		if !validProtocol || !rpcID(id, "ext:") || hasResult == hasError || msg["params"] != nil {
			p.diagnose("Invalid host response.")
			return
		}
		p.handleResponse(id, msg)
		return
	}
	if !validProtocol || !decode(msg, "method", &method) || method == "" || hasResult || hasError || (hasID && !rpcID(id, "ced:")) {
		if hasMethod && !hasID {
			p.diagnose("Invalid host notification.")
		} else {
			if !rpcID(id, "ced:") {
				id = ""
			}
			p.reply(id, nil, fault(-32600, "", "Invalid request envelope."))
		}
		return
	}
	if hasID {
		digits := strings.TrimPrefix(id, "ced:")
		if len(p.lastID) > len(digits) || len(p.lastID) == len(digits) && p.lastID >= digits {
			p.diagnose("Repeated or out-of-order host request ID.")
			return
		}
		p.lastID = digits
	}
	var params fields
	if !decode(msg, "params", &params) {
		p.respond(id, invalid("Expected a params object."))
		return
	}
	if method == "ced/initialize" {
		if id == "" {
			p.diagnose("Initialization requires a request ID.")
			return
		}
		p.initialize(id, params)
		return
	}
	if p.state == waiting {
		p.respond(id, fault(-32007, "NotActive", "The extension is not initialized."))
		return
	}
	ctx, ok := parseContext(params)
	if !ok {
		p.respond(id, invalid("Missing or malformed context."))
		return
	}
	if ctx != p.context {
		p.cancelJob(false, false)
		p.state = stopping
		if id != "" {
			p.respond(id, fault(-32003, "InvalidContext", "The workspace context is no longer active."))
		} else {
			p.failure = errors.New("invalid session context")
		}
		return
	}
	if id == "" {
		if method == "ced/cancel" {
			var requestID string
			if !decode(params, "requestId", &requestID) || !rpcID(requestID, "ced:") {
				p.diagnose("Invalid cancellation notification.")
			} else if p.job != nil && requestID == p.job.id {
				p.cancelJob(false, true)
			}
		} else {
			p.diagnose("Ignored unsupported host notification.")
		}
		return
	}

	switch method {
	case "ced/activate":
		var reason fields
		var event string
		if p.state != initialized {
			p.respond(id, fault(-32007, "NotActive", "Activation is unavailable in this state."))
		} else if !decode(params, "reason", &reason) || !decode(reason, "event", &event) ||
			event != "onCommand:"+sortID && event != "onCommand:"+mainID {
			p.respond(id, invalid("Expected a declared activation event."))
		} else {
			p.reply(id, struct{}{}, nil)
			p.state = active
		}
	case "ced/deactivate":
		var reason string
		if !decode(params, "reason", &reason) ||
			!slices.Contains([]string{"disable", "uninstall", "workspaceChanged", "shutdown", "restart"}, reason) {
			p.respond(id, invalid("Expected a shutdown reason."))
			return
		}
		p.cancelJob(false, false)
		p.state = stopping
		p.reply(id, struct{}{}, nil)
	case "commands/invoke":
		p.invoke(id, params)
	default:
		p.respond(id, fault(-32601, "", "Unknown request method."))
	}
}

func (p *peer) initialize(id string, params fields) {
	var host, identity, workspace fields
	var hostName, hostVersion, offeredID, offeredVersion, root string
	var trusted bool
	var api, grants, services []string
	ctx, hasContext := parseContext(params)
	fail := func(f *rpcFault) { p.respond(id, f) }
	switch {
	case p.state != waiting:
		fail(fault(-32010, "Conflict", "The extension is already initialized."))
	case !decode(params, "host", &host) || !decode(host, "name", &hostName) || hostName != "CED" ||
		!decode(host, "version", &hostVersion) || !versionRE.MatchString(hostVersion) ||
		!decode(params, "extension", &identity) || !decode(identity, "id", &offeredID) ||
		!decode(identity, "version", &offeredVersion) || !hasContext || !decode(params, "workspace", &workspace) ||
		!decode(workspace, "uri", &root) || root == "" || !decode(workspace, "trusted", &trusted) ||
		!decode(params, "apiVersions", &api) || !decode(params, "hostMethods", &services) ||
		!decode(params, "grantedCapabilities", &grants):
		fail(invalid("Invalid initialization parameters."))
	case offeredID != extensionID || offeredVersion != version:
		fail(fault(-32010, "Conflict", "The package identity does not match the executable."))
	case !slices.Contains(api, "1.0") || !containsAll(services, methods):
		fail(fault(-32008, "UnsupportedCapability", "CED API 1.0 and document snapshot/edit services are required."))
	case !trusted || !containsAll(grants, capabilities):
		fail(fault(-32001, "PermissionDenied", "A trusted workspace and documents.read and documents.edit grants are required."))
	default:
		var offeredLimits fields
		if !decode(params, "limits", &offeredLimits) {
			fail(invalid("Expected negotiated limits."))
			return
		}
		limits := make(map[string]int, len(limitCeiling))
		for key, ceiling := range limitCeiling {
			var n int
			if !decode(offeredLimits, key, &n) || n <= 0 || (ceiling > 0 && n > ceiling) ||
				(ceiling == 0 && uint64(n) > uint64(time.Duration(1<<63-1)/time.Millisecond)) {
				fail(invalid("Invalid negotiated limits."))
				return
			}
			limits[key] = n
		}
		p.wire = wireLimits{limits["maxHeaderBytes"], limits["maxMessageBytes"],
			limits["maxJsonDepth"], limits["maxQueuedBytes"]}
		p.limits, p.context = limits, ctx
		p.reply(id, map[string]any{
			"extension":  map[string]string{"id": extensionID, "version": version},
			"apiVersion": "1.0", "requiredHostMethods": methods,
		}, nil)
		p.state = initialized
	}
}

func containsAll(have, required []string) bool {
	for _, value := range required {
		if !slices.Contains(have, value) {
			return false
		}
	}
	return true
}

func (p *peer) duration(key string) time.Duration {
	return time.Duration(p.limits[key]) * time.Millisecond
}

func (p *peer) reply(id string, result any, f *rpcFault) {
	msg := map[string]any{"jsonrpc": "2.0", "id": nil}
	if id != "" {
		msg["id"] = id
	}
	if f == nil {
		msg["result"] = result
	} else {
		msg["error"] = f
	}
	if p.failure == nil {
		p.failure = p.out.send(p.wire, msg, nil, p.state == stopping)
	}
}

func (p *peer) respond(id string, f *rpcFault) {
	if id == "" {
		p.diagnose("Ignored invalid host notification.")
		return
	}
	p.reply(id, nil, f)
}

func (p *peer) invoke(id string, params fields) {
	if p.state != active {
		p.respond(id, fault(-32007, "NotActive", "Activate the extension before invoking a command."))
		return
	}
	if p.job != nil {
		p.respond(id, fault(-32006, "Busy", "Another language command is running."))
		return
	}
	var command string
	if !decode(params, "commandId", &command) {
		p.respond(id, invalid("Expected a command ID."))
		return
	}
	if command != sortID && command != mainID {
		p.respond(id, fault(-32601, "", "Unknown command."))
		return
	}
	if len(params["arguments"]) > p.limits["maxCommandArgumentsBytes"] {
		p.respond(id, fault(-32005, "ResourceLimit", "The command arguments exceed CED's negotiated limit."))
		return
	}
	var captured fields
	var selectionVersion uint64
	if !bytes.Equal(params["arguments"], []byte("null")) || !decode(params, "editor", &captured) ||
		!decode(captured, "selectionVersion", &selectionVersion) || !safe(selectionVersion) {
		p.respond(id, invalid("Pass null arguments and a valid editor context."))
		return
	}
	if bytes.Equal(captured["document"], []byte("null")) {
		if !bytes.Equal(captured["selection"], []byte("null")) {
			p.respond(id, invalid("Invalid editor selection."))
			return
		}
		p.respond(id, fault(-32001, "PermissionDenied", "Focus an accessible Java or Kotlin document in the trusted workspace."))
		return
	}
	doc, ok := parseDocument(captured)
	if !ok {
		p.respond(id, invalid("Invalid captured document."))
		return
	}
	if !doc.TextAccessible {
		p.respond(id, fault(-32011, "Unavailable", "The current buffer does not expose UTF-8 text."))
		return
	}
	var raw fields
	var selected selection
	if !decode(captured, "selection", &raw) || !decode(raw, "anchor", &selected.anchor) ||
		!safe(selected.anchor) || !decode(raw, "cursor", &selected.cursor) || !safe(selected.cursor) {
		p.respond(id, invalid("Invalid editor selection."))
		return
	}
	ext, name := sourceFile(doc.URI)
	if ext != ".java" && ext != ".kt" && ext != ".kts" {
		p.respond(id, invalid("Open a named .java, .kt, or .kts file in the editor."))
		return
	}
	p.job = &invocation{id: id, command: command, extension: ext, name: name,
		editor: editor{doc, selected, selectionVersion}, deadline: time.Now().Add(p.duration("commandTimeoutMs"))}
	p.service(getMethod, map[string]any{"documentId": doc.ID})
}

func (p *peer) service(method string, params map[string]any) {
	job := p.job
	if !time.Now().Before(job.deadline) {
		p.cancelJob(true, false)
		return
	}
	p.counter++
	job.requestID = "ext:" + strconv.FormatUint(p.counter, 10)
	job.cancel = make(chan struct{})
	params["context"] = p.context
	msg := map[string]any{"jsonrpc": "2.0", "id": job.requestID, "method": method, "params": params}
	if err := p.out.send(p.wire, msg, job.cancel, false); err != nil {
		if err == errQuota {
			p.complete(nil, fault(-32005, "ResourceLimit", "The request exceeds CED's negotiated limits."))
		} else {
			p.failure = err
		}
		return
	}
	job.method = method
	p.timer = time.NewTimer(min(time.Until(job.deadline), p.duration("serviceTimeoutMs")))
}

func (p *peer) finishJob() *invocation {
	job := p.job
	if job != nil && job.cancel != nil {
		close(job.cancel)
	}
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	p.job = nil
	return job
}

func (p *peer) complete(result any, f *rpcFault) {
	job := p.finishJob()
	if f == nil {
		raw, err := json.Marshal(result)
		if err != nil || len(raw) > p.limits["maxCommandResultBytes"] {
			f = fault(-32005, "ResourceLimit", "The command result exceeds CED's negotiated limits.")
		}
	}
	p.reply(job.id, result, f)
}

func (p *peer) cancelJob(timedOut, notify bool) {
	job := p.finishJob()
	if job == nil {
		return
	}
	f := fault(-32004, "Cancelled", "The command was cancelled.")
	if timedOut {
		f = fault(-32009, "Timeout", "The document service timed out.")
	}
	if job.method == applyMethod {
		f.Message += " The edit outcome is unknown."
	}
	if notify {
		msg := map[string]any{"jsonrpc": "2.0", "method": "ced/cancel",
			"params": map[string]any{"context": p.context, "requestId": job.requestID}}
		p.failure = p.out.send(p.wire, msg, nil, false)
	}
	p.reply(job.id, nil, f)
}

func (p *peer) handleResponse(id string, msg fields) {
	job := p.job
	if job == nil || job.requestID != id {
		p.diagnose("Ignored a response without a pending request.")
		return
	}
	p.timer.Stop()
	p.timer = nil
	if raw, hasError := msg["error"]; hasError {
		var f fields
		var code int
		var message string
		if json.Unmarshal(raw, &f) != nil || !decode(f, "code", &code) || !decode(f, "message", &message) {
			p.badResponse()
			return
		}
		var data any
		if raw := f["data"]; raw != nil {
			data = raw
		}
		switch code {
		case -32002, -32012:
			message = "The document or editor context changed. Run the command again."
		case -32001:
			message = "CED denied access to the captured document."
		case -32009, -32004:
			if job.method == applyMethod {
				message = "The edit was interrupted. Its outcome is unknown."
			}
		}
		if code == -32003 {
			p.state = stopping
		}
		p.complete(nil, &rpcFault{code, message, data})
		return
	}
	var result fields
	if !decode(msg, "result", &result) {
		p.badResponse()
		return
	}
	if job.method == getMethod {
		job.method = ""
		p.snapshot(result)
		return
	}
	var documentID string
	var revision uint64
	var changed bool
	if !decode(result, "documentId", &documentID) || documentID != job.editor.document.ID ||
		!decode(result, "version", &revision) || !safe(revision) ||
		!decode(result, "changed", &changed) ||
		(changed && revision != job.editor.document.Version+1) ||
		(!changed && revision != job.editor.document.Version) {
		p.badResponse()
		return
	}
	p.complete(commandResult{changed, revision}, nil)
}

func (p *peer) badResponse() {
	message := "CED returned an invalid document response."
	if p.job.method == applyMethod {
		message += " The edit outcome is unknown."
	}
	p.complete(nil, fault(-32603, "", message))
}

type commandResult struct {
	Changed bool   `json:"changed"`
	Version uint64 `json:"version"`
}

func (p *peer) snapshot(result fields) {
	job := p.job
	doc, ok := parseDocument(result)
	var text string
	if !ok || doc.ID != job.editor.document.ID ||
		!decode(result, "text", &text) || strings.ContainsRune(text, 0) {
		p.badResponse()
		return
	}
	if !doc.TextAccessible {
		p.complete(nil, fault(-32011, "Unavailable", "The captured buffer does not expose UTF-8 text."))
		return
	}
	if doc.Version != job.editor.document.Version {
		p.complete(nil, &rpcFault{-32002, "The buffer changed. Run the command again.",
			map[string]any{"reason": "StaleDocument", "currentVersion": doc.Version}})
		return
	}
	if doc.URI != job.editor.document.URI {
		p.complete(nil, fault(-32012, "StaleEditorContext", "The document was renamed. Run the command again."))
		return
	}
	s := job.editor.selection
	if !boundary(text, s.anchor) || !boundary(text, s.cursor) {
		p.complete(nil, invalid("The captured selection does not lie on UTF-8 boundaries."))
		return
	}
	if len(text) > p.limits["maxDocumentBytes"] {
		p.complete(nil, fault(-32005, "ResourceLimit", "The document exceeds the negotiated byte limit."))
		return
	}

	var edits []edit
	label := "Sort Imports"
	if job.command == sortID {
		var err error
		edits, err = sortImports(text, job.extension == ".java", p.limits["maxEditsPerTransaction"])
		if err != nil {
			p.complete(nil, fault(-32005, "ResourceLimit", err.Error()))
			return
		}
	} else {
		label = "Insert Main"
		if text != "" {
			p.complete(nil, invalid("Insert Main requires an empty buffer."))
			return
		}
		template, err := mainTemplate(job.extension, job.name)
		if err != nil {
			p.complete(nil, invalid(err.Error()))
			return
		}
		edits = []edit{{0, 0, template}}
	}
	if len(edits) == 0 {
		p.complete(commandResult{false, doc.Version}, nil)
		return
	}
	finalBytes := len(text)
	for _, e := range edits {
		finalBytes += len(e.Text) - (e.End - e.Start)
	}
	// The transforms construct sorted, disjoint ranges and preserve complete UTF-8 lines.
	// Check the success-result budget before issuing a mutation.
	if finalBytes > p.limits["maxDocumentBytes"] || len(label) > p.limits["maxPresentationBytes"] ||
		p.limits["maxCommandResultBytes"] < len(`{"changed":false,"version":}`)+len(strconv.FormatUint(doc.Version+1, 10)) {
		p.complete(nil, fault(-32005, "ResourceLimit", "The edit exceeds CED's negotiated limits."))
		return
	}
	if doc.Version == (1<<53)-1 {
		p.complete(nil, fault(-32005, "ResourceLimit", "The document revision cannot advance."))
		return
	}
	p.service(applyMethod, map[string]any{
		"documentId": doc.ID, "expectedVersion": doc.Version, "expectedSelectionVersion": job.editor.selectionVersion,
		"label": label, "edits": edits,
	})
}

func main() {
	if len(os.Args) != 1 {
		if len(os.Args) < 4 || len(os.Args) > 6 || os.Args[1] != "--language-server" {
			fmt.Fprintln(os.Stderr, "Expected --language-server <java|kotlin> <project-root> [<gradle-jdk> [<server-executable>]].")
			os.Exit(2)
		}
		var gradleJavaHome, serverExecutable string
		if len(os.Args) >= 5 {
			gradleJavaHome = os.Args[4]
		}
		if len(os.Args) == 6 {
			serverExecutable = os.Args[5]
		}
		if err := languageServer(os.Args[2], os.Args[3], gradleJavaHome, serverExecutable); err != nil {
			fmt.Fprintln(os.Stderr, "CED Java and Kotlin:", err)
			os.Exit(1)
		}
		return
	}
	p := peer{out: newOutput(os.Stdout), wire: draftWire}
	if err := p.run(os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, "CED Java and Kotlin:", err)
		os.Exit(1)
	}
}
