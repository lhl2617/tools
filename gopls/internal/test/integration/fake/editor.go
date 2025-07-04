// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/tools/gopls/internal/protocol"
	"golang.org/x/tools/gopls/internal/protocol/command"
	"golang.org/x/tools/gopls/internal/test/integration/fake/glob"
	"golang.org/x/tools/gopls/internal/util/bug"
	"golang.org/x/tools/gopls/internal/util/pathutil"
	"golang.org/x/tools/internal/jsonrpc2"
	"golang.org/x/tools/internal/jsonrpc2/servertest"
	"golang.org/x/tools/internal/xcontext"
)

// Editor is a fake client editor.  It keeps track of client state and can be
// used for writing LSP tests.
type Editor struct {

	// Server, client, and sandbox are concurrency safe and written only
	// at construction time, so do not require synchronization.
	Server     protocol.Server
	cancelConn func()
	serverConn jsonrpc2.Conn
	client     *Client
	sandbox    *Sandbox

	// TODO(rfindley): buffers should be keyed by protocol.DocumentURI.
	mu                       sync.Mutex
	config                   EditorConfig      // editor configuration
	buffers                  map[string]buffer // open buffers (relative path -> buffer content)
	watchPatterns            []*glob.Glob      // glob patterns to watch
	suggestionUseReplaceMode bool

	// These fields are populated by Connect.
	serverCapabilities protocol.ServerCapabilities
	semTokOpts         protocol.SemanticTokensOptions

	// Call metrics for the purpose of expectations. This is done in an ad-hoc
	// manner for now. Perhaps in the future we should do something more
	// systematic. Guarded with a separate mutex as calls may need to be accessed
	// asynchronously via callbacks into the Editor.
	callsMu sync.Mutex
	calls   CallCounts
}

// CallCounts tracks the number of protocol notifications of different types.
type CallCounts struct {
	DidOpen, DidChange, DidSave, DidChangeWatchedFiles, DidClose, DidChangeConfiguration uint64
}

// buffer holds information about an open buffer in the editor.
type buffer struct {
	version int              // monotonic version; incremented on edits
	path    string           // relative path in the workspace
	mapper  *protocol.Mapper // buffer content
	dirty   bool             // if true, content is unsaved (TODO(rfindley): rename this field)
}

func (b buffer) text() string {
	return string(b.mapper.Content)
}

// EditorConfig configures the editor's LSP session. This is similar to
// golang.UserOptions, but we use a separate type here so that we expose only
// that configuration which we support.
//
// The zero value for EditorConfig is the default configuration.
type EditorConfig struct {
	// ClientName sets the clientInfo.name for the LSP session (in the initialize request).
	//
	// Since this can only be set during initialization, changing this field via
	// Editor.ChangeConfiguration has no effect.
	//
	// If empty, "fake.Editor" is used.
	ClientName string

	// Env holds environment variables to apply on top of the default editor
	// environment. When applying these variables, the special string
	// $SANDBOX_WORKDIR is replaced by the absolute path to the sandbox working
	// directory.
	Env map[string]string

	// WorkspaceFolders is the workspace folders to configure on the LSP server.
	// Each workspace folder is a file path relative to the sandbox workdir, or
	// a uri (used when testing behavior with virtual file system or non-'file'
	// scheme document uris).
	//
	// As special cases, if WorkspaceFolders is nil the editor defaults to
	// configuring a single workspace folder corresponding to the workdir root.
	// To explicitly send no workspace folders, use an empty (non-nil) slice.
	WorkspaceFolders []string

	// NoDefaultWorkspaceFiles is used to specify whether the fake editor
	// should give a default workspace folder when WorkspaceFolders is nil.
	// When it's true, the editor will pass original WorkspaceFolders as is to the LSP server.
	NoDefaultWorkspaceFiles bool

	// RelRootPath is the root path which will be converted to rootUri to configure on the LSP server.
	RelRootPath string

	// Whether to edit files with windows line endings.
	WindowsLineEndings bool

	// Map of language ID -> regexp to match, used to set the file type of new
	// buffers. Applied as an overlay on top of the following defaults:
	//  "go"     -> ".*\.go"
	//  "go.mod" -> "go\.mod"
	//  "go.sum" -> "go\.sum"
	//  "gotmpl" -> ".*tmpl"
	//  "go.s"   -> ".*\.s"
	FileAssociations map[protocol.LanguageKind]string

	// Settings holds user-provided configuration for the LSP server.
	Settings map[string]any

	// FolderSettings holds user-provided per-folder configuration, if any.
	//
	// It maps each folder (as a relative path to the sandbox workdir) to its
	// configuration mapping (like Settings).
	FolderSettings map[string]map[string]any

	// CapabilitiesJSON holds JSON client capabilities to overlay over the
	// editor's default client capabilities.
	//
	// Specifically, this JSON string will be unmarshalled into the editor's
	// client capabilities struct, before sending to the server.
	CapabilitiesJSON []byte

	// If non-nil, MessageResponder is used to respond to ShowMessageRequest
	// messages.
	MessageResponder func(params *protocol.ShowMessageRequestParams) (*protocol.MessageActionItem, error)

	// MaxMessageDelay is used for fuzzing message delivery to reproduce test
	// flakes.
	MaxMessageDelay time.Duration
}

// NewEditor creates a new Editor.
func NewEditor(sandbox *Sandbox, config EditorConfig) *Editor {
	return &Editor{
		buffers: make(map[string]buffer),
		sandbox: sandbox,
		config:  config,
	}
}

// Connect configures the editor to communicate with an LSP server on conn. It
// is not concurrency safe, and should be called at most once, before using the
// editor.
//
// It returns the editor, so that it may be called as follows:
//
//	editor, err := NewEditor(s).Connect(ctx, conn, hooks)
func (e *Editor) Connect(ctx context.Context, connector servertest.Connector, hooks ClientHooks) (*Editor, error) {
	bgCtx, cancelConn := context.WithCancel(xcontext.Detach(ctx))
	conn := connector.Connect(bgCtx)
	e.cancelConn = cancelConn

	e.serverConn = conn
	e.Server = protocol.ServerDispatcher(conn)
	e.client = &Client{editor: e, hooks: hooks}
	handler := protocol.ClientHandler(e.client, jsonrpc2.MethodNotFound)
	if e.config.MaxMessageDelay > 0 {
		handler = DelayedHandler(e.config.MaxMessageDelay, handler)
	}
	conn.Go(bgCtx, protocol.Handlers(handler))

	if err := e.initialize(ctx); err != nil {
		return nil, err
	}
	e.sandbox.Workdir.AddWatcher(e.onFileChanges)
	return e, nil
}

// DelayedHandler waits [0, maxDelay) before handling each message.
func DelayedHandler(maxDelay time.Duration, handler jsonrpc2.Handler) jsonrpc2.Handler {
	return func(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
		delay := time.Duration(rand.Int64N(int64(maxDelay)))
		select {
		case <-ctx.Done():
		case <-time.After(delay):
		}
		return handler(ctx, reply, req)
	}
}

func (e *Editor) Stats() CallCounts {
	e.callsMu.Lock()
	defer e.callsMu.Unlock()
	return e.calls
}

// Shutdown issues the 'shutdown' LSP notification.
func (e *Editor) Shutdown(ctx context.Context) error {
	if e.Server != nil {
		if err := e.Server.Shutdown(ctx); err != nil {
			return fmt.Errorf("Shutdown: %w", err)
		}
	}
	return nil
}

// Exit issues the 'exit' LSP notification.
func (e *Editor) Exit(ctx context.Context) error {
	if e.Server != nil {
		// Not all LSP clients issue the exit RPC, but we do so here to ensure that
		// we gracefully handle it on multi-session servers.
		if err := e.Server.Exit(ctx); err != nil {
			return fmt.Errorf("Exit: %w", err)
		}
	}
	return nil
}

