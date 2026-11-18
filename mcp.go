// MCP endpoint (/mcp): JSON-RPC 2.0 server speaking the modern 2026-07-28
// protocol (server/discover) with a legacy 2025-11-25 initialize fallback.
// Six tools act on the same store as REST and GraphQL.
package main

import (
	"encoding/json"
	"strconv"
	"strings"
)

const mcpSessionID = "forge-session-1"

var mcpToolsJSON = `[{"name":"create_repo","description":"Create a repository for the viewer","inputSchema":{"type":"object","properties":{"name":{"type":"string","description":"Repository name"},"description":{"type":"string","description":"Repository description"},"private":{"type":"boolean","description":"Whether the repository is private"}},"required":["name"]}},{"name":"get_repo","description":"Get a repository by owner and name","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login"},"name":{"type":"string","description":"Repository name"},"repo":{"type":"string","description":"Repository name"},"full_name":{"type":"string","description":"Repository full name owner/repo"}},"required":["repo"]}},{"name":"create_issue","description":"Create an issue in a repository","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login"},"repo":{"type":"string","description":"Repository name"},"title":{"type":"string","description":"Issue title"},"body":{"type":"string","description":"Issue body"},"labels":{"type":"array","items":{"type":"string"},"description":"Issue labels"}},"required":["repo","title"]}},{"name":"list_issues","description":"List issues in a repository","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login"},"repo":{"type":"string","description":"Repository name"},"state":{"type":"string","description":"Issue state: open, closed or all"}},"required":["repo"]}},{"name":"create_pull_request","description":"Create a pull request","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login"},"repo":{"type":"string","description":"Repository name"},"head":{"type":"string","description":"Head branch name"},"base":{"type":"string","description":"Base branch name"},"title":{"type":"string","description":"Pull request title"},"body":{"type":"string","description":"Pull request body"}},"required":["repo","head","base","title"]}},{"name":"merge_pull_request","description":"Merge a pull request with a squash merge","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login"},"repo":{"type":"string","description":"Repository name"},"pull_number":{"type":"integer","description":"Pull request number"}},"required":["repo","pull_number"]}}]`

func (s *server) mcp(req *request) response {
	trimmed := strings.TrimSpace(string(req.body))
	if trimmed == "" {
		return s.mcpError(nil, -32700, "Parse error")
	}
	var msg map[string]any
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	if err := dec.Decode(&msg); err != nil {
		// Malformed JSON: HTTP 400 with a -32700 parse error.
		r := s.mcpError(nil, -32700, "Parse error")
		r.status = 400
		return r
	}
	id, hasID := msg["id"]
	method, _ := msg["method"].(string)
	params, _ := msg["params"].(map[string]any)

	// Notifications carry no id and get an empty 202.
	if !hasID {
		if method == "notifications/initialized" || strings.HasPrefix(method, "notifications/") {
			return response{status: 202, body: []byte{}}
		}
		return response{status: 202, body: []byte{}}
	}

	switch method {
	case "server/discover":
		result := `{"supportedVersions":["2026-07-28","2025-11-25","2025-06-18"],"protocolVersion":"2026-07-28","serverInfo":{"name":"forge","version":"1.0.0"},"capabilities":{"tools":{}}}`
		return s.mcpResult(id, result)
	case "initialize":
		result := `{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"forge","version":"1.0.0"}}`
		return s.mcpResultSession(id, result)
	case "ping":
		return s.mcpResult(id, `{}`)
	case "tools/list":
		return s.mcpResult(id, `{"tools":`+mcpToolsJSON+`}`)
	case "tools/call":
		return s.mcpToolsCall(id, params)
	default:
		return s.mcpError(id, -32601, "Method not found: "+method)
	}
}

func mcpIDJSON(id any) string {
	switch v := id.(type) {
	case nil:
		return "null"
	case string:
		return quote(v)
	case json.Number:
		return v.String()
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		if v {
			return "true"
		}
		return "false"
	default:
		return "null"
	}
}

func (s *server) mcpResult(id any, resultJSON string) response {
	body := `{"jsonrpc":"2.0","id":` + mcpIDJSON(id) + `,"result":` + resultJSON + `}`
	return response{status: 200, body: []byte(body)}
}

