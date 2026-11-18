// REST v3 surface plus the request router. Every reply is JSON with a
// non-empty X-GitHub-Request-Id; errors carry {"message": string}.
package main

import (
	"encoding/base64"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// router

func (s *server) dispatch(req *request) response {
	path := req.path
	method := req.method

	if path == "/healthz" {
		if method != "GET" {
			return jsonErr(404, "Not Found")
		}
		return response{status: 200, body: []byte(`{"ok":true}`)}
	}

	// Git smart HTTP.
	if strings.HasSuffix(path, ".git/info/refs") {
		if method != "GET" || !s.authed(req) {
			return s.authOr404(req, method == "GET")
		}
		owner, name := splitGitPath(path[:len(path)-len("/info/refs")])
		service, _ := queryVal(req.query, "service")
		if service != "git-upload-pack" && service != "git-receive-pack" {
			return jsonErr(404, "Not Found")
		}
		return s.gitInfoRefs(req, owner, name, service)
	}
	if strings.HasSuffix(path, ".git/git-upload-pack") {
		if method != "POST" || !s.authed(req) {
			return s.authOr404(req, method == "POST")
		}
		owner, name := splitGitPath(path[:len(path)-len("/git-upload-pack")])
		return s.gitUploadPack(req, owner, name)
	}
	if strings.HasSuffix(path, ".git/git-receive-pack") {
		if method != "POST" || !s.authed(req) {
			return s.authOr404(req, method == "POST")
		}
		owner, name := splitGitPath(path[:len(path)-len("/git-receive-pack")])
		return s.gitReceivePack(req, owner, name)
	}

	// GraphQL and MCP.
	if path == "/api/graphql" {
		if method != "POST" {
			return jsonErr(404, "Not Found")
		}
		if !s.authed(req) {
			return jsonErr(401, "Requires authentication")
		}
		return s.graphql(req)
	}
	if path == "/mcp" {
		if method == "DELETE" {
			return response{status: 200, body: []byte{}}
		}
		if method != "POST" {
			return jsonErr(404, "Not Found")
		}
		if !s.authed(req) {
			return jsonErr(401, "Requires authentication")
		}
		return s.mcp(req)
	}

	// REST v3.
	if path == "/api/v3" || path == "/api/v3/" {
		if method != "GET" {
			return jsonErr(404, "Not Found")
		}
		if !s.authed(req) {
			return jsonErr(401, "Requires authentication")
		}
		return response{status: 200, body: []byte(`{"current_user_url":"/api/v3/user","repository_url":"/api/v3/repos/{owner}/{repo}","user_url":"/api/v3/users/{username}","meta_url":"/api/v3/meta"}`), extra: []header{{"X-OAuth-Scopes", "repo, read:org"}}}
	}
	if !strings.HasPrefix(path, "/api/v3/") {
		return jsonErr(404, "Not Found")
	}
	if !s.authed(req) {
		return jsonErr(401, "Requires authentication")
	}
	return s.rest(req, path[len("/api/v3/"):])
}

// authOr404 answers 401 when the method is right but the token is wrong,
// and 404 otherwise (an unknown git route is a 404, never a 401).
func (s *server) authOr404(req *request, methodOK bool) response {
	if methodOK {
		return jsonErr(401, "Requires authentication")
	}
	return jsonErr(404, "Not Found")
}

// splitGitPath splits "/owner/repo" (the .git suffix already stripped).
func splitGitPath(p string) (string, string) {
	p = strings.TrimPrefix(p, "/")
	i := strings.IndexByte(p, '/')
	if i < 0 {
		return p, ""
	}
	name := strings.TrimSuffix(p[i+1:], ".git")
	return p[:i], name
}

// ---------------------------------------------------------------------------
// REST helpers

func (s *server) rest(req *request, rest string) response {
	parts := strings.Split(rest, "/")
	method := req.method

	// /meta
	if rest == "meta" {
		if method != "GET" {
			return jsonErr(404, "Not Found")
		}
		return response{status: 200, body: []byte(`{"installed_version":"1.0.0"}`)}
	}
	// /user
	if rest == "user" {
		if method != "GET" {
			return jsonErr(404, "Not Found")
		}
		return response{status: 200, body: []byte(`{"login":"` + viewerLogin + `","id":1000,"name":"` + viewerName + `","email":"` + viewerEmail + `","type":"User"}`)}
	}
	// /user/repos
	if rest == "user/repos" {
		if method == "GET" {
			return s.listUserRepos(req)
		}
		if method == "POST" {
			return s.createUserRepo(req)
		}
		return jsonErr(404, "Not Found")
	}
	// /repos/{owner}/{repo}[/...]
	if len(parts) >= 3 && parts[0] == "repos" {
		owner, name := parts[1], parts[2]
		if owner != viewerLogin {
			return jsonErr(404, "Not Found")
		}
		tail := parts[3:]
		switch {
		case len(tail) == 0:
			switch method {
			case "GET":
				return s.getRepo(req, name)
			case "PATCH":
				return s.patchRepo(req, name)
			case "DELETE":
				return s.deleteRepo(req, name)
			}
		case len(tail) == 1 && tail[0] == "branches":
			if method == "GET" {
				return s.listBranches(req, name)
			}
		case len(tail) == 2 && tail[0] == "branches":
			if method == "GET" {
				return s.getBranch(req, name, tail[1])
			}
		case len(tail) == 1 && tail[0] == "commits":
			if method == "GET" {
				return s.listCommits(req, name)
			}
		case len(tail) == 1 && tail[0] == "labels":
			if method == "GET" {
				return s.listLabels(req, name)
			}
			if method == "POST" {
				return s.createLabel(req, name)
			}
		case len(tail) == 1 && tail[0] == "issues":
			if method == "GET" {
				return s.listIssues(req, name)
			}
			if method == "POST" {
				return s.createIssueREST(req, name)
			}
		case len(tail) == 2 && tail[0] == "issues":
			if method == "GET" {
				return s.getIssueREST(req, name, tail[1])
			}
			if method == "PATCH" {
				return s.patchIssueREST(req, name, tail[1])
			}
		case len(tail) == 3 && tail[0] == "issues" && tail[2] == "comments":
			if method == "GET" {
				return s.listComments(req, name, tail[1])
			}
			if method == "POST" {
				return s.createComment(req, name, tail[1])
			}
		case len(tail) == 1 && tail[0] == "pulls":
			if method == "GET" {
				return s.listPulls(req, name)
			}
			if method == "POST" {
				return s.createPullREST(req, name)
			}
		case len(tail) == 2 && tail[0] == "pulls":
			if method == "GET" {
				return s.getPullREST(req, name, tail[1])
			}
		case len(tail) == 3 && tail[0] == "pulls" && tail[2] == "files":
			if method == "GET" {
				return s.listPullFiles(req, name, tail[1])
			}
		case len(tail) == 3 && tail[0] == "pulls" && tail[2] == "merge":
			if method == "PUT" {
				return s.mergePullREST(req, name, tail[1])
			}
		case len(tail) >= 1 && tail[0] == "contents":
			// /contents/{path...} — path may itself hold slashes; the
			// router split them, so rejoin.
			contentPath := strings.Join(tail[1:], "/")
			switch method {
			case "GET":
				return s.getContents(req, name, contentPath)
			case "PUT":
				return s.putContents(req, name, contentPath)
			case "DELETE":
				return s.deleteContents(req, name, contentPath)
			}
		case len(tail) == 2 && tail[0] == "git" && tail[1] == "refs":
			if method == "POST" {
				return s.createGitRef(req, name)
			}
		case len(tail) == 5 && tail[0] == "git" && tail[1] == "refs" && tail[2] == "heads":
			if method == "GET" {
				return s.getGitRef(req, name, "refs/heads/"+tail[3]+"/"+tail[4])
			}
		case len(tail) == 4 && tail[0] == "git" && tail[1] == "refs" && tail[2] == "heads":
			if method == "GET" {
				return s.getGitRef(req, name, "refs/heads/"+tail[3])
			}
		}
		return jsonErr(404, "Not Found")
	}
	return jsonErr(404, "Not Found")
}

// parseBody decodes a JSON request body into a map. An empty body is an
// empty object; malformed JSON is a 400.
func parseBody(req *request) (map[string]any, *response) {
	if len(req.body) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(req.body, &m); err != nil {
		r := jsonErr(400, "Invalid JSON body")
		return nil, &r
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func bodyString(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func bodyBool(m map[string]any, key string, def bool) bool {
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

// pageParams reads per_page/page from the query (defaults 30/1).
func pageParams(req *request) (perPage, page int) {
	perPage, page = 30, 1
	if v, ok := queryVal(req.query, "per_page"); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			perPage = n
			if perPage > 100 {
				perPage = 100
			}
		}
	}
	if v, ok := queryVal(req.query, "page"); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	return perPage, page
}

// paginate slices items and builds a Link header for the next page.
func (s *server) paginate(req *request, total int, perPage, page int) []header {
	pages := (total + perPage - 1) / perPage
	if page >= pages {
		return nil
	}
	host, _ := req.header("host")
	if host == "" {
		host = "127.0.0.1"
	}
	base := "http://" + host + req.path
	next := base + "?per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page+1)
	last := base + "?per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(pages)
	return []header{{"Link", `<` + next + `>; rel="next", <` + last + `>; rel="last"`}}
}

// ---------------------------------------------------------------------------
// repositories

func (s *server) listUserRepos(req *request) response {
	perPage, page := pageParams(req)
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	total := len(s.store.order)
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, r := range s.store.order[start:end] {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(s.store.repoJSON(r))
	}
	b.WriteByte(']')
	return response{status: 200, body: []byte(b.String()), extra: s.paginate(req, total, perPage, page)}
}

func (s *server) createUserRepo(req *request) response {
	m, errResp := parseBody(req)
	if errResp != nil {
		return *errResp
	}
	name := bodyString(m, "name")
	if name == "" || len(name) > 100 {
		return jsonErr(422, "Validation Failed: name is required")
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	r, ok := s.store.createRepo(name, bodyString(m, "description"), bodyBool(m, "private", false))
	if !ok {
		return jsonErr(422, "Validation Failed: repository already exists")
	}
	return response{status: 201, body: s.store.repoJSON(r)}
}

func (s *server) getRepo(req *request, name string) response {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	r := s.store.getRepo(name)
	if r == nil {
		return jsonErr(404, "Not Found")
	}
	return response{status: 200, body: s.store.repoJSON(r)}
}

func (s *server) patchRepo(req *request, name string) response {
	m, errResp := parseBody(req)
	if errResp != nil {
		return *errResp
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	r := s.store.getRepo(name)
	if r == nil {
		return jsonErr(404, "Not Found")
	}
	if v, ok := m["description"]; ok {
		if d, ok := v.(string); ok {
			r.Description = d
		} else if v == nil {
			r.Description = ""
		}
	}
	return response{status: 200, body: s.store.repoJSON(r)}
}

func (s *server) deleteRepo(req *request, name string) response {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if !s.store.deleteRepo(name) {
		return jsonErr(404, "Not Found")
	}
	return response{status: 204, body: nil}
}

// ---------------------------------------------------------------------------
// branches, commits, git refs

func (s *server) listBranches(req *request, name string) response {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	r := s.store.getRepo(name)
	if r == nil {
		return jsonErr(404, "Not Found")
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, branch := range r.sortedBranchNames() {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"name":`)
		b.WriteString(quote(branch))
		b.WriteString(`,"commit":{"sha":`)
		b.WriteString(quote(r.Refs["refs/heads/"+branch]))
		b.WriteString(`},"protected":false}`)
	}
	b.WriteByte(']')
	return response{status: 200, body: []byte(b.String())}
}

func (s *server) getBranch(req *request, name, branch string) response {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	r := s.store.getRepo(name)
	if r == nil {
		return jsonErr(404, "Not Found")
	}
	oid, ok := r.Refs["refs/heads/"+branch]
	if !ok {
		return jsonErr(404, "Not Found")
	}
	return response{status: 200, body: []byte(`{"name":` + quote(branch) + `,"commit":{"sha":` + quote(oid) + `},"protected":false}`)}
}

func (s *server) listCommits(req *request, name string) response {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	r := s.store.getRepo(name)
	if r == nil {
		return jsonErr(404, "Not Found")
	}
	ref := r.headTarget()
	if v, ok := queryVal(req.query, "sha"); ok && v != "" {
		ref = v
	}
	var tip string
	if isOID(ref) {
		tip = ref
	} else if strings.HasPrefix(ref, "refs/") {
		tip = r.Refs[ref]
	} else {
		tip = r.Refs["refs/heads/"+ref]
		if tip == "" {
			tip = r.Refs["refs/tags/"+ref]
		}
	}
	if tip == "" {
		return response{status: 200, body: []byte(`[]`)}
	}
	// Walk all parents from the tip, tip first, deduplicated.
	var order []string
	seen := make(map[string]bool)
	stack := []string{tip}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := r.Objects[oid]
		if obj == nil || obj.typ != "commit" {
			continue
		}
		order = append(order, oid)
		c := parseCommit(obj.data)
		for i := len(c.parents) - 1; i >= 0; i-- {
			stack = append(stack, c.parents[i])
		}
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, oid := range order {
		if i > 0 {
			b.WriteByte(',')
		}
		c := parseCommit(r.Objects[oid].data)
		b.WriteString(`{"sha":`)
		b.WriteString(quote(oid))
		b.WriteString(`,"commit":{"message":`)
		b.WriteString(quote(c.message))
		b.WriteString(`,"author":`)
		b.WriteString(identityJSON(c.author))
		b.WriteString(`,"committer":`)
		b.WriteString(identityJSON(c.committer))
		b.WriteString(`},"author":{"login":`)
		b.WriteString(quote(viewerLogin))
		b.WriteString(`}}`)
	}
	b.WriteByte(']')
	return response{status: 200, body: []byte(b.String())}
}

// identityJSON renders "Name <email> <epoch> +0000" as a JSON object.
func identityJSON(line string) string {
	name, email, epoch := splitIdentity(line)
	return `{"name":` + quote(name) + `,"email":` + quote(email) + `,"date":` + quote(epochToISO(epoch)) + `}`
}

func splitIdentity(line string) (name, email string, epoch int) {
	lt := strings.IndexByte(line, '<')
	gt := strings.IndexByte(line, '>')
	if lt < 0 || gt < 0 {
		return line, "", 0
	}
	name = strings.TrimSpace(line[:lt])
	email = line[lt+1 : gt]
	rest := strings.Fields(line[gt+1:])
	if len(rest) > 0 {
		epoch, _ = strconv.Atoi(rest[0])
	}
	return name, email, epoch
}

func epochToISO(epoch int) string {
	if epoch <= 0 {
		return isoTime(0)
	}
	days := epoch / 86400
	secs := epoch % 86400
	y, m, d := civilFromDays(days)
	return pad4(y) + "-" + pad2(m) + "-" + pad2(d) + "T" + pad2(secs/3600) + ":" + pad2((secs/60)%60) + ":" + pad2(secs%60) + "Z"
}

func (s *server) createGitRef(req *request, name string) response {
	m, errResp := parseBody(req)
	if errResp != nil {
		return *errResp
	}
	ref := bodyString(m, "ref")
	sha := bodyString(m, "sha")
	if ref == "" || sha == "" || !isOID(sha) {
		return jsonErr(422, "Validation Failed: ref and sha are required")
	}
	if !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/") {
		return jsonErr(422, "Validation Failed: invalid ref")
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	r := s.store.getRepo(name)
	if r == nil {
		return jsonErr(404, "Not Found")
	}
	if r.Objects[sha] == nil {
		return jsonErr(422, "Validation Failed: object does not exist")
	}
	if old, ok := r.Refs[ref]; ok {
		if old == sha {
			return response{status: 201, body: []byte(`{"ref":` + quote(ref) + `,"object":{"sha":` + quote(sha) + `,"type":"commit"}}`)}
		}
		return jsonErr(422, "Validation Failed: reference already exists")
	}
	r.Refs[ref] = sha
	r.dropPackCache()
	return response{status: 201, body: []byte(`{"ref":` + quote(ref) + `,"object":{"sha":` + quote(sha) + `,"type":"commit"}}`)}
}

func (s *server) getGitRef(req *request, name, ref string) response {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	r := s.store.getRepo(name)
	if r == nil {
		return jsonErr(404, "Not Found")
	}
	oid, ok := r.Refs[ref]
	if !ok {
		return jsonErr(404, "Not Found")
	}
	return response{status: 200, body: []byte(`{"ref":` + quote(ref) + `,"object":{"sha":` + quote(oid) + `,"type":"commit"}}`)}
}

// ---------------------------------------------------------------------------
// contents

// flattenTree returns path -> (mode, oid) for every blob in a tree.
func flattenTree(r *Repo, treeOID string) map[string][2]string {
	out := make(map[string][2]string)
	var walk func(oid, prefix string)
	walk = func(oid, prefix string) {
		obj := r.Objects[oid]
		if obj == nil || obj.typ != "tree" {
			return
		}
		for _, e := range parseTree(obj.data) {
			path := prefix + e.name
			if e.mode == "40000" {
				walk(e.oid, path+"/")
			} else {
				out[path] = [2]string{e.mode, e.oid}
			}
		}
	}
	walk(treeOID, "")
	return out
}

// rebuildTree builds nested tree objects from a flat path map and stores
// them, returning the root tree oid.
func (s *server) rebuildTree(r *Repo, files map[string][2]string) string {
	type node struct {
		files map[string][2]string
		dirs  map[string]*node
	}
	newNode := func() *node { return &node{make(map[string][2]string), make(map[string]*node)} }
	root := newNode()
	for path, entry := range files {
		parts := strings.Split(path, "/")
		n := root
		for _, part := range parts[:len(parts)-1] {
			child, ok := n.dirs[part]
			if !ok {
				child = newNode()
				n.dirs[part] = child
			}
			n = child
		}
		n.files[parts[len(parts)-1]] = entry
	}
	var build func(n *node) string
	build = func(n *node) string {
		var entries []treeEntry
		for name, e := range n.files {
			entries = append(entries, treeEntry{e[0], name, e[1]})
		}
		for name, child := range n.dirs {
			entries = append(entries, treeEntry{"40000", name, build(child)})
		}
		data := buildTree(entries)
		oid := hashObject("tree", data)
		r.Objects[oid] = &gitObject{"tree", data}
		return oid
	}
	return build(root)
}

// contentsCommit creates a commit on branchRef applying one file change
// (content nil deletes the path) and returns the new commit oid.
func (s *server) contentsCommit(r *Repo, branchRef, path string, content []byte, message string, del bool) (string, string) {
	tip := r.Refs[branchRef]
	files := make(map[string][2]string)
	if tip != "" {
		if obj := r.Objects[tip]; obj != nil && obj.typ == "commit" {
			files = flattenTree(r, parseCommit(obj.data).tree)
		}
	}
	var blobSHA string
	if del {
		delete(files, path)
	} else {
		blobSHA = hashObject("blob", content)
		r.Objects[blobSHA] = &gitObject{"blob", content}
		files[path] = [2]string{"100644", blobSHA}
	}
	treeOID := s.rebuildTree(r, files)
	var parents []string
	if tip != "" {
		parents = []string{tip}
	}
	epoch := commitBaseEpoch + s.store.nextCommitSeq()
	ident := identityLine(epoch)
	commitData := buildCommit(treeOID, parents, ident, ident, message)
	commitOID := hashObject("commit", commitData)
	r.Objects[commitOID] = &gitObject{"commit", commitData}
	r.Refs[branchRef] = commitOID
	r.dropPackCache()
	s.clearPullCaches(r)
	return commitOID, blobSHA
}

// resolveContentsBranch picks the branch a contents write targets: the
// named branch when it exists, else the default branch's tip as its base.
func (s *server) resolveContentsBranch(r *Repo, branch string) string {
	if branch == "" {
		return "refs/heads/" + r.DefaultBranch
	}
	ref := "refs/heads/" + branch
	if _, ok := r.Refs[ref]; ok {
		return ref
	}
	return ref // new branch: built on the default tip inside contentsCommit
}

func (s *server) putContents(req *request, name, path string) response {
	m, errResp := parseBody(req)
	if errResp != nil {
		return *errResp
	}
	message := bodyString(m, "message")
	encoded := bodyString(m, "content")
	content, err := base64.StdEncoding.DecodeString(strings.NewReplacer("\n", "", "\r", "", " ", "", "\t", "").Replace(encoded))
	if err != nil {
		return jsonErr(400, "Invalid base64 content")
	}
	branch := bodyString(m, "branch")
	sha := bodyString(m, "sha")
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	r := s.store.getRepo(name)
	if r == nil {
		return jsonErr(404, "Not Found")
	}
	branchRef := s.resolveContentsBranch(r, branch)
	// Stale-sha guard: the sha must name the file's current blob.
	tip := r.Refs[branchRef]
	exists := false
	var currentSHA string
	if tip != "" {
		if obj := r.Objects[tip]; obj != nil && obj.typ == "commit" {
			files := flattenTree(r, parseCommit(obj.data).tree)
			if e, ok := files[path]; ok {
				exists = true
				currentSHA = e[1]
			}
		}
	}
	if sha != "" {
		if !exists || currentSHA != sha {
			return jsonErr(409, "Conflict: sha does not match the current file")
		}
	} else if exists {
		return jsonErr(409, "Conflict: sha is required to update a file")
	}
	if message == "" {
		message = "Update " + path
	}
	commitOID, blobSHA := s.contentsCommit(r, branchRef, path, content, message, false)
	status := 201
	if exists {
		status = 200
	}
	body := `{"content":{"name":` + quote(baseName(path)) + `,"path":` + quote(path) + `,"sha":` + quote(blobSHA) + `,"size":` + strconv.Itoa(len(content)) + `,"type":"file","encoding":"base64"},"commit":{"sha":` + quote(commitOID) + `}}`
	return response{status: status, body: []byte(body)}
}

func baseName(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

func (s *server) getContents(req *request, name, path string) response {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	r := s.store.getRepo(name)
	if r == nil {
		return jsonErr(404, "Not Found")
	}
	ref := r.headTarget()
	if v, ok := queryVal(req.query, "ref"); ok && v != "" {
		if strings.HasPrefix(v, "refs/") {
			ref = v
		} else {
			ref = "refs/heads/" + v
		}
	}
	tip := r.Refs[ref]
	if tip == "" {
		// A ref name may itself be a commit oid.
		if isOID(ref) || isOID(strings.TrimPrefix(ref, "refs/heads/")) {
			tip = ref
			if strings.HasPrefix(tip, "refs/") {
				tip = ""
			}
		}
		if tip == "" {
			return jsonErr(404, "Not Found")
		}
	}
	obj := r.Objects[tip]
	if obj == nil || obj.typ != "commit" {
		return jsonErr(404, "Not Found")
	}
	treeOID := parseCommit(obj.data).tree
	if path == "" {
		return response{status: 200, body: s.dirListing(r, treeOID, "")}
	}
	// Walk the tree to the path.
	parts := strings.Split(path, "/")
	cur := treeOID
	for i, part := range parts {
		curObj := r.Objects[cur]
		if curObj == nil || curObj.typ != "tree" {
			return jsonErr(404, "Not Found")
		}
		var found *treeEntry
		for _, e := range parseTree(curObj.data) {
			if e.name == part {
				e2 := e
				found = &e2
				break
			}
		}
		if found == nil {
			return jsonErr(404, "Not Found")
		}
		if i == len(parts)-1 {
			if found.mode == "40000" {
				return response{status: 200, body: s.dirListing(r, found.oid, path)}
			}
			blob := r.Objects[found.oid]
			if blob == nil {
				return jsonErr(404, "Not Found")
			}
			body := `{"type":"file","encoding":"base64","size":` + strconv.Itoa(len(blob.data)) + `,"name":` + quote(part) + `,"path":` + quote(path) + `,"sha":` + quote(found.oid) + `,"content":` + quote(base64.StdEncoding.EncodeToString(blob.data)) + `}`
			return response{status: 200, body: []byte(body)}
		}
		if found.mode != "40000" {
			return jsonErr(404, "Not Found")
		}
		cur = found.oid
	}
	return jsonErr(404, "Not Found")
}

// dirListing renders one directory's entries as a JSON array.
func (s *server) dirListing(r *Repo, treeOID, prefix string) []byte {
	obj := r.Objects[treeOID]
	var entries []treeEntry
	if obj != nil && obj.typ == "tree" {
		entries = parseTree(obj.data)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	var b strings.Builder
	b.WriteByte('[')
	for i, e := range entries {
		if i > 0 {
			b.WriteByte(',')
		}
		typ := "file"
		if e.mode == "40000" {
			typ = "dir"
		}
		size := 0
		if typ == "file" {
			if blob := r.Objects[e.oid]; blob != nil {
				size = len(blob.data)
			}
		}
		p := e.name
		if prefix != "" {
			p = prefix + "/" + e.name
		}
		b.WriteString(`{"type":`)
		b.WriteString(quote(typ))
		b.WriteString(`,"name":`)
		b.WriteString(quote(e.name))
		b.WriteString(`,"path":`)
		b.WriteString(quote(p))
		b.WriteString(`,"sha":`)
		b.WriteString(quote(e.oid))
		b.WriteString(`,"size":`)
		b.WriteString(strconv.Itoa(size))
		b.WriteString(`}`)
	}
	b.WriteByte(']')
	return []byte(b.String())
}

func (s *server) deleteContents(req *request, name, path string) response {
	m, errResp := parseBody(req)
	if errResp != nil {
		return *errResp
	}
	message := bodyString(m, "message")
	sha := bodyString(m, "sha")
	branch := bodyString(m, "branch")
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	r := s.store.getRepo(name)
	if r == nil {
		return jsonErr(404, "Not Found")
	}
	branchRef := s.resolveContentsBranch(r, branch)
	tip := r.Refs[branchRef]
	if tip == "" {
		return jsonErr(404, "Not Found")
	}
	obj := r.Objects[tip]
	if obj == nil || obj.typ != "commit" {
		return jsonErr(404, "Not Found")
	}
	files := flattenTree(r, parseCommit(obj.data).tree)
	e, ok := files[path]
	if !ok {
		return jsonErr(404, "Not Found")
	}
	if sha != "" && e[1] != sha {
		return jsonErr(409, "Conflict: sha does not match the current file")
	}
	if message == "" {
		message = "Delete " + path
	}
	commitOID, _ := s.contentsCommit(r, branchRef, path, nil, message, true)
	return response{status: 200, body: []byte(`{"commit":{"sha":` + quote(commitOID) + `}}`)}
}

// ---------------------------------------------------------------------------
// labels

func (s *server) listLabels(req *request, name string) response {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	r := s.store.getRepo(name)
	if r == nil {
		return jsonErr(404, "Not Found")
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, l := range r.LabelOrder {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"name":` + quote(l.Name) + `,"color":` + quote(l.Color) + `}`)
	}
	b.WriteByte(']')
	return response{status: 200, body: []byte(b.String())}
}

func (s *server) createLabel(req *request, name string) response {
	m, errResp := parseBody(req)
	if errResp != nil {
		return *errResp
	}
	label := bodyString(m, "name")
	color := bodyString(m, "color")
	if label == "" {
		return jsonErr(422, "Validation Failed: name is required")
	}
	if color == "" {
		color = "ededed"
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	r := s.store.getRepo(name)
	if r == nil {
		return jsonErr(404, "Not Found")
	}
	if existing, ok := r.Labels[label]; ok {
		return response{status: 201, body: []byte(`{"name":` + quote(existing.Name) + `,"color":` + quote(existing.Color) + `}`)}
	}
	l := &Label{Name: label, Color: color}
	r.Labels[label] = l
	r.LabelOrder = append(r.LabelOrder, l)
	return response{status: 201, body: []byte(`{"name":` + quote(l.Name) + `,"color":` + quote(l.Color) + `}`)}
}

// ---------------------------------------------------------------------------
// issues

func (s *server) createIssueREST(req *request, name string) response {
	m, errResp := parseBody(req)
	if errResp != nil {
		return *errResp
	}
	title := bodyString(m, "title")
	if title == "" {
		return jsonErr(422, "Validation Failed: title is required")
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	r := s.store.getRepo(name)
	if r == nil {
		return jsonErr(404, "Not Found")
	}
	is, errR := s.createIssueLocked(r, title, bodyString(m, "body"), toStringSlice(m["labels"]))
	if errR != nil {
		return *errR
	}
	return response{status: 201, body: issueJSON(s.store, r, is)}
}

func toStringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, e := range arr {
		if str, ok := e.(string); ok {
			out = append(out, str)
		}
	}
	return out
}

// createIssueLocked creates an issue; caller holds the store write lock.
func (s *server) createIssueLocked(r *Repo, title, body string, labels []string) (*Issue, *response) {
	var ls []*Label
	for _, name := range labels {
		l, ok := r.Labels[name]
		if !ok {
			r := jsonErr(422, "Validation Failed: label does not exist")
			return nil, &r
		}
		ls = append(ls, l)
	}
	is := &Issue{
		ID:     s.store.nextIssueID,
		Number: r.NextNum,
		Title:  title,
		Body:   body,
		State:  "open",
		Labels: ls,
		Seq:    s.store.nextSeq(),
	}
	s.store.nextIssueID++
	r.NextNum++
	r.Issues = append(r.Issues, is)
	r.IssuesByNum[is.Number] = is
	return is, nil
}

func (s *server) listIssues(req *request, name string) response {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	r := s.store.getRepo(name)
	if r == nil {
		return jsonErr(404, "Not Found")
	}
	state, _ := queryVal(req.query, "state")
	if state == "" {
		state = "open"
	}
	sortKey, _ := queryVal(req.query, "sort")
	direction, _ := queryVal(req.query, "direction")
	if direction == "" {
		direction = "desc"
	}
	_ = sortKey
	perPage, page := pageParams(req)
	var filtered []*Issue
	for _, is := range r.Issues {
		if state == "all" || is.State == state {
			filtered = append(filtered, is)
		}
	}
	// Issues are stored in creation order; created sort is by that order.
	if direction == "desc" {
		for i, j := 0, len(filtered)-1; i < j; i, j = i+1, j-1 {
			filtered[i], filtered[j] = filtered[j], filtered[i]
		}
	}
	total := len(filtered)
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, is := range filtered[start:end] {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(issueJSON(s.store, r, is))
	}
	b.WriteByte(']')
	return response{status: 200, body: []byte(b.String()), extra: s.paginate(req, total, perPage, page)}
}

func parseNum(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	return n, err == nil
}

func (s *server) getIssueREST(req *request, name, numStr string) response {
	n, ok := parseNum(numStr)
	if !ok {
		return jsonErr(404, "Not Found")
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	r := s.store.getRepo(name)
	if r == nil {
		return jsonErr(404, "Not Found")
	}
	is := r.IssuesByNum[n]
	if is == nil {
		return jsonErr(404, "Not Found")
	}
	return response{status: 200, body: issueJSON(s.store, r, is)}
}

func (s *server) patchIssueREST(req *request, name, numStr string) response {
	n, ok := parseNum(numStr)
	if !ok {
		return jsonErr(404, "Not Found")
	}
	m, errResp := parseBody(req)
	if errResp != nil {
		return *errResp
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	r := s.store.getRepo(name)
	if r == nil {
		return jsonErr(404, "Not Found")
	}
	is := r.IssuesByNum[n]
	if is == nil {
		return jsonErr(404, "Not Found")
	}
	if v, ok := m["state"]; ok {
		if st, ok := v.(string); ok && (st == "open" || st == "closed") {
			is.State = st
		}
	}
	if v, ok := m["title"]; ok {
		if t, ok := v.(string); ok {
			is.Title = t
		}
	}
	if v, ok := m["body"]; ok {
		if t, ok := v.(string); ok {
			is.Body = t
		}
	}
	is.cache = nil
	return response{status: 200, body: issueJSON(s.store, r, is)}
}

func (s *server) createComment(req *request, name, numStr string) response {
	n, ok := parseNum(numStr)
	if !ok {
		return jsonErr(404, "Not Found")
	}
	m, errResp := parseBody(req)
	if errResp != nil {
		return *errResp
	}
	body := bodyString(m, "body")
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	r := s.store.getRepo(name)
	if r == nil {
		return jsonErr(404, "Not Found")
	}
	is := r.IssuesByNum[n]
	if is == nil {
		return jsonErr(404, "Not Found")
	}
	id := r.NextCommentID
	r.NextCommentID++
	// Comments live on the issue struct's cache-busting side: store them in
	// a per-repo map keyed by issue number.
	if r.Comments == nil {
		r.Comments = make(map[int][]*Comment)
	}
	c := &Comment{ID: id, Body: body}
	r.Comments[n] = append(r.Comments[n], c)
	return response{status: 201, body: []byte(`{"id":` + strconv.Itoa(id) + `,"body":` + quote(body) + `,"user":{"login":` + quote(viewerLogin) + `}}`)}
}

func (s *server) listComments(req *request, name, numStr string) response {
	n, ok := parseNum(numStr)
	if !ok {
		return jsonErr(404, "Not Found")
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	r := s.store.getRepo(name)
	if r == nil {
		return jsonErr(404, "Not Found")
	}
	if r.IssuesByNum[n] == nil {
		return jsonErr(404, "Not Found")
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, c := range r.Comments[n] {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"id":` + strconv.Itoa(c.ID) + `,"body":` + quote(c.Body) + `,"user":{"login":` + quote(viewerLogin) + `}}`)
	}
	b.WriteByte(']')
	return response{status: 200, body: []byte(b.String())}
}

// ---------------------------------------------------------------------------
// pull requests

func (s *server) createPullREST(req *request, name string) response {
	m, errResp := parseBody(req)
	if errResp != nil {
		return *errResp
	}
	head := bodyString(m, "head")
	base := bodyString(m, "base")
	title := bodyString(m, "title")
	if head == "" || base == "" || title == "" {
		return jsonErr(422, "Validation Failed: head, base and title are required")
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	r := s.store.getRepo(name)
	if r == nil {
		return jsonErr(404, "Not Found")
	}
	p, errR := s.createPullLocked(r, title, bodyString(m, "body"), head, base)
	if errR != nil {
		return *errR
	}
	return response{status: 201, body: s.pullJSON(r, p)}
}

// createPullLocked opens a pull request; caller holds the write lock.
func (s *server) createPullLocked(r *Repo, title, body, head, base string) (*Pull, *response) {
	headSHA, ok := r.Refs["refs/heads/"+head]
	if !ok {
		r := jsonErr(422, "Validation Failed: head branch does not exist")
		return nil, &r
	}
	baseSHA, ok := r.Refs["refs/heads/"+base]
	if !ok {
		r := jsonErr(422, "Validation Failed: base branch does not exist")
		return nil, &r
	}
	for _, p := range r.Pulls {
		if p.State == "open" && p.HeadRef == head && p.BaseRef == base {
			r := jsonErr(422, "Validation Failed: a pull request already exists for this head and base")
			return nil, &r
		}
	}
	p := &Pull{
		ID:      s.store.nextIssueID,
		Number:  r.NextNum,
		Title:   title,
		Body:    body,
		State:   "open",
		HeadRef: head,
		BaseRef: base,
		HeadSHA: headSHA,
		BaseSHA: baseSHA,
		Seq:     s.store.nextSeq(),
	}
	s.store.nextIssueID++
	r.NextNum++
	r.Pulls = append(r.Pulls, p)
	r.PullsByNum[p.Number] = p
	return p, nil
}

func (s *server) listPulls(req *request, name string) response {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	r := s.store.getRepo(name)
	if r == nil {
		return jsonErr(404, "Not Found")
	}
	state, _ := queryVal(req.query, "state")
	if state == "" {
		state = "open"
	}
	perPage, page := pageParams(req)
	var filtered []*Pull
	for _, p := range r.Pulls {
		if state == "all" || p.State == state {
			filtered = append(filtered, p)
		}
	}
	total := len(filtered)
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, p := range filtered[start:end] {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(s.pullJSON(r, p))
	}
	b.WriteByte(']')
	return response{status: 200, body: []byte(b.String()), extra: s.paginate(req, total, perPage, page)}
}

func (s *server) getPullREST(req *request, name, numStr string) response {
	n, ok := parseNum(numStr)
	if !ok {
		return jsonErr(404, "Not Found")
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	r := s.store.getRepo(name)
	if r == nil {
		return jsonErr(404, "Not Found")
	}
	p := r.PullsByNum[n]
	if p == nil {
		return jsonErr(404, "Not Found")
	}
	return response{status: 200, body: s.pullJSON(r, p)}
}

// diffTrees compares two tree oids and returns changed paths with their
// before/after blob oids.
type treeChange struct {
	path   string
	oldOID string
	newOID string
}

func diffTrees(r *Repo, oldTree, newTree string) []treeChange {
	oldFiles := flattenTree(r, oldTree)
	newFiles := flattenTree(r, newTree)
	seen := make(map[string]bool)
	var out []treeChange
	for path, e := range newFiles {
		seen[path] = true
		if old, ok := oldFiles[path]; !ok {
			out = append(out, treeChange{path, "", e[1]})
		} else if old[1] != e[1] {
			out = append(out, treeChange{path, old[1], e[1]})
		}
	}
	for path, e := range oldFiles {
		if !seen[path] {
			out = append(out, treeChange{path, e[1], ""})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

// countLineDiff counts added/removed lines between two blobs (multiset
// difference over lines).
func countLineDiff(r *Repo, oldOID, newOID string) (add, del int) {
	var oldLines, newLines []string
	if oldOID != "" {
		if o := r.Objects[oldOID]; o != nil {
			oldLines = strings.Split(string(o.data), "\n")
		}
	}
	if newOID != "" {
		if o := r.Objects[newOID]; o != nil {
			newLines = strings.Split(string(o.data), "\n")
		}
	}
	counts := make(map[string]int)
	for _, l := range oldLines {
		counts[l]++
	}
	for _, l := range newLines {
		if counts[l] > 0 {
			counts[l]--
		} else {
			add++
		}
	}
	for _, c := range counts {
		del += c
	}
	return add, del
}

func (s *server) listPullFiles(req *request, name, numStr string) response {
	n, ok := parseNum(numStr)
	if !ok {
		return jsonErr(404, "Not Found")
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	r := s.store.getRepo(name)
	if r == nil {
		return jsonErr(404, "Not Found")
	}
	p := r.PullsByNum[n]
	if p == nil {
		return jsonErr(404, "Not Found")
	}
	if p.filesCache != nil {
		return response{status: 200, body: p.filesCache}
	}
	body := s.pullFilesJSON(r, p)
	p.filesCache = body
	return response{status: 200, body: body}
}

func (s *server) pullFilesJSON(r *Repo, p *Pull) []byte {
	baseObj := r.Objects[p.BaseSHA]
	headObj := r.Objects[p.HeadSHA]
	var changes []treeChange
	if baseObj != nil && headObj != nil {
		changes = diffTrees(r, parseCommit(baseObj.data).tree, parseCommit(headObj.data).tree)
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, ch := range changes {
		if i > 0 {
			b.WriteByte(',')
		}
		status := "modified"
		if ch.oldOID == "" {
			status = "added"
		} else if ch.newOID == "" {
			status = "removed"
		}
		add, del := countLineDiff(r, ch.oldOID, ch.newOID)
		b.WriteString(`{"filename":`)
		b.WriteString(quote(ch.path))
		b.WriteString(`,"status":`)
		b.WriteString(quote(status))
		b.WriteString(`,"additions":`)
		b.WriteString(strconv.Itoa(add))
		b.WriteString(`,"deletions":`)
		b.WriteString(strconv.Itoa(del))
		b.WriteString(`,"changes":`)
		b.WriteString(strconv.Itoa(add + del))
		b.WriteString(`}`)
	}
	b.WriteByte(']')
	return []byte(b.String())
}

// pullConflict reports whether merging p now would conflict: a path the
// head changed since the pull opened that the base also changed, to
// different bytes. Caller holds the write lock (merge path) or read lock.
func (s *server) pullConflict(r *Repo, p *Pull) bool {
	baseObj := r.Objects[p.BaseSHA]
	headObj := r.Objects[p.HeadSHA]
	baseTipOID := r.Refs["refs/heads/"+p.BaseRef]
	if baseObj == nil || headObj == nil || baseTipOID == "" {
		return false
	}
	baseTipObj := r.Objects[baseTipOID]
	if baseTipObj == nil {
		return false
	}
	headChanges := diffTrees(r, parseCommit(baseObj.data).tree, parseCommit(headObj.data).tree)
	baseChanges := diffTrees(r, parseCommit(baseObj.data).tree, parseCommit(baseTipObj.data).tree)
	baseMap := make(map[string]string, len(baseChanges))
	for _, ch := range baseChanges {
		baseMap[ch.path] = ch.newOID
	}
	for _, ch := range headChanges {
		if baseNew, ok := baseMap[ch.path]; ok && baseNew != ch.newOID {
			return true
		}
	}
	return false
}

// pullMergeable is the GraphQL mergeable verdict for a pull request.
func (s *server) pullMergeable(r *Repo, p *Pull) bool {
	if p.Merged {
		return true
	}
	if p.mergeable != nil {
		return *p.mergeable
	}
	m := !s.pullConflict(r, p)
	p.mergeable = &m
	return m
}

func (s *server) clearPullCaches(r *Repo) {
	for _, p := range r.Pulls {
		p.cache = nil
		p.filesCache = nil
		p.mergeable = nil
	}
}

func (s *server) mergePullREST(req *request, name, numStr string) response {
	n, ok := parseNum(numStr)
	if !ok {
		return jsonErr(404, "Not Found")
	}
	m, errResp := parseBody(req)
	if errResp != nil {
		return *errResp
	}
	_ = m
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	r := s.store.getRepo(name)
	if r == nil {
		return jsonErr(404, "Not Found")
	}
	p := r.PullsByNum[n]
	if p == nil {
		return jsonErr(404, "Not Found")
	}
	mergeSHA, errR := s.mergePullLocked(r, p)
	if errR != nil {
		return *errR
	}
	body := `{"sha":` + quote(mergeSHA) + `,"merged":true,"message":"Merge pull request #` + strconv.Itoa(p.Number) + ` from ` + viewerLogin + `/` + p.HeadRef + `"}`
	return response{status: 200, body: []byte(body)}
}

// mergePullLocked squash-merges a pull request; caller holds the write lock.
func (s *server) mergePullLocked(r *Repo, p *Pull) (string, *response) {
	if p.Merged || p.State != "open" {
		r := jsonErr(405, "Conflict: pull request is already merged or closed")
		return "", &r
	}
	baseTipOID := r.Refs["refs/heads/"+p.BaseRef]
	headObj := r.Objects[p.HeadSHA]
	baseObj := r.Objects[p.BaseSHA]
	baseTipObj := r.Objects[baseTipOID]
	if headObj == nil || baseObj == nil || baseTipObj == nil {
		r := jsonErr(405, "Conflict: cannot merge")
		return "", &r
	}
	if s.pullConflict(r, p) {
		r := jsonErr(405, "Conflict: merge conflict")
		return "", &r
	}
	// Squash: base tip tree with the head's changes applied.
	files := flattenTree(r, parseCommit(baseTipObj.data).tree)
	headChanges := diffTrees(r, parseCommit(baseObj.data).tree, parseCommit(headObj.data).tree)
	for _, ch := range headChanges {
		if ch.newOID == "" {
			delete(files, ch.path)
		} else {
			files[ch.path] = [2]string{"100644", ch.newOID}
		}
	}
	treeOID := s.rebuildTree(r, files)
	epoch := commitBaseEpoch + s.store.nextCommitSeq()
	ident := identityLine(epoch)
	msg := "Merge pull request #" + strconv.Itoa(p.Number) + " from " + viewerLogin + "/" + p.HeadRef + "\n\n" + p.Title
	commitData := buildCommit(treeOID, []string{baseTipOID}, ident, ident, msg)
	mergeSHA := hashObject("commit", commitData)
	r.Objects[mergeSHA] = &gitObject{"commit", commitData}
	r.Refs["refs/heads/"+p.BaseRef] = mergeSHA
	r.dropPackCache()
	p.State = "closed"
	p.Merged = true
	p.MergedAt = isoTime(s.store.nextSeq())
	p.MergeSHA = mergeSHA
	s.clearPullCaches(r)
	return mergeSHA, nil
}