// Close disconnects the LSP client session.
// TODO(rfindley): rename to 'Disconnect'.
func (e *Editor) Close(ctx context.Context) error {
	if err := e.Shutdown(ctx); err != nil {
		return err
	}
	if err := e.Exit(ctx); err != nil {
		return err
	}
	defer func() {
		e.cancelConn()
	}()

	// called close on the editor should result in the connection closing
	select {
	case <-e.serverConn.Done():
		// connection closed itself
		return nil
	case <-ctx.Done():
		return fmt.Errorf("connection not closed: %w", ctx.Err())
	}
}

// Client returns the LSP client for this editor.
func (e *Editor) Client() *Client {
	return e.client
}

// makeSettings builds the settings map for use in LSP settings RPCs.
func makeSettings(sandbox *Sandbox, config EditorConfig, scopeURI *protocol.URI) map[string]any {
	env := make(map[string]string)
	maps.Copy(env, sandbox.GoEnv())
	maps.Copy(env, config.Env)
	for k, v := range env {
		v = strings.ReplaceAll(v, "$SANDBOX_WORKDIR", sandbox.Workdir.RootURI().Path())
		env[k] = v
	}

	settings := map[string]any{
		"env": env,

		// Use verbose progress reporting so that integration tests can assert on
		// asynchronous operations being completed (such as diagnosing a snapshot).
		"verboseWorkDoneProgress": true,

		// Set an unlimited completion budget, so that tests don't flake because
		// completions are too slow.
		"completionBudget": "0s",
	}

	for k, v := range config.Settings {
		if k == "env" {
			panic("must not provide env via the EditorConfig.Settings field: use the EditorConfig.Env field instead")
		}
		settings[k] = v
	}

	// If the server is requesting configuration for a specific scope, apply
	// settings for the nearest folder that has customized settings, if any.
	if scopeURI != nil {
		var (
			scopePath       = protocol.DocumentURI(*scopeURI).Path()
			closestDir      string         // longest dir with settings containing the scope, if any
			closestSettings map[string]any // settings for that dir, if any
		)
		for relPath, settings := range config.FolderSettings {
			dir := sandbox.Workdir.AbsPath(relPath)
			if strings.HasPrefix(scopePath+string(filepath.Separator), dir+string(filepath.Separator)) && len(dir) > len(closestDir) {
				closestDir = dir
				closestSettings = settings
			}
		}
		if closestSettings != nil {
			maps.Copy(settings, closestSettings)
		}
	}

	return settings
}

func (e *Editor) initialize(ctx context.Context) error {
	config := e.Config()

	clientName := config.ClientName
	if clientName == "" {
		clientName = "fake.Editor"
	}

	params := &protocol.ParamInitialize{}
	params.ClientInfo = &protocol.ClientInfo{
		Name:    clientName,
		Version: "v1.0.0",
	}
	params.InitializationOptions = makeSettings(e.sandbox, config, nil)

	params.WorkspaceFolders = makeWorkspaceFolders(e.sandbox, config.WorkspaceFolders, config.NoDefaultWorkspaceFiles)
	params.RootURI = protocol.URIFromPath(config.RelRootPath)
	if !uriRE.MatchString(config.RelRootPath) { // relative file path
		params.RootURI = e.sandbox.Workdir.URI(config.RelRootPath)
	}

	capabilities, err := clientCapabilities(config)
	if err != nil {
		return fmt.Errorf("unmarshalling EditorConfig.CapabilitiesJSON: %v", err)
	}
	params.Capabilities = capabilities

	trace := protocol.TraceValue("messages")
	params.Trace = &trace
	// TODO: support workspace folders.
	if e.Server != nil {
		resp, err := e.Server.Initialize(ctx, params)
		if err != nil {
			return fmt.Errorf("initialize: %w", err)
		}
		semTokOpts, err := marshalUnmarshal[protocol.SemanticTokensOptions](resp.Capabilities.SemanticTokensProvider)
		if err != nil {
			return fmt.Errorf("unmarshalling semantic tokens options: %v", err)
		}
		e.serverCapabilities = resp.Capabilities
		e.semTokOpts = semTokOpts

		if err := e.Server.Initialized(ctx, &protocol.InitializedParams{}); err != nil {
			return fmt.Errorf("initialized: %w", err)
		}
	}
	// TODO: await initial configuration here, or expect gopls to manage that?
	return nil
}

func clientCapabilities(cfg EditorConfig) (protocol.ClientCapabilities, error) {
	var capabilities protocol.ClientCapabilities
	// Set various client capabilities that are sought by gopls.
	capabilities.Workspace.Configuration = true // support workspace/configuration
	capabilities.TextDocument.Completion.CompletionItem.TagSupport = &protocol.CompletionItemTagOptions{}
	capabilities.TextDocument.Completion.CompletionItem.TagSupport.ValueSet = []protocol.CompletionItemTag{protocol.ComplDeprecated}
	capabilities.TextDocument.Completion.CompletionItem.SnippetSupport = true
	capabilities.TextDocument.Completion.CompletionItem.InsertReplaceSupport = true
	capabilities.TextDocument.SemanticTokens.Requests.Full = &protocol.Or_ClientSemanticTokensRequestOptions_full{Value: true}
	capabilities.Window.WorkDoneProgress = true                                                // support window/workDoneProgress
	capabilities.Window.ShowDocument = &protocol.ShowDocumentClientCapabilities{Support: true} // support window/showDocument
	capabilities.TextDocument.SemanticTokens.TokenTypes = []string{
		"namespace", "type", "class", "enum", "interface",
		"struct", "typeParameter", "parameter", "variable", "property", "enumMember",
		"event", "function", "method", "macro", "keyword", "modifier", "comment",
		"string", "number", "regexp", "operator",
		// Additional types supported by this client:
		"label",
	}
	capabilities.TextDocument.SemanticTokens.TokenModifiers = []string{
		"declaration", "definition", "readonly", "static",
		"deprecated", "abstract", "async", "modification", "documentation", "defaultLibrary",
		// Additional modifiers supported by this client:
		"interface", "struct", "signature", "pointer", "array", "map", "slice", "chan", "string", "number", "bool", "invalid",
	}
	// Request that the server provide its complete list of code action kinds.
	capabilities.TextDocument.CodeAction = protocol.CodeActionClientCapabilities{
		DataSupport: true,
		ResolveSupport: &protocol.ClientCodeActionResolveOptions{
			Properties: []string{"edit"},
		},
		CodeActionLiteralSupport: protocol.ClientCodeActionLiteralOptions{
			CodeActionKind: protocol.ClientCodeActionKindOptions{
				ValueSet: []protocol.CodeActionKind{protocol.Empty}, // => all
			},
		},
	}
	// The LSP tests have historically enabled this flag,
	// but really we should test both ways for older editors.
	capabilities.TextDocument.DocumentSymbol.HierarchicalDocumentSymbolSupport = true
	// Glob pattern watching is enabled.
	capabilities.Workspace.DidChangeWatchedFiles.DynamicRegistration = true
	// "rename" operations are used for package renaming.
	//
	// TODO(rfindley): add support for other resource operations (create, delete, ...)
	capabilities.Workspace.WorkspaceEdit = &protocol.WorkspaceEditClientCapabilities{
		ResourceOperations: []protocol.ResourceOperationKind{
			"rename",
		},
	}

	// Apply capabilities overlay.
	if cfg.CapabilitiesJSON != nil {
		if err := json.Unmarshal(cfg.CapabilitiesJSON, &capabilities); err != nil {
			return protocol.ClientCapabilities{}, fmt.Errorf("unmarshalling EditorConfig.CapabilitiesJSON: %v", err)
		}
	}
	return capabilities, nil
}

// Returns the connected LSP server's capabilities.
// Only populated after a call to [Editor.Connect].
func (e *Editor) ServerCapabilities() protocol.ServerCapabilities {
	return e.serverCapabilities
}