func (s *server) mcpResultSession(id any, resultJSON string) response {
	r := s.mcpResult(id, resultJSON)
	r.extra = []header{{"Mcp-Session-Id", mcpSessionID}}
	return r
}

func (s *server) mcpError(id any, code int, message string) response {
	body := `{"jsonrpc":"2.0","id":` + mcpIDJSON(id) + `,"error":{"code":` + strconv.Itoa(code) + `,"message":` + quote(message) + `}}`
	return response{status: 200, body: []byte(body)}
}

func mcpTextResult(text string) string {
	return `{"content":[{"type":"text","text":` + quote(text) + `}]}`
}

func mcpIsError(text string) string {
	return `{"isError":true,"content":[{"type":"text","text":` + quote(text) + `}]}`
}

// mcpArgs flattens tool arguments: the arguments object itself, or an
// "arguments" wrapper, with flexible key aliases resolved by the caller.
func mcpArgs(params map[string]any) map[string]any {
	if params == nil {
		return map[string]any{}
	}
	if a, ok := params["arguments"].(map[string]any); ok {
		return a
	}
	return params
}

func mcpArgStr(args map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := args[k]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return ""
}

func mcpArgInt(args map[string]any, keys ...string) (int, bool) {
	for _, k := range keys {
		if v, ok := args[k]; ok {
			switch n := v.(type) {
			case json.Number:
				if i, err := n.Int64(); err == nil {
					return int(i), true
				}
			case float64:
				return int(n), true
			case string:
				if i, err := strconv.Atoi(n); err == nil {
					return i, true
				}
			}
		}
	}
	return 0, false
}

func mcpArgBool(args map[string]any, keys ...string) bool {
	for _, k := range keys {
		if v, ok := args[k]; ok {
			if b, ok := v.(bool); ok {
				return b
			}
		}
	}
	return false
}

// mcpRepoName resolves a repository name from flexible argument spellings:
// full_name "owner/repo", owner_login + name, repository, repo, or name.
func mcpRepoName(args map[string]any) (owner, name string) {
	owner = mcpArgStr(args, "owner", "owner_login")
	name = mcpArgStr(args, "name", "repo", "repository", "full_name")
	if full := mcpArgStr(args, "full_name"); full != "" {
		if i := strings.IndexByte(full, '/'); i >= 0 {
			owner, name = full[:i], full[i+1:]
		}
	}
	if i := strings.IndexByte(name, '/'); i >= 0 {
		if owner == "" {
			owner = name[:i]
		}
		name = name[i+1:]
	}
	return owner, name
}

// ---------------------------------------------------------------------------
// tools/call

func (s *server) mcpToolsCall(id any, params map[string]any) response {
	name, _ := params["name"].(string)
	args := mcpArgs(params)
	tool := mcpMatchTool(name)
	if tool == "" {
		return s.mcpError(id, -32602, "Unknown tool: "+name)
	}
	switch tool {
	case "create_repo":
		return s.mcpResult(id, s.mcpCreateRepo(args))
	case "get_repo":
		return s.mcpResult(id, s.mcpGetRepo(args))
	case "create_issue":
		return s.mcpResult(id, s.mcpCreateIssue(args))
	case "list_issues":
		return s.mcpResult(id, s.mcpListIssues(args))
	case "create_pull_request":
		return s.mcpResult(id, s.mcpCreatePullRequest(args))
	case "merge_pull_request":
		return s.mcpResult(id, s.mcpMergePullRequest(args))
	}
	return s.mcpError(id, -32602, "Unknown tool: "+name)
}

// mcpMatchTool matches a requested tool name against the six known tools,
// allowing a server prefix and case-insensitive suffix matching.
func mcpMatchTool(name string) string {
	lower := strings.ToLower(name)
	known := []string{"create_repo", "get_repo", "create_issue", "list_issues", "create_pull_request", "merge_pull_request"}
	for _, k := range known {
		if lower == k || strings.HasSuffix(lower, "_"+k) || strings.HasSuffix(lower, k) {
			return k
		}
	}
	return ""
}