// marshalUnmarshal is a helper to json Marshal and then Unmarshal as a
// different type. Used to work around cases where our protocol types are not
// specific.
func marshalUnmarshal[T any](v any) (T, error) {
	var t T
	data, err := json.Marshal(v)
	if err != nil {
		return t, err
	}
	err = json.Unmarshal(data, &t)
	return t, err
}

// HasCommand reports whether the connected server supports the command with the given ID.
func (e *Editor) HasCommand(cmd command.Command) bool {
	return slices.Contains(e.serverCapabilities.ExecuteCommandProvider.Commands, cmd.String())
}

// Examples: https://www.iana.org/assignments/uri-schemes/uri-schemes.xhtml
var uriRE = regexp.MustCompile(`^[a-z][a-z0-9+\-.]*://\S+`)

// makeWorkspaceFolders creates a slice of workspace folders to use for
// this editing session, based on the editor configuration.
func makeWorkspaceFolders(sandbox *Sandbox, paths []string, useEmpty bool) (folders []protocol.WorkspaceFolder) {
	if len(paths) == 0 {
		if useEmpty {
			return nil
		}
		paths = []string{string(sandbox.Workdir.RelativeTo)}
	}

	for _, path := range paths {
		uri := path
		if !uriRE.MatchString(path) { // relative file path
			uri = string(sandbox.Workdir.URI(path))
		}
		folders = append(folders, protocol.WorkspaceFolder{
			URI:  uri,
			Name: filepath.Base(uri),
		})
	}

	return folders
}

// onFileChanges is registered to be called by the Workdir on any writes that
// go through the Workdir API. It is called synchronously by the Workdir.
func (e *Editor) onFileChanges(ctx context.Context, evts []protocol.FileEvent) {
	if e.Server == nil {
		return
	}

	// e may be locked when onFileChanges is called, but it is important that we
	// synchronously increment this counter so that we can subsequently assert on
	// the number of expected DidChangeWatchedFiles calls.
	e.callsMu.Lock()
	e.calls.DidChangeWatchedFiles++
	e.callsMu.Unlock()

	// Since e may be locked, we must run this mutation asynchronously.
	go func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		for _, evt := range evts {
			// Always send an on-disk change, even for events that seem useless
			// because they're shadowed by an open buffer.
			path := e.sandbox.Workdir.URIToPath(evt.URI)
			if buf, ok := e.buffers[path]; ok {
				// Following VS Code, don't honor deletions or changes to dirty buffers.
				if buf.dirty || evt.Type == protocol.Deleted {
					continue
				}

				content, err := e.sandbox.Workdir.ReadFile(path)
				if err != nil {
					continue // A race with some other operation.
				}
				// No need to update if the buffer content hasn't changed.
				if string(content) == buf.text() {
					continue
				}
				// During shutdown, this call will fail. Ignore the error.
				_ = e.setBufferContentLocked(ctx, path, false, content, nil)
			}
		}
		var matchedEvts []protocol.FileEvent
		for _, evt := range evts {
			filename := filepath.ToSlash(evt.URI.Path())
			for _, g := range e.watchPatterns {
				if g.Match(filename) {
					matchedEvts = append(matchedEvts, evt)
					break
				}
			}
		}

		// TODO(rfindley): don't send notifications while locked.
		e.Server.DidChangeWatchedFiles(ctx, &protocol.DidChangeWatchedFilesParams{
			Changes: matchedEvts,
		})
	}()
}

// OpenFile creates a buffer for the given workdir-relative file.
//
// If the file is already open, it is a no-op.
func (e *Editor) OpenFile(ctx context.Context, path string) error {
	if e.HasBuffer(path) {
		return nil
	}
	content, err := e.sandbox.Workdir.ReadFile(path)
	if err != nil {
		return err
	}
	if e.Config().WindowsLineEndings {
		content = toWindowsLineEndings(content)
	}
	return e.createBuffer(ctx, path, false, content)
}

// toWindowsLineEndings checks whether content has windows line endings.
//
// If so, it returns content unmodified. If not, it returns a new byte slice modified to use CRLF line endings.
func toWindowsLineEndings(content []byte) []byte {
	abnormal := false
	for i, b := range content {
		if b == '\n' && (i == 0 || content[i-1] != '\r') {
			abnormal = true
			break
		}
	}
	if !abnormal {
		return content
	}
	var buf bytes.Buffer
	for i, b := range content {
		if b == '\n' && (i == 0 || content[i-1] != '\r') {
			buf.WriteByte('\r')
		}
		buf.WriteByte(b)
	}
	return buf.Bytes()
}

// CreateBuffer creates a new unsaved buffer corresponding to the workdir path,
// containing the given textual content.
func (e *Editor) CreateBuffer(ctx context.Context, path, content string) error {
	return e.createBuffer(ctx, path, true, []byte(content))
}

func (e *Editor) createBuffer(ctx context.Context, path string, dirty bool, content []byte) error {
	e.mu.Lock()

	if _, ok := e.buffers[path]; ok {
		e.mu.Unlock()
		return fmt.Errorf("buffer %q already exists", path)
	}

	uri := e.sandbox.Workdir.URI(path)
	buf := buffer{
		version: 1,
		path:    path,
		mapper:  protocol.NewMapper(uri, content),
		dirty:   dirty,
	}
	e.buffers[path] = buf

	item := e.textDocumentItem(buf)
	e.mu.Unlock()

	return e.sendDidOpen(ctx, item)
}

// textDocumentItem builds a protocol.TextDocumentItem for the given buffer.
//
// Precondition: e.mu must be held.
func (e *Editor) textDocumentItem(buf buffer) protocol.TextDocumentItem {
	return protocol.TextDocumentItem{
		URI:        e.sandbox.Workdir.URI(buf.path),
		LanguageID: languageID(buf.path, e.config.FileAssociations),
		Version:    int32(buf.version),
		Text:       buf.text(),
	}
}

func (e *Editor) sendDidOpen(ctx context.Context, item protocol.TextDocumentItem) error {
	if e.Server != nil {
		if err := e.Server.DidOpen(ctx, &protocol.DidOpenTextDocumentParams{
			TextDocument: item,
		}); err != nil {
			return fmt.Errorf("DidOpen: %w", err)
		}
		e.callsMu.Lock()
		e.calls.DidOpen++
		e.callsMu.Unlock()
	}
	return nil
}

var defaultFileAssociations = map[protocol.LanguageKind]*regexp.Regexp{
	"go":      regexp.MustCompile(`^.*\.go$`), // '$' is important: don't match .gotmpl!
	"go.mod":  regexp.MustCompile(`^go\.mod$`),
	"go.sum":  regexp.MustCompile(`^go(\.work)?\.sum$`),
	"go.work": regexp.MustCompile(`^go\.work$`),
	"gotmpl":  regexp.MustCompile(`^.*tmpl$`),
	"go.s":    regexp.MustCompile(`\.s$`),
}

// languageID returns the language identifier for the path p given the user
// configured fileAssociations.
func languageID(p string, fileAssociations map[protocol.LanguageKind]string) protocol.LanguageKind {
	base := path.Base(p)
	for lang, re := range fileAssociations {
		re := regexp.MustCompile(re)
		if re.MatchString(base) {
			return lang
		}
	}
	for lang, re := range defaultFileAssociations {
		if re.MatchString(base) {
			return lang
		}
	}
	return ""
}

// CloseBuffer removes the current buffer (regardless of whether it is saved).
// CloseBuffer returns an error if the buffer is not open.
func (e *Editor) CloseBuffer(ctx context.Context, path string) error {
	e.mu.Lock()
	_, ok := e.buffers[path]
	if !ok {
		e.mu.Unlock()
		return ErrUnknownBuffer
	}
	delete(e.buffers, path)
	e.mu.Unlock()

	return e.sendDidClose(ctx, e.TextDocumentIdentifier(path))
}

func (e *Editor) sendDidClose(ctx context.Context, doc protocol.TextDocumentIdentifier) error {
	if e.Server != nil {
		if err := e.Server.DidClose(ctx, &protocol.DidCloseTextDocumentParams{
			TextDocument: doc,
		}); err != nil {
			return fmt.Errorf("DidClose: %w", err)
		}
		e.callsMu.Lock()
		e.calls.DidClose++
		e.callsMu.Unlock()
	}
	return nil
}

func (e *Editor) DocumentURI(path string) protocol.DocumentURI {
	return e.sandbox.Workdir.URI(path)
}

func (e *Editor) TextDocumentIdentifier(path string) protocol.TextDocumentIdentifier {
	return protocol.TextDocumentIdentifier{
		URI: e.DocumentURI(path),
	}
}

// SaveBuffer writes the content of the buffer specified by the given path to
// the filesystem.
func (e *Editor) SaveBuffer(ctx context.Context, path string) error {
	if err := e.OrganizeImports(ctx, path); err != nil {
		return fmt.Errorf("organizing imports before save: %w", err)
	}
	if err := e.FormatBuffer(ctx, path); err != nil {
		return fmt.Errorf("formatting before save: %w", err)
	}
	return e.SaveBufferWithoutActions(ctx, path)
}

func (e *Editor) SaveBufferWithoutActions(ctx context.Context, path string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	buf, ok := e.buffers[path]
	if !ok {
		return fmt.Errorf("unknown buffer: %q", path)
	}
	content := buf.text()
	includeText := false
	syncOptions, ok := e.serverCapabilities.TextDocumentSync.(protocol.TextDocumentSyncOptions)
	if ok {
		includeText = syncOptions.Save.IncludeText
	}

	docID := e.TextDocumentIdentifier(buf.path)
	if e.Server != nil {
		if err := e.Server.WillSave(ctx, &protocol.WillSaveTextDocumentParams{
			TextDocument: docID,
			Reason:       protocol.Manual,
		}); err != nil {
			return fmt.Errorf("WillSave: %w", err)
		}
	}
	if err := e.sandbox.Workdir.WriteFile(ctx, path, content); err != nil {
		return fmt.Errorf("writing %q: %w", path, err)
	}

	buf.dirty = false
	e.buffers[path] = buf

	if e.Server != nil {
		params := &protocol.DidSaveTextDocumentParams{
			TextDocument: docID,
		}
		if includeText {
			params.Text = &content
		}
		if err := e.Server.DidSave(ctx, params); err != nil {
			return fmt.Errorf("DidSave: %w", err)
		}
		e.callsMu.Lock()
		e.calls.DidSave++
		e.callsMu.Unlock()
	}
	return nil
}

// ErrNoMatch is returned if a regexp search fails.
var (
	ErrNoMatch       = errors.New("no match")
	ErrUnknownBuffer = errors.New("unknown buffer")
)

// regexpLocation returns the location of the first occurrence of either re
// or its singular subgroup. It returns ErrNoMatch if the regexp doesn't match.
func regexpLocation(mapper *protocol.Mapper, re string) (protocol.Location, error) {
	var start, end int
	rec, err := regexp.Compile(re)
	if err != nil {
		return protocol.Location{}, err
	}
	indexes := rec.FindSubmatchIndex(mapper.Content)
	if indexes == nil {
		return protocol.Location{}, ErrNoMatch
	}
	switch len(indexes) {
	case 2:
		// no subgroups: return the range of the regexp expression
		start, end = indexes[0], indexes[1]
	case 4:
		// one subgroup: return its range
		start, end = indexes[2], indexes[3]
	default:
		return protocol.Location{}, fmt.Errorf("invalid search regexp %q: expect either 0 or 1 subgroups, got %d", re, len(indexes)/2-1)
	}
	return mapper.OffsetLocation(start, end)
}

// RegexpSearch returns the Location of the first match for re in the buffer
// bufName. For convenience, RegexpSearch supports the following two modes:
//  1. If re has no subgroups, return the position of the match for re itself.
//  2. If re has one subgroup, return the position of the first subgroup.
//
// It returns an error re is invalid, has more than one subgroup, or doesn't
// match the buffer.
func (e *Editor) RegexpSearch(bufName, re string) (protocol.Location, error) {
	e.mu.Lock()
	buf, ok := e.buffers[bufName]
	e.mu.Unlock()
	if !ok {
		return protocol.Location{}, ErrUnknownBuffer
	}
	return regexpLocation(buf.mapper, re)
}

// RegexpReplace edits the buffer corresponding to path by replacing the first
// instance of re, or its first subgroup, with the replace text. See
// RegexpSearch for more explanation of these two modes.
// It returns an error if re is invalid, has more than one subgroup, or doesn't
// match the buffer.
func (e *Editor) RegexpReplace(ctx context.Context, path, re, replace string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	buf, ok := e.buffers[path]
	if !ok {
		return ErrUnknownBuffer
	}
	loc, err := regexpLocation(buf.mapper, re)
	if err != nil {
		return err
	}
	edits := []protocol.TextEdit{{
		Range:   loc.Range,
		NewText: replace,
	}}
	patched, err := applyEdits(buf.mapper, edits, e.config.WindowsLineEndings)
	if err != nil {
		return fmt.Errorf("editing %q: %v", path, err)
	}
	return e.setBufferContentLocked(ctx, path, true, patched, edits)
}

// EditBuffer applies the given test edits to the buffer identified by path.
func (e *Editor) EditBuffer(ctx context.Context, path string, edits []protocol.TextEdit) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.editBufferLocked(ctx, path, edits)
}

func (e *Editor) SetBufferContent(ctx context.Context, path, content string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.setBufferContentLocked(ctx, path, true, []byte(content), nil)
}

// HasBuffer reports whether the file name is open in the editor.
func (e *Editor) HasBuffer(name string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.buffers[name]
	return ok
}

// BufferText returns the content of the buffer with the given name, or "" if
// the file at that path is not open. The second return value reports whether
// the file is open.
func (e *Editor) BufferText(name string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	buf, ok := e.buffers[name]
	if !ok {
		return "", false
	}
	return buf.text(), true
}

// Mapper returns the protocol.Mapper for the given buffer name, if it is open.
func (e *Editor) Mapper(name string) (*protocol.Mapper, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	buf, ok := e.buffers[name]
	if !ok {
		return nil, fmt.Errorf("no mapper for %q", name)
	}
	return buf.mapper, nil
}

// BufferVersion returns the current version of the buffer corresponding to
// name (or 0 if it is not being edited).
func (e *Editor) BufferVersion(name string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.buffers[name].version
}

func (e *Editor) editBufferLocked(ctx context.Context, path string, edits []protocol.TextEdit) error {
	buf, ok := e.buffers[path]
	if !ok {
		return fmt.Errorf("unknown buffer %q", path)
	}
	content, err := applyEdits(buf.mapper, edits, e.config.WindowsLineEndings)
	if err != nil {
		return fmt.Errorf("editing %q: %v; edits:\n%v", path, err, edits)
	}
	return e.setBufferContentLocked(ctx, path, true, content, edits)
}

func (e *Editor) setBufferContentLocked(ctx context.Context, path string, dirty bool, content []byte, fromEdits []protocol.TextEdit) error {
	buf, ok := e.buffers[path]
	if !ok {
		return fmt.Errorf("unknown buffer %q", path)
	}
	buf.mapper = protocol.NewMapper(buf.mapper.URI, content)
	buf.version++
	buf.dirty = dirty
	e.buffers[path] = buf

	// A simple heuristic: if there is only one edit, send it incrementally.
	// Otherwise, send the entire content.
	var evt protocol.TextDocumentContentChangeEvent
	if len(fromEdits) == 1 {
		evt.Range = &fromEdits[0].Range
		evt.Text = fromEdits[0].NewText
	} else {
		evt.Text = buf.text()
	}
	params := &protocol.DidChangeTextDocumentParams{
		TextDocument: protocol.VersionedTextDocumentIdentifier{
			Version:                int32(buf.version),
			TextDocumentIdentifier: e.TextDocumentIdentifier(buf.path),
		},
		ContentChanges: []protocol.TextDocumentContentChangeEvent{evt},
	}
	if e.Server != nil {
		if err := e.Server.DidChange(ctx, params); err != nil {
			return fmt.Errorf("DidChange: %w", err)
		}
		e.callsMu.Lock()
		e.calls.DidChange++
		e.callsMu.Unlock()
	}
	return nil
}