// mcpFindRepo resolves the target repo for a tool call; on failure it
// returns an isError result JSON string.
func (s *server) mcpFindRepo(args map[string]any) (*Repo, string) {
	owner, name := mcpRepoName(args)
	if name == "" {
		return nil, mcpIsError("Missing required argument: repo name")
	}
	if owner != "" && owner != viewerLogin {
		return nil, mcpIsError("Repository not found: " + owner + "/" + name)
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	r := s.store.getRepo(name)
	if r == nil {
		return nil, mcpIsError("Repository not found: " + name)
	}
	return r, ""
}

func (s *server) mcpCreateRepo(args map[string]any) string {
	name := mcpArgStr(args, "name", "repo", "repository")
	if name == "" {
		return mcpIsError("Missing required argument: name")
	}
	if i := strings.IndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	desc := mcpArgStr(args, "description")
	priv := mcpArgBool(args, "private")
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	r, ok := s.store.createRepo(name, desc, priv)
	if !ok {
		return mcpIsError("Repository already exists: " + name)
	}
	return mcpTextResult(string(s.store.repoJSON(r)))
}

func (s *server) mcpGetRepo(args map[string]any) string {
	r, errJSON := s.mcpFindRepo(args)
	if errJSON != "" {
		return errJSON
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	return mcpTextResult(string(s.store.repoJSON(r)))
}

func (s *server) mcpCreateIssue(args map[string]any) string {
	title := mcpArgStr(args, "title")
	if title == "" {
		return mcpIsError("Missing required argument: title")
	}
	body := mcpArgStr(args, "body")
	var labels []string
	if arr, ok := args["labels"].([]any); ok {
		for _, e := range arr {
			if str, ok := e.(string); ok {
				labels = append(labels, str)
			}
		}
	}
	r, errJSON := s.mcpFindRepo(args)
	if errJSON != "" {
		return errJSON
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	is, errR := s.createIssueLocked(r, title, body, labels)
	if errR != nil {
		return mcpIsError("Failed to create issue: label does not exist")
	}
	return mcpTextResult(string(issueJSON(s.store, r, is)))
}

func (s *server) mcpListIssues(args map[string]any) string {
	r, errJSON := s.mcpFindRepo(args)
	if errJSON != "" {
		return errJSON
	}
	state := mcpArgStr(args, "state")
	if state == "" {
		state = "open"
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	var b strings.Builder
	b.WriteByte('[')
	first := true
	for _, is := range r.Issues {
		if state != "all" && is.State != state {
			continue
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		b.Write(issueJSON(s.store, r, is))
	}
	b.WriteByte(']')
	return mcpTextResult(b.String())
}

func (s *server) mcpCreatePullRequest(args map[string]any) string {
	head := mcpArgStr(args, "head", "head_ref", "headRef")
	base := mcpArgStr(args, "base", "base_ref", "baseRef")
	title := mcpArgStr(args, "title")
	if head == "" || base == "" || title == "" {
		return mcpIsError("Missing required argument: head, base and title are required")
	}
	body := mcpArgStr(args, "body")
	r, errJSON := s.mcpFindRepo(args)
	if errJSON != "" {
		return errJSON
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	p, errR := s.createPullLocked(r, title, body, head, base)
	if errR != nil {
		return mcpIsError("Failed to create pull request: head or base branch does not exist, or a pull request already exists")
	}
	return mcpTextResult(string(s.pullJSON(r, p)))
}

func (s *server) mcpMergePullRequest(args map[string]any) string {
	num, ok := mcpArgInt(args, "pull_number", "pullNumber", "number")
	if !ok {
		return mcpIsError("Missing required argument: pull_number")
	}
	r, errJSON := s.mcpFindRepo(args)
	if errJSON != "" {
		return errJSON
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	p := r.PullsByNum[num]
	if p == nil {
		return mcpIsError("Pull request not found: #" + strconv.Itoa(num))
	}
	mergeSHA, errR := s.mergePullLocked(r, p)
	if errR != nil {
		return mcpIsError("Failed to merge pull request: conflict or already merged")
	}
	body := `{"sha":` + quote(mergeSHA) + `,"merged":true,"message":"Merge pull request #` + strconv.Itoa(p.Number) + ` from ` + viewerLogin + `/` + p.HeadRef + `"}`
	return mcpTextResult(body)
}