// Definitions returns the definitions of the symbol at the given
// location in an open buffer.
func (e *Editor) Definitions(ctx context.Context, loc protocol.Location) ([]protocol.Location, error) {
	if err := e.checkBufferLocation(loc); err != nil {
		return nil, err
	}
	params := &protocol.DefinitionParams{}
	params.TextDocument.URI = loc.URI
	params.Position = loc.Range.Start

	return e.Server.Definition(ctx, params)
}

// TypeDefinitions returns the type definitions of the symbol at the
// given location in an open buffer.
func (e *Editor) TypeDefinitions(ctx context.Context, loc protocol.Location) ([]protocol.Location, error) {
	if err := e.checkBufferLocation(loc); err != nil {
		return nil, err
	}
	params := &protocol.TypeDefinitionParams{}
	params.TextDocument.URI = loc.URI
	params.Position = loc.Range.Start

	return e.Server.TypeDefinition(ctx, params)
}

// Symbol performs a workspace symbol search using query
func (e *Editor) Symbol(ctx context.Context, query string) ([]protocol.SymbolInformation, error) {
	params := &protocol.WorkspaceSymbolParams{Query: query}
	return e.Server.Symbol(ctx, params)
}

// OrganizeImports requests and performs the source.organizeImports codeAction.
func (e *Editor) OrganizeImports(ctx context.Context, path string) error {
	loc := e.sandbox.Workdir.EntireFile(path)
	_, err := e.applyCodeActions(ctx, loc, nil, protocol.SourceOrganizeImports)
	return err
}

// RefactorRewrite requests and performs the source.refactorRewrite codeAction.
func (e *Editor) RefactorRewrite(ctx context.Context, loc protocol.Location) error {
	applied, err := e.applyCodeActions(ctx, loc, nil, protocol.RefactorRewrite)
	if err != nil {
		return err
	}
	if applied == 0 {
		return fmt.Errorf("no refactorings were applied")
	}
	return nil
}

// ApplyQuickFixes requests and performs the quickfix codeAction.
func (e *Editor) ApplyQuickFixes(ctx context.Context, loc protocol.Location, diagnostics []protocol.Diagnostic) error {
	applied, err := e.applyCodeActions(ctx, loc, diagnostics, protocol.SourceFixAll, protocol.QuickFix)
	if applied == 0 {
		return fmt.Errorf("no quick fixes were applied")
	}
	return err
}

// ApplyCodeAction applies the given code action.
func (e *Editor) ApplyCodeAction(ctx context.Context, action protocol.CodeAction) error {
	// Resolve the code actions if necessary and supported.
	if action.Edit == nil {
		editSupport, err := e.EditResolveSupport()
		if err != nil {
			return err
		}
		if editSupport {
			ca, err := e.Server.ResolveCodeAction(ctx, &action)
			if err != nil {
				return err
			}
			action.Edit = ca.Edit
		}
	}

	if action.Edit != nil {
		for _, change := range action.Edit.DocumentChanges {
			if change.TextDocumentEdit != nil {
				path := e.sandbox.Workdir.URIToPath(change.TextDocumentEdit.TextDocument.URI)
				if int32(e.buffers[path].version) != change.TextDocumentEdit.TextDocument.Version {
					// Skip edits for old versions.
					continue
				}
				if err := e.EditBuffer(ctx, path, protocol.AsTextEdits(change.TextDocumentEdit.Edits)); err != nil {
					return fmt.Errorf("editing buffer %q: %w", path, err)
				}
			}
		}
	}
	// Execute any commands. The specification says that commands are
	// executed after edits are applied.
	if action.Command != nil {
		if err := e.ExecuteCommand(ctx, &protocol.ExecuteCommandParams{
			Command:   action.Command.Command,
			Arguments: action.Command.Arguments,
		}, nil); err != nil {
			return err
		}
	}
	// Some commands may edit files on disk.
	return e.sandbox.Workdir.CheckForFileChanges(ctx)
}

func (e *Editor) Diagnostics(ctx context.Context, path string) ([]protocol.Diagnostic, error) {
	if e.Server == nil {
		return nil, errors.New("not connected")
	}
	e.mu.Lock()
	capabilities := e.serverCapabilities.DiagnosticProvider
	e.mu.Unlock()

	if capabilities == nil {
		return nil, errors.New("server does not support pull diagnostics")
	}
	switch capabilities.Value.(type) {
	case nil:
		return nil, errors.New("server does not support pull diagnostics")
	case protocol.DiagnosticOptions:
	case protocol.DiagnosticRegistrationOptions:
		// We could optionally check TextDocumentRegistrationOptions here to
		// see if any filters apply to path.
	default:
		panic(fmt.Sprintf("unknown DiagnosticsProvider type %T", capabilities.Value))
	}

	params := &protocol.DocumentDiagnosticParams{
		TextDocument: e.TextDocumentIdentifier(path),
	}
	result, err := e.Server.Diagnostic(ctx, params)
	if err != nil {
		return nil, err
	}
	report, ok := result.Value.(protocol.RelatedFullDocumentDiagnosticReport)
	if !ok {
		return nil, fmt.Errorf("unexpected diagnostics report type %T", result)
	}
	return report.Items, nil
}

// GetQuickFixes returns the available quick fix code actions.
func (e *Editor) GetQuickFixes(ctx context.Context, loc protocol.Location, diagnostics []protocol.Diagnostic) ([]protocol.CodeAction, error) {
	return e.CodeActions(ctx, loc, diagnostics, protocol.QuickFix, protocol.SourceFixAll)
}

func (e *Editor) applyCodeActions(ctx context.Context, loc protocol.Location, diagnostics []protocol.Diagnostic, only ...protocol.CodeActionKind) (int, error) {
	actions, err := e.CodeActions(ctx, loc, diagnostics, only...)
	if err != nil {
		return 0, err
	}
	applied := 0
	for _, action := range actions {
		if action.Title == "" {
			return 0, fmt.Errorf("empty title for code action")
		}
		applied++
		if err := e.ApplyCodeAction(ctx, action); err != nil {
			return 0, err
		}
	}
	return applied, nil
}

// TODO(rfindley): add missing documentation to exported methods here.

func (e *Editor) CodeActions(ctx context.Context, loc protocol.Location, diagnostics []protocol.Diagnostic, only ...protocol.CodeActionKind) ([]protocol.CodeAction, error) {
	if e.Server == nil {
		return nil, nil
	}
	params := &protocol.CodeActionParams{}
	params.TextDocument.URI = loc.URI
	params.Context.Only = only
	params.Range = loc.Range // may be zero => whole file
	if diagnostics != nil {
		params.Context.Diagnostics = diagnostics
	}
	return e.Server.CodeAction(ctx, params)
}

func (e *Editor) ExecuteCodeLensCommand(ctx context.Context, path string, cmd command.Command, result any) error {
	lenses, err := e.CodeLens(ctx, path)
	if err != nil {
		return err
	}
	var lens protocol.CodeLens
	var found bool
	for _, l := range lenses {
		if l.Command.Command == cmd.String() {
			lens = l
			found = true
		}
	}
	if !found {
		return fmt.Errorf("found no command with the ID %s", cmd)
	}
	return e.ExecuteCommand(ctx, &protocol.ExecuteCommandParams{
		Command:   lens.Command.Command,
		Arguments: lens.Command.Arguments,
	}, result)
}

// ExecuteCommand makes a workspace/executeCommand request to the connected LSP
// server, if any.
//
// Result contains a pointer to a variable to be populated by json.Unmarshal.
func (e *Editor) ExecuteCommand(ctx context.Context, params *protocol.ExecuteCommandParams, result any) error {
	if e.Server == nil {
		return nil
	}
	var match bool
	if e.serverCapabilities.ExecuteCommandProvider != nil {
		// Ensure that this command was actually listed as a supported command.
		if slices.Contains(e.serverCapabilities.ExecuteCommandProvider.Commands, params.Command) {
			match = true
		}
	}
	if !match {
		return fmt.Errorf("unsupported command %q", params.Command)
	}
	response, err := e.Server.ExecuteCommand(ctx, params)
	if err != nil {
		return err
	}
	// Some commands use the go command, which writes directly to disk.
	// For convenience, check for those changes.
	if err := e.sandbox.Workdir.CheckForFileChanges(ctx); err != nil {
		return fmt.Errorf("checking for file changes: %v", err)
	}
	if result != nil {
		// ExecuteCommand already unmarshalled the response without knowing
		// its schema, using the generic map[string]any representation.
		// Encode and decode again, this time into a typed variable.
		//
		// This could be improved by generating a jsonrpc2 command client from the
		// command.Interface, but that should only be done if we're consolidating
		// this part of the tsprotocol generation.
		//
		// TODO(rfindley): we could also improve this by having ExecuteCommand return
		// a json.RawMessage, similar to what we do with arguments.
		data, err := json.Marshal(response)
		if err != nil {
			return bug.Errorf("marshalling response: %v", err)
		}
		if err := json.Unmarshal(data, result); err != nil {
			return fmt.Errorf("unmarshalling response: %v", err)
		}
	}
	return nil
}

// FormatBuffer gofmts a Go file.
func (e *Editor) FormatBuffer(ctx context.Context, path string) error {
	if e.Server == nil {
		return nil
	}
	e.mu.Lock()
	version := e.buffers[path].version
	e.mu.Unlock()
	params := &protocol.DocumentFormattingParams{}
	params.TextDocument.URI = e.sandbox.Workdir.URI(path)
	edits, err := e.Server.Formatting(ctx, params)
	if err != nil {
		return fmt.Errorf("textDocument/formatting: %w", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if versionAfter := e.buffers[path].version; versionAfter != version {
		return fmt.Errorf("before receipt of formatting edits, buffer version changed from %d to %d", version, versionAfter)
	}
	if len(edits) == 0 {
		return nil
	}
	return e.editBufferLocked(ctx, path, edits)
}

func (e *Editor) checkBufferLocation(loc protocol.Location) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	path := e.sandbox.Workdir.URIToPath(loc.URI)
	buf, ok := e.buffers[path]
	if !ok {
		return fmt.Errorf("buffer %q is not open", path)
	}

	_, _, err := buf.mapper.RangeOffsets(loc.Range)
	return err
}

// RunGenerate runs `go generate` non-recursively in the workdir-relative dir
// path. It does not report any resulting file changes as a watched file
// change, so must be followed by a call to Workdir.CheckForFileChanges once
// the generate command has completed.
// TODO(rFindley): this shouldn't be necessary anymore. Delete it.
func (e *Editor) RunGenerate(ctx context.Context, dir string) error {
	if e.Server == nil {
		return nil
	}
	absDir := e.sandbox.Workdir.AbsPath(dir)
	cmd := command.NewGenerateCommand("", command.GenerateArgs{
		Dir:       protocol.URIFromPath(absDir),
		Recursive: false,
	})
	params := &protocol.ExecuteCommandParams{
		Command:   cmd.Command,
		Arguments: cmd.Arguments,
	}
	if err := e.ExecuteCommand(ctx, params, nil); err != nil {
		return fmt.Errorf("running generate: %v", err)
	}
	// Unfortunately we can't simply poll the workdir for file changes here,
	// because server-side command may not have completed. In integration tests, we can
	// Await this state change, but here we must delegate that responsibility to
	// the caller.
	return nil
}

// CodeLens executes a codelens request on the server.
func (e *Editor) CodeLens(ctx context.Context, path string) ([]protocol.CodeLens, error) {
	if e.Server == nil {
		return nil, nil
	}
	e.mu.Lock()
	_, ok := e.buffers[path]
	e.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("buffer %q is not open", path)
	}
	params := &protocol.CodeLensParams{
		TextDocument: e.TextDocumentIdentifier(path),
	}
	lens, err := e.Server.CodeLens(ctx, params)
	if err != nil {
		return nil, err
	}
	return lens, nil
}

// Completion executes a completion request on the server.
func (e *Editor) Completion(ctx context.Context, loc protocol.Location) (*protocol.CompletionList, error) {
	if e.Server == nil {
		return nil, nil
	}
	path := e.sandbox.Workdir.URIToPath(loc.URI)
	e.mu.Lock()
	_, ok := e.buffers[path]
	e.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("buffer %q is not open", path)
	}
	params := &protocol.CompletionParams{
		TextDocumentPositionParams: protocol.LocationTextDocumentPositionParams(loc),
	}
	completions, err := e.Server.Completion(ctx, params)
	if err != nil {
		return nil, err
	}
	return completions, nil
}

func (e *Editor) DidCreateFiles(ctx context.Context, files ...protocol.DocumentURI) error {
	if e.Server == nil {
		return nil
	}
	params := &protocol.CreateFilesParams{}
	for _, file := range files {
		params.Files = append(params.Files, protocol.FileCreate{
			URI: string(file),
		})
	}
	return e.Server.DidCreateFiles(ctx, params)
}

func (e *Editor) SetSuggestionInsertReplaceMode(_ context.Context, useReplaceMode bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.suggestionUseReplaceMode = useReplaceMode
}

// AcceptCompletion accepts a completion for the given item
// at the given position based on the editor's suggestion insert mode.
// The server provides separate insert/replace ranges only if the
// Editor declares `InsertReplaceSupport` capability during initialization.
// Otherwise, it returns a single range and the insert/replace mode is ignored.
func (e *Editor) AcceptCompletion(ctx context.Context, loc protocol.Location, item protocol.CompletionItem) error {
	if e.Server == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	path := e.sandbox.Workdir.URIToPath(loc.URI)
	_, ok := e.buffers[path]
	if !ok {
		return fmt.Errorf("buffer %q is not open", path)
	}
	edit, err := protocol.SelectCompletionTextEdit(item, e.suggestionUseReplaceMode)
	if err != nil {
		return err
	}
	return e.editBufferLocked(ctx, path, append([]protocol.TextEdit{
		edit,
	}, item.AdditionalTextEdits...))
}

// Symbols executes a workspace/symbols request on the server.
func (e *Editor) Symbols(ctx context.Context, sym string) ([]protocol.SymbolInformation, error) {
	if e.Server == nil {
		return nil, nil
	}
	params := &protocol.WorkspaceSymbolParams{Query: sym}
	ans, err := e.Server.Symbol(ctx, params)
	return ans, err
}

// CodeLens executes a codelens request on the server.
func (e *Editor) InlayHint(ctx context.Context, path string) ([]protocol.InlayHint, error) {
	if e.Server == nil {
		return nil, nil
	}
	e.mu.Lock()
	_, ok := e.buffers[path]
	e.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("buffer %q is not open", path)
	}
	params := &protocol.InlayHintParams{
		TextDocument: e.TextDocumentIdentifier(path),
	}
	hints, err := e.Server.InlayHint(ctx, params)
	if err != nil {
		return nil, err
	}
	return hints, nil
}

// References returns references to the object at loc, as returned by
// the connected LSP server. If no server is connected, it returns (nil, nil).
func (e *Editor) References(ctx context.Context, loc protocol.Location) ([]protocol.Location, error) {
	if e.Server == nil {
		return nil, nil
	}
	path := e.sandbox.Workdir.URIToPath(loc.URI)
	e.mu.Lock()
	_, ok := e.buffers[path]
	e.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("buffer %q is not open", path)
	}
	params := &protocol.ReferenceParams{
		TextDocumentPositionParams: protocol.LocationTextDocumentPositionParams(loc),
		Context: protocol.ReferenceContext{
			IncludeDeclaration: true,
		},
	}
	locations, err := e.Server.References(ctx, params)
	if err != nil {
		return nil, err
	}
	return locations, nil
}

// Rename performs a rename of the object at loc to newName, using the
// connected LSP server. If no server is connected, it returns nil.
func (e *Editor) Rename(ctx context.Context, loc protocol.Location, newName string) error {
	if e.Server == nil {
		return nil
	}
	path := e.sandbox.Workdir.URIToPath(loc.URI)

	// Verify that PrepareRename succeeds.
	prepareParams := &protocol.PrepareRenameParams{}
	prepareParams.TextDocument = e.TextDocumentIdentifier(path)
	prepareParams.Position = loc.Range.Start
	if _, err := e.Server.PrepareRename(ctx, prepareParams); err != nil {
		return fmt.Errorf("preparing rename: %v", err)
	}

	params := &protocol.RenameParams{
		TextDocument: e.TextDocumentIdentifier(path),
		Position:     loc.Range.Start,
		NewName:      newName,
	}
	wsedit, err := e.Server.Rename(ctx, params)
	if err != nil {
		return err
	}
	return e.applyWorkspaceEdit(ctx, wsedit)
}

// Implementations returns implementations for the object at loc, as
// returned by the connected LSP server. If no server is connected, it returns
// (nil, nil).
func (e *Editor) Implementations(ctx context.Context, loc protocol.Location) ([]protocol.Location, error) {
	if e.Server == nil {
		return nil, nil
	}
	path := e.sandbox.Workdir.URIToPath(loc.URI)
	e.mu.Lock()
	_, ok := e.buffers[path]
	e.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("buffer %q is not open", path)
	}
	params := &protocol.ImplementationParams{
		TextDocumentPositionParams: protocol.LocationTextDocumentPositionParams(loc),
	}
	return e.Server.Implementation(ctx, params)
}

func (e *Editor) SignatureHelp(ctx context.Context, loc protocol.Location) (*protocol.SignatureHelp, error) {
	if e.Server == nil {
		return nil, nil
	}
	path := e.sandbox.Workdir.URIToPath(loc.URI)
	e.mu.Lock()
	_, ok := e.buffers[path]
	e.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("buffer %q is not open", path)
	}
	params := &protocol.SignatureHelpParams{
		TextDocumentPositionParams: protocol.LocationTextDocumentPositionParams(loc),
	}
	return e.Server.SignatureHelp(ctx, params)
}

func (e *Editor) RenameFile(ctx context.Context, oldPath, newPath string) error {
	closed, opened, err := e.renameBuffers(oldPath, newPath)
	if err != nil {
		return err
	}

	for _, c := range closed {
		if err := e.sendDidClose(ctx, c); err != nil {
			return err
		}
	}
	for _, o := range opened {
		if err := e.sendDidOpen(ctx, o); err != nil {
			return err
		}
	}

	// Finally, perform the renaming on disk.
	if err := e.sandbox.Workdir.RenameFile(ctx, oldPath, newPath); err != nil {
		return fmt.Errorf("renaming sandbox file: %w", err)
	}
	return nil
}

// renameBuffers renames in-memory buffers affected by the renaming of
// oldPath->newPath, returning the resulting text documents that must be closed
// and opened over the LSP.
func (e *Editor) renameBuffers(oldPath, newPath string) (closed []protocol.TextDocumentIdentifier, opened []protocol.TextDocumentItem, _ error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// In case either oldPath or newPath is absolute, convert to absolute paths
	// before checking for containment.
	oldAbs := e.sandbox.Workdir.AbsPath(oldPath)
	newAbs := e.sandbox.Workdir.AbsPath(newPath)

	// Collect buffers that are affected by the given file or directory renaming.
	buffersToRename := make(map[string]string) // old path -> new path

	for path := range e.buffers {
		abs := e.sandbox.Workdir.AbsPath(path)
		if oldAbs == abs || pathutil.InDir(oldAbs, abs) {
			rel, err := filepath.Rel(oldAbs, abs)
			if err != nil {
				return nil, nil, fmt.Errorf("filepath.Rel(%q, %q): %v", oldAbs, abs, err)
			}
			nabs := filepath.Join(newAbs, rel)
			newPath := e.sandbox.Workdir.RelPath(nabs)
			buffersToRename[path] = newPath
		}
	}

	// Update buffers, and build protocol changes.
	for old, new := range buffersToRename {
		buf := e.buffers[old]
		delete(e.buffers, old)
		buf.version = 1
		buf.path = new
		e.buffers[new] = buf

		closed = append(closed, e.TextDocumentIdentifier(old))
		opened = append(opened, e.textDocumentItem(buf))
	}

	return closed, opened, nil
}

// applyWorkspaceEdit applies the sequence of document changes in
// wsedit to the Editor.
//
// See also:
//   - changedFiles in ../../marker/marker_test.go for the
//     handler used by the marker test to intercept edits.
//   - client.applyWorkspaceEdit in ../../../cmd/cmd.go for the
//     CLI variant.
func (e *Editor) applyWorkspaceEdit(ctx context.Context, wsedit *protocol.WorkspaceEdit) error {
	uriToPath := e.sandbox.Workdir.URIToPath

	for _, change := range wsedit.DocumentChanges {
		switch {
		case change.TextDocumentEdit != nil:
			if err := e.applyTextDocumentEdit(ctx, *change.TextDocumentEdit); err != nil {
				return err
			}

		case change.RenameFile != nil:
			old := uriToPath(change.RenameFile.OldURI)
			new := uriToPath(change.RenameFile.NewURI)
			return e.RenameFile(ctx, old, new)

		case change.CreateFile != nil:
			path := uriToPath(change.CreateFile.URI)
			if err := e.CreateBuffer(ctx, path, ""); err != nil {
				return err // e.g. already exists
			}

		case change.DeleteFile != nil:
			path := uriToPath(change.CreateFile.URI)
			_ = e.CloseBuffer(ctx, path) // returns error if not open
			if err := e.sandbox.Workdir.RemoveFile(ctx, path); err != nil {
				return err // e.g. doesn't exist
			}

		default:
			return bug.Errorf("invalid DocumentChange")
		}
	}
	return nil
}

func (e *Editor) applyTextDocumentEdit(ctx context.Context, change protocol.TextDocumentEdit) error {
	path := e.sandbox.Workdir.URIToPath(change.TextDocument.URI)
	if ver := int32(e.BufferVersion(path)); ver != change.TextDocument.Version {
		return fmt.Errorf("buffer versions for %q do not match: have %d, editing %d", path, ver, change.TextDocument.Version)
	}
	if !e.HasBuffer(path) {
		err := e.OpenFile(ctx, path)
		if os.IsNotExist(err) {
			// TODO: it's unclear if this is correct. Here we create the buffer (with
			// version 1), then apply edits. Perhaps we should apply the edits before
			// sending the didOpen notification.
			err = e.CreateBuffer(ctx, path, "")
		}
		if err != nil {
			return err
		}
	}
	return e.EditBuffer(ctx, path, protocol.AsTextEdits(change.Edits))
}

// Config returns the current editor configuration.
func (e *Editor) Config() EditorConfig {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.config
}

func (e *Editor) SetConfig(cfg EditorConfig) {
	e.mu.Lock()
	e.config = cfg
	e.mu.Unlock()
}

// ChangeConfiguration sets the new editor configuration, and if applicable
// sends a didChangeConfiguration notification.
//
// An error is returned if the change notification failed to send.
func (e *Editor) ChangeConfiguration(ctx context.Context, newConfig EditorConfig) error {
	e.SetConfig(newConfig)
	if e.Server != nil {
		var params protocol.DidChangeConfigurationParams // empty: gopls ignores the Settings field
		if err := e.Server.DidChangeConfiguration(ctx, &params); err != nil {
			return err
		}
		e.callsMu.Lock()
		e.calls.DidChangeConfiguration++
		e.callsMu.Unlock()
	}
	return nil
}

// ChangeWorkspaceFolders sets the new workspace folders, and sends a
// didChangeWorkspaceFolders notification to the server.
//
// The given folders must all be unique.
func (e *Editor) ChangeWorkspaceFolders(ctx context.Context, folders []string) error {
	config := e.Config()

	// capture existing folders so that we can compute the change.
	oldFolders := makeWorkspaceFolders(e.sandbox, config.WorkspaceFolders, config.NoDefaultWorkspaceFiles)
	newFolders := makeWorkspaceFolders(e.sandbox, folders, config.NoDefaultWorkspaceFiles)
	config.WorkspaceFolders = folders
	e.SetConfig(config)

	if e.Server == nil {
		return nil
	}

	var params protocol.DidChangeWorkspaceFoldersParams

	// Keep track of old workspace folders that must be removed.
	toRemove := make(map[protocol.URI]protocol.WorkspaceFolder)
	for _, folder := range oldFolders {
		toRemove[folder.URI] = folder
	}

	// Sanity check: if we see a folder twice the algorithm below doesn't work,
	// so track seen folders to ensure that we panic in that case.
	seen := make(map[protocol.URI]protocol.WorkspaceFolder)
	for _, folder := range newFolders {
		if _, ok := seen[folder.URI]; ok {
			panic(fmt.Sprintf("folder %s seen twice", folder.URI))
		}

		// If this folder already exists, we don't want to remove it.
		// Otherwise, we need to add it.
		if _, ok := toRemove[folder.URI]; ok {
			delete(toRemove, folder.URI)
		} else {
			params.Event.Added = append(params.Event.Added, folder)
		}
	}

	for _, v := range toRemove {
		params.Event.Removed = append(params.Event.Removed, v)
	}

	return e.Server.DidChangeWorkspaceFolders(ctx, &params)
}

// CodeAction executes a codeAction request on the server.
// If loc.Range is zero, the whole file is implied.
// To reduce distraction, the trigger action (unknown, automatic, invoked)
// may affect what actions are offered.
func (e *Editor) CodeAction(ctx context.Context, loc protocol.Location, diagnostics []protocol.Diagnostic, trigger protocol.CodeActionTriggerKind) ([]protocol.CodeAction, error) {
	if e.Server == nil {
		return nil, nil
	}
	path := e.sandbox.Workdir.URIToPath(loc.URI)
	e.mu.Lock()
	_, ok := e.buffers[path]
	e.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("buffer %q is not open", path)
	}
	params := &protocol.CodeActionParams{
		TextDocument: e.TextDocumentIdentifier(path),
		Context: protocol.CodeActionContext{
			Diagnostics: diagnostics,
			TriggerKind: &trigger,
			Only:        []protocol.CodeActionKind{protocol.Empty}, // => all
		},
		Range: loc.Range, // may be zero
	}
	lens, err := e.Server.CodeAction(ctx, params)
	if err != nil {
		return nil, err
	}
	return lens, nil
}

func (e *Editor) EditResolveSupport() (bool, error) {
	capabilities, err := clientCapabilities(e.Config())
	if err != nil {
		return false, err
	}
	return capabilities.TextDocument.CodeAction.ResolveSupport != nil && slices.Contains(capabilities.TextDocument.CodeAction.ResolveSupport.Properties, "edit"), nil
}

// Hover triggers a hover at the given position in an open buffer.
// It may return (nil, zero) if no symbol was selected.
func (e *Editor) Hover(ctx context.Context, loc protocol.Location) (*protocol.MarkupContent, protocol.Location, error) {
	if err := e.checkBufferLocation(loc); err != nil {
		return nil, protocol.Location{}, err
	}
	params := &protocol.HoverParams{}
	params.TextDocument.URI = loc.URI
	params.Position = loc.Range.Start

	resp, err := e.Server.Hover(ctx, params)
	if err != nil {
		return nil, protocol.Location{}, fmt.Errorf("hover: %w", err)
	}
	if resp == nil {
		return nil, protocol.Location{}, nil // e.g. no selected symbol
	}
	return &resp.Contents, loc.URI.Location(resp.Range), nil
}

func (e *Editor) DocumentLink(ctx context.Context, path string) ([]protocol.DocumentLink, error) {
	if e.Server == nil {
		return nil, nil
	}
	params := &protocol.DocumentLinkParams{}
	params.TextDocument.URI = e.sandbox.Workdir.URI(path)
	return e.Server.DocumentLink(ctx, params)
}

func (e *Editor) DocumentHighlight(ctx context.Context, loc protocol.Location) ([]protocol.DocumentHighlight, error) {
	if e.Server == nil {
		return nil, nil
	}
	if err := e.checkBufferLocation(loc); err != nil {
		return nil, err
	}
	params := &protocol.DocumentHighlightParams{}
	params.TextDocument.URI = loc.URI
	params.Position = loc.Range.Start

	return e.Server.DocumentHighlight(ctx, params)
}

// SemanticTokensFull invokes textDocument/semanticTokens/full, and interprets
// its result.
func (e *Editor) SemanticTokensFull(ctx context.Context, path string) ([]SemanticToken, error) {
	p := &protocol.SemanticTokensParams{
		TextDocument: protocol.TextDocumentIdentifier{
			URI: e.sandbox.Workdir.URI(path),
		},
	}
	resp, err := e.Server.SemanticTokensFull(ctx, p)
	if err != nil {
		return nil, err
	}
	content, ok := e.BufferText(path)
	if !ok {
		return nil, fmt.Errorf("buffer %s is not open", path)
	}
	return e.interpretTokens(resp.Data, content), nil
}

// SemanticTokensRange invokes textDocument/semanticTokens/range, and
// interprets its result.
func (e *Editor) SemanticTokensRange(ctx context.Context, loc protocol.Location) ([]SemanticToken, error) {
	p := &protocol.SemanticTokensRangeParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: loc.URI},
		Range:        loc.Range,
	}
	resp, err := e.Server.SemanticTokensRange(ctx, p)
	if err != nil {
		return nil, err
	}
	path := e.sandbox.Workdir.URIToPath(loc.URI)
	// As noted above: buffers should be keyed by protocol.DocumentURI.
	content, ok := e.BufferText(path)
	if !ok {
		return nil, fmt.Errorf("buffer %s is not open", path)
	}
	return e.interpretTokens(resp.Data, content), nil
}

// A SemanticToken is an interpreted semantic token value.
type SemanticToken struct {
	Token     string
	TokenType string
	Mod       string
}

// Note: previously this function elided comment, string, and number tokens.
// Instead, filtering of token types should be done by the caller.
func (e *Editor) interpretTokens(x []uint32, contents string) []SemanticToken {
	legend := e.semTokOpts.Legend
	lines := strings.Split(contents, "\n")
	ans := []SemanticToken{}
	line, col := 1, 1
	for i := 0; i < len(x); i += 5 {
		line += int(x[i])
		col += int(x[i+1])
		if x[i] != 0 { // new line
			col = int(x[i+1]) + 1 // 1-based column numbers
		}
		sz := x[i+2]
		t := legend.TokenTypes[x[i+3]]
		l := x[i+4]
		var mods []string
		for i, mod := range legend.TokenModifiers {
			if l&(1<<i) != 0 {
				mods = append(mods, mod)
			}
		}
		// Preexisting note: "col is a utf-8 offset"
		// TODO(rfindley): is that true? Or is it UTF-16, like other columns in the LSP?
		tok := lines[line-1][col-1 : col-1+int(sz)]
		ans = append(ans, SemanticToken{tok, t, strings.Join(mods, " ")})
	}
	return ans
}
