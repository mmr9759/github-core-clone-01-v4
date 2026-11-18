// GraphQL endpoint (/api/graphql): a small string-based GraphQL engine
// written from the documented schema surface. It parses query/mutation
// documents (with variables, fragments, aliases and directives), resolves
// them against the same in-memory store as REST, and answers batched
// documents (a JSON array body) with one result per document, in order.
package main

import (
	"encoding/base64"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// global ids: opaque, stable, base64 of a typed key (never the raw number)

func gid(kind string, parts ...string) string {
	raw := kind + ":" + strings.Join(parts, ":")
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func ungid(id string) (string, []string, bool) {
	b, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		b, err = base64.StdEncoding.DecodeString(id)
		if err != nil {
			return "", nil, false
		}
	}
	segs := strings.Split(string(b), ":")
	if len(segs) < 2 {
		return "", nil, false
	}
	return segs[0], segs[1:], true
}

func repoGID(r *Repo) string { return gid("repo", r.Name) }
func issueGID(r *Repo, is *Issue) string {
	return gid("issue", r.Name, strconv.Itoa(is.Number))
}
func pullGID(r *Repo, p *Pull) string {
	return gid("pull", r.Name, strconv.Itoa(p.Number))
}
func commentGID(r *Repo, n, id int) string {
	return gid("comment", r.Name, strconv.Itoa(n), strconv.Itoa(id))
}

// ---------------------------------------------------------------------------
// GraphQL document parser

type gqlVarRef struct{ name string }

type gqlArg struct {
	name string
	val  any // string, int, float64, bool, nil, []any, map[string]any, gqlVarRef
}

type gqlField struct {
	name  string
	alias string
	args  []gqlArg
	sel   []*gqlField
}

type gqlOp struct {
	kind string // "query" or "mutation"
	name string
	sel  []*gqlField
}

type gqlParser struct {
	s    string
	pos  int
	frag map[string][]*gqlField
}

func (p *gqlParser) skipSpace() {
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			p.pos++
		} else if c == '#' {
			for p.pos < len(p.s) && p.s[p.pos] != '\n' {
				p.pos++
			}
		} else {
			break
		}
	}
}

func (p *gqlParser) peek() byte {
	p.skipSpace()
	if p.pos >= len(p.s) {
		return 0
	}
	return p.s[p.pos]
}

func (p *gqlParser) readName() string {
	p.skipSpace()
	start := p.pos
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			p.pos++
		} else {
			break
		}
	}
	return p.s[start:p.pos]
}

func (p *gqlParser) expect(c byte) bool {
	p.skipSpace()
	if p.pos < len(p.s) && p.s[p.pos] == c {
		p.pos++
		return true
	}
	return false
}

// readString parses a double-quoted GraphQL string literal.
func (p *gqlParser) readString() (string, bool) {
	p.skipSpace()
	if p.pos >= len(p.s) || p.s[p.pos] != '"' {
		return "", false
	}
	p.pos++
	var b strings.Builder
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		p.pos++
		if c == '"' {
			return b.String(), true
		}
		if c == '\\' && p.pos < len(p.s) {
			e := p.s[p.pos]
			p.pos++
			switch e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case '/':
				b.WriteByte('/')
			case '\\':
				b.WriteByte('\\')
			case '"':
				b.WriteByte('"')
			case 'u':
				if p.pos+4 <= len(p.s) {
					if n, err := strconv.ParseUint(p.s[p.pos:p.pos+4], 16, 32); err == nil {
						b.WriteRune(rune(n))
						p.pos += 4
					}
				}
			default:
				b.WriteByte(e)
			}
			continue
		}
		b.WriteByte(c)
	}
	return "", false
}

func (p *gqlParser) readValue() any {
	p.skipSpace()
	if p.pos >= len(p.s) {
		return nil
	}
	switch c := p.s[p.pos]; {
	case c == '"':
		v, _ := p.readString()
		return v
	case c == '$':
		p.pos++
		return gqlVarRef{p.readName()}
	case c == '[':
		p.pos++
		var out []any
		for {
			p.skipSpace()
			if p.pos < len(p.s) && p.s[p.pos] == ']' {
				p.pos++
				return out
			}
			out = append(out, p.readValue())
		}
	case c == '{':
		p.pos++
		out := map[string]any{}
		for {
			p.skipSpace()
			if p.pos < len(p.s) && p.s[p.pos] == '}' {
				p.pos++
				return out
			}
			k := p.readName()
			p.expect(':')
			out[k] = p.readValue()
		}
	case c == '-' || (c >= '0' && c <= '9'):
		start := p.pos
		for p.pos < len(p.s) {
			d := p.s[p.pos]
			if (d >= '0' && d <= '9') || d == '-' || d == '+' || d == '.' || d == 'e' || d == 'E' {
				p.pos++
			} else {
				break
			}
		}
		tok := p.s[start:p.pos]
		if strings.ContainsAny(tok, ".eE") {
			if f, err := strconv.ParseFloat(tok, 64); err == nil {
				return f
			}
			return tok
		}
		if n, err := strconv.Atoi(tok); err == nil {
			return n
		}
		return tok
	default:
		name := p.readName()
		switch name {
		case "true":
			return true
		case "false":
			return false
		case "null":
			return nil
		default:
			return name // enum value: keep as string
		}
	}
}

// parseDirectives consumes @skip/@include and reports whether the field
// survives (variables already resolved by the caller-supplied lookup).
func (p *gqlParser) parseDirectives(resolve func(string) any) bool {
	keep := true
	for {
		p.skipSpace()
		if p.pos >= len(p.s) || p.s[p.pos] != '@' {
			return keep
		}
		p.pos++
		name := p.readName()
		var ifVal any
		if p.peek() == '(' {
			p.pos++
			for {
				p.skipSpace()
				if p.pos < len(p.s) && p.s[p.pos] == ')' {
					p.pos++
					break
				}
				an := p.readName()
				p.expect(':')
				av := p.readValue()
				if an == "if" {
					ifVal = av
				}
			}
		}
		if vr, ok := ifVal.(gqlVarRef); ok {
			ifVal = resolve(vr.name)
		}
		cond, _ := ifVal.(bool)
		if name == "skip" && cond {
			keep = false
		}
		if name == "include" && !cond {
			keep = false
		}
	}
}

func (p *gqlParser) parseArgs(resolve func(string) any) []gqlArg {
	var out []gqlArg
	if !p.expect('(') {
		return out
	}
	for {
		p.skipSpace()
		if p.pos < len(p.s) && p.s[p.pos] == ')' {
			p.pos++
			return out
		}
		name := p.readName()
		p.expect(':')
		out = append(out, gqlArg{name, p.readValue()})
	}
}

func (p *gqlParser) parseSelectionSet(resolve func(string) any) []*gqlField {
	var out []*gqlField
	if !p.expect('{') {
		return out
	}
	for {
		p.skipSpace()
		if p.pos >= len(p.s) {
			return out
		}
		if p.s[p.pos] == '}' {
			p.pos++
			return out
		}
		if p.s[p.pos] == '.' && p.pos+2 < len(p.s) && p.s[p.pos+1] == '.' && p.s[p.pos+2] == '.' {
			p.pos += 3
			p.skipSpace()
			name := p.readName()
			if name == "on" {
				p.readName() // type condition
				out = append(out, p.parseSelectionSet(resolve)...)
			} else if name != "" {
				// named fragment spread: inline its fields
				if fields, ok := p.frag[name]; ok {
					out = append(out, fields...)
				}
				// directives after a spread are ignored
				p.parseDirectives(resolve)
			} else {
				out = append(out, p.parseSelectionSet(resolve)...)
			}
			continue
		}
		name := p.readName()
		if name == "" {
			p.pos++
			continue
		}
		f := &gqlField{name: name, alias: name}
		p.skipSpace()
		if p.pos < len(p.s) && p.s[p.pos] == ':' {
			p.pos++
			f.name = p.readName()
		}
		f.args = p.parseArgs(resolve)
		if !p.parseDirectives(resolve) {
			// field skipped: still consume its selection set if present
			p.skipSpace()
			if p.pos < len(p.s) && p.s[p.pos] == '{' {
				p.parseSelectionSet(resolve)
			}
			continue
		}
		p.skipSpace()
		if p.pos < len(p.s) && p.s[p.pos] == '{' {
			f.sel = p.parseSelectionSet(resolve)
		}
		out = append(out, f)
	}
}

// parseDocument returns the operations in a GraphQL document string.
// Fragment definitions are collected first so spreads resolve anywhere.
func parseDocument(src string) []*gqlOp {
	p := &gqlParser{s: src, frag: map[string][]*gqlField{}}
	// First pass: collect fragment definitions.
	for {
		p.skipSpace()
		if p.pos >= len(p.s) {
			break
		}
		save := p.pos
		kw := p.readName()
		if kw == "fragment" {
			name := p.readName()
			if p.readName() == "on" {
				p.readName()
				p.frag[name] = p.parseSelectionSet(nil)
			}
			continue
		}
		p.pos = save
		break
	}
	var ops []*gqlOp
	for {
		p.skipSpace()
		if p.pos >= len(p.s) {
			break
		}
		if p.s[p.pos] == '{' {
			ops = append(ops, &gqlOp{kind: "query", sel: p.parseSelectionSet(nil)})
			continue
		}
		kw := p.readName()
		if kw != "query" && kw != "mutation" && kw != "subscription" {
			// stray token: skip it
			if kw == "" {
				p.pos++
			}
			continue
		}
		op := &gqlOp{kind: kw}
		p.skipSpace()
		if p.pos < len(p.s) && p.s[p.pos] != '(' && p.s[p.pos] != '{' {
			op.name = p.readName()
		}
		// variable definitions: skip "(...)" block
		if p.peek() == '(' {
			depth := 0
			for p.pos < len(p.s) {
				c := p.s[p.pos]
				p.pos++
				if c == '(' {
					depth++
				} else if c == ')' {
					depth--
					if depth == 0 {
						break
					}
				}
			}
		}
		op.sel = p.parseSelectionSet(nil)
		ops = append(ops, op)
	}
	return ops
}

// resolveArg looks an argument up by name, resolving $variables.
func resolveArg(args []gqlArg, name string, vars map[string]any) (any, bool) {
	for _, a := range args {
		if a.name != name {
			continue
		}
		return resolveVal(a.val, vars), true
	}
	return nil, false
}

func resolveVal(v any, vars map[string]any) any {
	switch t := v.(type) {
	case gqlVarRef:
		if vars != nil {
			if val, ok := vars[t.name]; ok {
				return val
			}
		}
		return nil
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = resolveVal(e, vars)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, e := range t {
			out[k] = resolveVal(e, vars)
		}
		return out
	default:
		return v
	}
}

func argStr(args []gqlArg, vars map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := resolveArg(args, k, vars); ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return ""
}

func argInt(args []gqlArg, vars map[string]any, keys ...string) (int, bool) {
	for _, k := range keys {
		if v, ok := resolveArg(args, k, vars); ok {
			switch n := v.(type) {
			case int:
				return n, true
			case float64:
				return int(n), true
			case json.Number:
				if i, err := n.Int64(); err == nil {
					return int(i), true
				}
			}
		}
	}
	return 0, false
}

func argMap(args []gqlArg, vars map[string]any, key string) map[string]any {
	if v, ok := resolveArg(args, key, vars); ok {
		if m, ok := v.(map[string]any); ok {
			return m
		}
	}
	return nil
}

func argStrList(args []gqlArg, vars map[string]any, key string) []string {
	v, ok := resolveArg(args, key, vars)
	if !ok {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		if s, ok := v.(string); ok {
			return []string{s}
		}
		return nil
	}
	var out []string
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// handler

func (s *server) graphql(req *request) response {
	trimmed := strings.TrimSpace(string(req.body))
	if trimmed == "" {
		return jsonErr(400, "Invalid JSON body")
	}
	var docs []map[string]any
	if strings.HasPrefix(trimmed, "[") {
		var arr []map[string]any
		dec := json.NewDecoder(strings.NewReader(trimmed))
		dec.UseNumber()
		if err := dec.Decode(&arr); err != nil {
			return jsonErr(400, "Invalid JSON body")
		}
		docs = arr
	} else {
		var single map[string]any
		dec := json.NewDecoder(strings.NewReader(trimmed))
		dec.UseNumber()
		if err := dec.Decode(&single); err != nil {
			return jsonErr(400, "Invalid JSON body")
		}
		docs = []map[string]any{single}
	}
	var results []string
	for _, doc := range docs {
		results = append(results, s.execGraphQLDoc(doc))
	}
	if len(results) == 1 {
		return response{status: 200, body: []byte(results[0])}
	}
	return response{status: 200, body: []byte("[" + strings.Join(results, ",") + "]")}
}

func (s *server) execGraphQLDoc(doc map[string]any) string {
	query, _ := doc["query"].(string)
	var vars map[string]any
	if v, ok := doc["variables"].(map[string]any); ok {
		vars = v
	}
	opName, _ := doc["operationName"].(string)
	ops := parseDocument(query)
	if len(ops) == 0 {
		return `{"errors":[{"message":"No operation found in document"}]}`
	}
	op := ops[0]
	if opName != "" {
		for _, o := range ops {
			if o.name == opName {
				op = o
				break
			}
		}
	}
	if op.kind == "mutation" {
		s.store.mu.Lock()
		defer s.store.mu.Unlock()
		return s.execMutation(op, vars)
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	return s.execQuery(op, vars)
}

// ---------------------------------------------------------------------------
// resolved nodes and field execution

type gqlFileNode struct {
	path                 string
	additions, deletions int
	status               string // added, modified, removed
}

type gqlConn struct {
	items                  []gqlNode
	total                  int
	hasNext                bool
	startCursor, endCursor string
}

type gqlNode struct {
	typ     string
	repo    *Repo
	issue   *Issue
	pull    *Pull
	comment *Comment
	label   *Label
	file    *gqlFileNode
	conn    *gqlConn
	created string // comment creation time
	cursor  string // edge cursor
}

func (s *server) execQuery(op *gqlOp, vars map[string]any) string {
	var errs []string
	body := s.execFields(op.sel, gqlNode{typ: "Query"}, vars, &errs)
	return s.gqlResponse(body, errs)
}

func (s *server) execMutation(op *gqlOp, vars map[string]any) string {
	var errs []string
	var b strings.Builder
	b.WriteByte('{')
	for i, f := range op.sel {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(quote(f.alias))
		b.WriteByte(':')
		val, ok := s.execMutationField(f, vars, &errs)
		if !ok {
			b.WriteString("null")
			continue
		}
		b.WriteString(val)
	}
	b.WriteByte('}')
	return s.gqlResponse(b.String(), errs)
}

func (s *server) gqlResponse(data string, errs []string) string {
	var b strings.Builder
	b.WriteString(`{"data":`)
	b.WriteString(data)
	if len(errs) > 0 {
		b.WriteString(`,"errors":[`)
		for i, e := range errs {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"message":`)
			b.WriteString(quote(e))
			b.WriteString(`}`)
		}
		b.WriteByte(']')
	}
	b.WriteByte('}')
	return b.String()
}

// execFields renders one object's selection set.
func (s *server) execFields(fields []*gqlField, n gqlNode, vars map[string]any, errs *[]string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(quote(f.alias))
		b.WriteByte(':')
		if f.name == "__typename" {
			b.WriteString(quote(n.typ))
			continue
		}
		val, ok := s.resolveField(n, f, vars, errs)
		if !ok {
			*errs = append(*errs, "Cannot query field \""+f.name+"\" on type \""+n.typ+"\".")
			b.WriteString("null")
			continue
		}
		b.WriteString(val)
	}
	b.WriteByte('}')
	return b.String()
}

func issueStateGQL(is *Issue) string {
	if is.State == "closed" {
		return "CLOSED"
	}
	return "OPEN"
}

func pullStateGQL(p *Pull) string {
	if p.Merged {
		return "MERGED"
	}
	if p.State == "closed" {
		return "CLOSED"
	}
	return "OPEN"
}

func cursorFor(n int) string {
	return base64.StdEncoding.EncodeToString([]byte("c:" + strconv.Itoa(n)))
}

func numberAfter(after string) int {
	if after == "" {
		return -1
	}
	b, err := base64.StdEncoding.DecodeString(after)
	if err != nil {
		b, err = base64.RawURLEncoding.DecodeString(after)
		if err != nil {
			return -1
		}
	}
	if !strings.HasPrefix(string(b), "c:") {
		return -1
	}
	n, err := strconv.Atoi(string(b)[2:])
	if err != nil {
		return -1
	}
	return n
}

// paginateConn slices an ordered list by first/after and builds a connection.
func paginateConn(items []gqlNode, nums []int, f *gqlField, vars map[string]any) *gqlConn {
	first, ok := argInt(f.args, vars, "first")
	if !ok || first <= 0 {
		first = 30
	}
	after, _ := resolveArg(f.args, "after", vars)
	afterStr, _ := after.(string)
	afterNum := numberAfter(afterStr)
	start := 0
	if afterNum >= 0 {
		for i, n := range nums {
			if n == afterNum {
				start = i + 1
				break
			}
		}
	}
	if start > len(items) {
		start = len(items)
	}
	end := start + first
	hasNext := false
	if end < len(items) {
		hasNext = true
	} else {
		end = len(items)
	}
	c := &gqlConn{total: len(items), hasNext: hasNext, items: items[start:end]}
	if len(c.items) > 0 {
		c.startCursor = cursorFor(nums[start])
		c.endCursor = cursorFor(nums[end-1])
	}
	return c
}

func (s *server) issueConn(r *Repo, f *gqlField, vars map[string]any) *gqlConn {
	states := argStrList(f.args, vars, "states")
	want := map[string]bool{}
	for _, st := range states {
		want[strings.ToUpper(st)] = true
	}
	ob := argMap(f.args, vars, "orderBy")
	dir := "DESC"
	if ob != nil {
		if d, ok := ob["direction"].(string); ok {
			dir = strings.ToUpper(d)
		}
	}
	var filtered []*Issue
	for _, is := range r.Issues {
		if len(want) > 0 && !want[issueStateGQL(is)] {
			continue
		}
		filtered = append(filtered, is)
	}
	if dir == "DESC" {
		for i, j := 0, len(filtered)-1; i < j; i, j = i+1, j-1 {
			filtered[i], filtered[j] = filtered[j], filtered[i]
		}
	}
	items := make([]gqlNode, len(filtered))
	nums := make([]int, len(filtered))
	for i, is := range filtered {
		items[i] = gqlNode{typ: "Issue", repo: r, issue: is}
		nums[i] = is.Number
	}
	return paginateConn(items, nums, f, vars)
}

func (s *server) pullConn(r *Repo, f *gqlField, vars map[string]any) *gqlConn {
	states := argStrList(f.args, vars, "states")
	want := map[string]bool{}
	for _, st := range states {
		want[strings.ToUpper(st)] = true
	}
	ob := argMap(f.args, vars, "orderBy")
	dir := "DESC"
	if ob != nil {
		if d, ok := ob["direction"].(string); ok {
			dir = strings.ToUpper(d)
		}
	}
	var filtered []*Pull
	for _, p := range r.Pulls {
		if len(want) > 0 && !want[pullStateGQL(p)] {
			continue
		}
		filtered = append(filtered, p)
	}
	if dir == "DESC" {
		for i, j := 0, len(filtered)-1; i < j; i, j = i+1, j-1 {
			filtered[i], filtered[j] = filtered[j], filtered[i]
		}
	}
	items := make([]gqlNode, len(filtered))
	nums := make([]int, len(filtered))
	for i, p := range filtered {
		items[i] = gqlNode{typ: "PullRequest", repo: r, pull: p}
		nums[i] = p.Number
	}
	return paginateConn(items, nums, f, vars)
}

// connJSON renders a connection object for its selection set.
func (s *server) connJSON(c *gqlConn, edgeTyp string, f *gqlField, vars map[string]any, errs *[]string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, sf := range f.sel {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(quote(sf.alias))
		b.WriteByte(':')
		switch sf.name {
		case "__typename":
			b.WriteString(quote(edgeTyp + "Connection"))
		case "totalCount":
			b.WriteString(strconv.Itoa(c.total))
		case "nodes":
			b.WriteByte('[')
			for j, item := range c.items {
				if j > 0 {
					b.WriteByte(',')
				}
				b.WriteString(s.execFields(sf.sel, item, vars, errs))
			}
			b.WriteByte(']')
		case "edges":
			b.WriteByte('[')
			for j, item := range c.items {
				if j > 0 {
					b.WriteByte(',')
				}
				edge := item
				edge.typ = edgeTyp + "Edge"
				edge.cursor = cursorFor(nodeNumber(item))
				b.WriteString(s.execFields(sf.sel, edge, vars, errs))
			}
			b.WriteByte(']')
		case "pageInfo":
			b.WriteString(s.execFields(sf.sel, gqlNode{typ: "PageInfo", conn: c}, vars, errs))
		default:
			*errs = append(*errs, "Cannot query field \""+sf.name+"\" on type \""+edgeTyp+"Connection\".")
			b.WriteString("null")
		}
	}
	b.WriteByte('}')
	return b.String()
}

func nodeNumber(n gqlNode) int {
	if n.issue != nil {
		return n.issue.Number
	}
	if n.pull != nil {
		return n.pull.Number
	}
	return 0
}

func userNode() gqlNode { return gqlNode{typ: "User"} }

func (s *server) lookupGID(id string) (string, *Repo, *Issue, *Pull) {
	kind, parts, ok := ungid(id)
	if !ok {
		return "", nil, nil, nil
	}
	switch kind {
	case "repo":
		if len(parts) >= 1 {
			return kind, s.store.getRepo(parts[0]), nil, nil
		}
	case "issue":
		if len(parts) >= 2 {
			r := s.store.getRepo(parts[0])
			if r != nil {
				if n, err := strconv.Atoi(parts[1]); err == nil {
					return kind, r, r.IssuesByNum[n], nil
				}
			}
		}
	case "pull":
		if len(parts) >= 2 {
			r := s.store.getRepo(parts[0])
			if r != nil {
				if n, err := strconv.Atoi(parts[1]); err == nil {
					return kind, r, nil, r.PullsByNum[n]
				}
			}
		}
	}
	return kind, nil, nil, nil
}

func (s *server) resolveField(n gqlNode, f *gqlField, vars map[string]any, errs *[]string) (string, bool) {
	switch n.typ {
	case "Query":
		switch f.name {
		case "viewer":
			return s.execFields(f.sel, userNode(), vars, errs), true
		case "repository":
			owner := argStr(f.args, vars, "owner")
			name := argStr(f.args, vars, "name")
			if owner != viewerLogin {
				return "null", true
			}
			r := s.store.getRepo(name)
			if r == nil {
				return "null", true
			}
			return s.execFields(f.sel, gqlNode{typ: "Repository", repo: r}, vars, errs), true
		case "node":
			id := argStr(f.args, vars, "id")
			kind, r, is, p := s.lookupGID(id)
			switch kind {
			case "repo":
				if r == nil {
					return "null", true
				}
				return s.execFields(f.sel, gqlNode{typ: "Repository", repo: r}, vars, errs), true
			case "issue":
				if is == nil {
					return "null", true
				}
				return s.execFields(f.sel, gqlNode{typ: "Issue", repo: r, issue: is}, vars, errs), true
			case "pull":
				if p == nil {
					return "null", true
				}
				return s.execFields(f.sel, gqlNode{typ: "PullRequest", repo: r, pull: p}, vars, errs), true
			}
			return "null", true
		case "__type":
			name := argStr(f.args, vars, "name")
			return gqlTypeJSON(name, f.sel, vars, errs), true
		case "__schema":
			return s.execFields(f.sel, gqlNode{typ: "__Schema"}, vars, errs), true
		}
		return "", false

	case "__Schema":
		switch f.name {
		case "queryType":
			return s.execFields(f.sel, gqlNode{typ: "__Type", created: "Query"}, vars, errs), true
		case "types":
			var names []string
			for name := range gqlTypeFields {
				names = append(names, name)
			}
			sort.Strings(names)
			names = append(names, "SearchType", "Query", "Mutation")
			var b strings.Builder
			b.WriteByte('[')
			for i, name := range names {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString(s.execFields(f.sel, gqlNode{typ: "__Type", created: name}, vars, errs))
			}
			b.WriteByte(']')
			return b.String(), true
		}
		return "", false

	case "Repository":
		r := n.repo
		switch f.name {
		case "id":
			return quote(repoGID(r)), true
		case "databaseId":
			return strconv.Itoa(r.DBID), true
		case "name":
			return quote(r.Name), true
		case "nameWithOwner":
			return quote(viewerLogin + "/" + r.Name), true
		case "description":
			if r.Description == "" {
				return "null", true
			}
			return quote(r.Description), true
		case "owner":
			return s.execFields(f.sel, userNode(), vars, errs), true
		case "defaultBranchRef":
			return s.execFields(f.sel, gqlNode{typ: "Ref", created: r.DefaultBranch}, vars, errs), true
		case "viewerPermission":
			return quote("ADMIN"), true
		case "hasIssuesEnabled":
			return "true", true
		case "url":
			return quote("https://example.invalid/" + viewerLogin + "/" + r.Name), true
		case "issue":
			num, ok := argInt(f.args, vars, "number")
			if !ok {
				return "null", true
			}
			is := r.IssuesByNum[num]
			if is == nil {
				return "null", true
			}
			return s.execFields(f.sel, gqlNode{typ: "Issue", repo: r, issue: is}, vars, errs), true
		case "issues":
			return s.connJSON(s.issueConn(r, f, vars), "Issue", f, vars, errs), true
		case "pullRequest":
			num, ok := argInt(f.args, vars, "number")
			if !ok {
				return "null", true
			}
			p := r.PullsByNum[num]
			if p == nil {
				return "null", true
			}
			return s.execFields(f.sel, gqlNode{typ: "PullRequest", repo: r, pull: p}, vars, errs), true
		case "pullRequests":
			return s.connJSON(s.pullConn(r, f, vars), "PullRequest", f, vars, errs), true
		}
		return "", false

	case "Issue":
		is := n.issue
		r := n.repo
		switch f.name {
		case "id":
			return quote(issueGID(r, is)), true
		case "databaseId":
			return strconv.Itoa(is.ID), true
		case "number":
			return strconv.Itoa(is.Number), true
		case "title":
			return quote(is.Title), true
		case "body":
			return quote(is.Body), true
		case "state":
			return quote(issueStateGQL(is)), true
		case "author":
			return s.execFields(f.sel, userNode(), vars, errs), true
		case "createdAt", "updatedAt":
			return quote(isoTime(is.Seq)), true
		case "url":
			return quote("https://example.invalid/" + viewerLogin + "/" + r.Name + "/issues/" + strconv.Itoa(is.Number)), true
		case "repository":
			return s.execFields(f.sel, gqlNode{typ: "Repository", repo: r}, vars, errs), true
		case "labels":
			items := make([]gqlNode, len(is.Labels))
			nums := make([]int, len(is.Labels))
			for i, l := range is.Labels {
				items[i] = gqlNode{typ: "Label", label: l}
				nums[i] = i + 1
			}
			c := paginateConn(items, nums, f, vars)
			c.total = len(is.Labels)
			return s.connJSON(c, "Label", f, vars, errs), true
		case "comments":
			list := r.Comments[is.Number]
			items := make([]gqlNode, len(list))
			nums := make([]int, len(list))
			for i, c := range list {
				items[i] = gqlNode{typ: "IssueComment", repo: r, issue: is, comment: c, created: isoTime(is.Seq)}
				nums[i] = c.ID
			}
			cc := paginateConn(items, nums, f, vars)
			cc.total = len(list)
			return s.connJSON(cc, "IssueComment", f, vars, errs), true
		}
		return "", false

	case "PullRequest":
		p := n.pull
		r := n.repo
		switch f.name {
		case "id":
			return quote(pullGID(r, p)), true
		case "databaseId":
			return strconv.Itoa(p.ID), true
		case "number":
			return strconv.Itoa(p.Number), true
		case "title":
			return quote(p.Title), true
		case "body":
			return quote(p.Body), true
		case "state":
			return quote(pullStateGQL(p)), true
		case "headRefName":
			return quote(p.HeadRef), true
		case "baseRefName":
			return quote(p.BaseRef), true
		case "headRefOid":
			return quote(p.HeadSHA), true
		case "baseRefOid":
			return quote(p.BaseSHA), true
		case "headRef":
			return s.execFields(f.sel, gqlNode{typ: "Ref", created: p.HeadRef}, vars, errs), true
		case "baseRef":
			return s.execFields(f.sel, gqlNode{typ: "Ref", created: p.BaseRef}, vars, errs), true
		case "mergeable":
			// MergeableState enum: MERGEABLE / CONFLICTING / UNKNOWN.
			if s.pullMergeable(r, p) {
				return quote("MERGEABLE"), true
			}
			return quote("CONFLICTING"), true
		case "merged":
			if p.Merged {
				return "true", true
			}
			return "false", true
		case "author":
			return s.execFields(f.sel, userNode(), vars, errs), true
		case "createdAt", "updatedAt":
			return quote(isoTime(p.Seq)), true
		case "url":
			return quote("https://example.invalid/" + viewerLogin + "/" + r.Name + "/pull/" + strconv.Itoa(p.Number)), true
		case "repository":
			return s.execFields(f.sel, gqlNode{typ: "Repository", repo: r}, vars, errs), true
		case "files":
			baseObj := r.Objects[p.BaseSHA]
			headObj := r.Objects[p.HeadSHA]
			var changes []treeChange
			if baseObj != nil && headObj != nil {
				changes = diffTrees(r, parseCommit(baseObj.data).tree, parseCommit(headObj.data).tree)
			}
			items := make([]gqlNode, len(changes))
			nums := make([]int, len(changes))
			for i, ch := range changes {
				status := "modified"
				if ch.oldOID == "" {
					status = "added"
				} else if ch.newOID == "" {
					status = "removed"
				}
				add, del := countLineDiff(r, ch.oldOID, ch.newOID)
				items[i] = gqlNode{typ: "PullRequestChangedFile", file: &gqlFileNode{path: ch.path, additions: add, deletions: del, status: status}}
				nums[i] = i + 1
			}
			c := paginateConn(items, nums, f, vars)
			c.total = len(changes)
			return s.connJSON(c, "PullRequestChangedFile", f, vars, errs), true
		}
		return "", false

	case "User":
		switch f.name {
		case "id":
			return quote(gid("user", viewerLogin)), true
		case "databaseId":
			return strconv.Itoa(viewerID), true
		case "login":
			return quote(viewerLogin), true
		case "name":
			return quote(viewerName), true
		case "email":
			return quote(viewerEmail), true
		}
		return "", false

	case "Ref":
		if f.name == "name" {
			return quote(n.created), true
		}
		return "", false

	case "Label":
		switch f.name {
		case "id":
			return quote(gid("label", n.label.Name)), true
		case "name":
			return quote(n.label.Name), true
		case "color":
			return quote(n.label.Color), true
		}
		return "", false

	case "IssueComment":
		switch f.name {
		case "id":
			return quote(commentGID(n.repo, n.issue.Number, n.comment.ID)), true
		case "body":
			return quote(n.comment.Body), true
		case "author":
			return s.execFields(f.sel, userNode(), vars, errs), true
		case "createdAt":
			return quote(n.created), true
		case "url":
			return quote("https://example.invalid/" + viewerLogin + "/" + n.repo.Name + "/issues/" + strconv.Itoa(n.issue.Number) + "#issuecomment-" + strconv.Itoa(n.comment.ID)), true
		}
		return "", false

	case "PullRequestChangedFile":
		switch f.name {
		case "path":
			return quote(n.file.path), true
		case "additions":
			return strconv.Itoa(n.file.additions), true
		case "deletions":
			return strconv.Itoa(n.file.deletions), true
		case "changeType":
			switch n.file.status {
			case "added":
				return quote("ADDED"), true
			case "removed":
				return quote("REMOVED"), true
			default:
				return quote("MODIFIED"), true
			}
		}
		return "", false

	case "PageInfo":
		c := n.conn
		switch f.name {
		case "hasNextPage":
			if c.hasNext {
				return "true", true
			}
			return "false", true
		case "hasPreviousPage":
			return "false", true
		case "startCursor":
			if c.startCursor == "" {
				return "null", true
			}
			return quote(c.startCursor), true
		case "endCursor":
			if c.endCursor == "" {
				return "null", true
			}
			return quote(c.endCursor), true
		}
		return "", false

	case "IssueEdge", "PullRequestEdge", "LabelEdge", "IssueCommentEdge", "PullRequestChangedFileEdge":
		switch f.name {
		case "cursor":
			return quote(n.cursor), true
		case "node":
			inner := n
			inner.typ = strings.TrimSuffix(n.typ, "Edge")
			inner.cursor = ""
			return s.execFields(f.sel, inner, vars, errs), true
		}
		return "", false

	case "__Type":
		switch f.name {
		case "name":
			return quote(n.created), true
		case "kind":
			if n.created == "SearchType" || n.created == "IssueState" || n.created == "PullRequestState" {
				return quote("ENUM"), true
			}
			return quote("OBJECT"), true
		case "fields":
			return gqlFieldsJSON(n.created, f.sel, vars, errs), true
		case "enumValues":
			if n.created == "SearchType" {
				return `[{"name":"ISSUE"},{"name":"REPOSITORY"},{"name":"USER"}]`, true
			}
			return "[]", true
		}
		return "", false

	case "__EnumValue":
		if f.name == "name" {
			return quote(n.created), true
		}
		return "", false
	}
	return "", false
}

// ---------------------------------------------------------------------------
// introspection tables

var gqlTypeFields = map[string][]string{
	"Issue":        {"id", "databaseId", "number", "title", "body", "state", "author", "createdAt", "updatedAt", "url", "repository", "labels", "comments"},
	"PullRequest":  {"id", "databaseId", "number", "title", "body", "state", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "headRef", "baseRef", "mergeable", "merged", "files", "author", "createdAt", "updatedAt", "url", "repository"},
	"Repository":   {"id", "databaseId", "name", "nameWithOwner", "description", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequest", "pullRequests", "url"},
	"User":         {"id", "databaseId", "login", "name", "email"},
	"IssueComment": {"id", "body", "author", "createdAt", "url"},
	"Label":        {"id", "name", "color"},
	"Ref":          {"name"},
	"Query":        {"viewer", "repository", "node"},
	"Mutation":     {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"},
}

func gqlFieldsJSON(typeName string, sel []*gqlField, vars map[string]any, errs *[]string) string {
	fields, ok := gqlTypeFields[typeName]
	if !ok {
		return "null"
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, name := range fields {
		if i > 0 {
			b.WriteByte(',')
		}
		// render each __Field with only the requested sub-selection
		var fb strings.Builder
		fb.WriteByte('{')
		for j, sf := range sel {
			if j > 0 {
				fb.WriteByte(',')
			}
			fb.WriteString(quote(sf.alias))
			fb.WriteByte(':')
			switch sf.name {
			case "name":
				fb.WriteString(quote(name))
			case "type":
				fb.WriteString(`{"name":"String","kind":"SCALAR"}`)
			case "description":
				fb.WriteString("null")
			default:
				fb.WriteString("null")
			}
		}
		fb.WriteByte('}')
		b.WriteString(fb.String())
	}
	b.WriteByte(']')
	return b.String()
}

func gqlTypeJSON(typeName string, sel []*gqlField, vars map[string]any, errs *[]string) string {
	if typeName != "SearchType" {
		if _, ok := gqlTypeFields[typeName]; !ok {
			return "null"
		}
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, f := range sel {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(quote(f.alias))
		b.WriteByte(':')
		switch f.name {
		case "name":
			b.WriteString(quote(typeName))
		case "kind":
			if typeName == "SearchType" {
				b.WriteString(`"ENUM"`)
			} else {
				b.WriteString(`"OBJECT"`)
			}
		case "fields":
			b.WriteString(gqlFieldsJSON(typeName, f.sel, vars, errs))
		case "enumValues":
			if typeName == "SearchType" {
				b.WriteString(`[{"name":"ISSUE"},{"name":"REPOSITORY"},{"name":"USER"}]`)
			} else {
				b.WriteString("[]")
			}
		case "description":
			b.WriteString("null")
		default:
			b.WriteString("null")
		}
	}
	b.WriteByte('}')
	return b.String()
}

// ---------------------------------------------------------------------------
// mutations (caller holds the store write lock)

func (s *server) execMutationField(f *gqlField, vars map[string]any, errs *[]string) (string, bool) {
	input := argMap(f.args, vars, "input")
	// also allow flat arguments (no input wrapper)
	flat := func(key string) string {
		if input != nil {
			if v, ok := input[key].(string); ok {
				return v
			}
		}
		return argStr(f.args, vars, key)
	}
	switch f.name {
	case "createIssue":
		repoID := flat("repositoryId")
		_, r, _, _ := s.lookupGID(repoID)
		if r == nil {
			*errs = append(*errs, "repository not found")
			return "", false
		}
		title := flat("title")
		body := flat("body")
		var labels []string
		if input != nil {
			if arr, ok := input["labels"].([]any); ok {
				for _, e := range arr {
					if str, ok := e.(string); ok {
						labels = append(labels, str)
					}
				}
			}
		}
		is, errR := s.createIssueLocked(r, title, body, labels)
		if errR != nil {
			*errs = append(*errs, "createIssue failed")
			return "", false
		}
		return s.mutationResult(f, vars, errs, gqlNode{typ: "Issue", repo: r, issue: is}, "issue"), true
	case "closeIssue":
		id := flat("issueId")
		_, r, is, _ := s.lookupGID(id)
		if is == nil {
			*errs = append(*errs, "issue not found")
			return "", false
		}
		is.State = "closed"
		is.cache = nil
		return s.mutationResult(f, vars, errs, gqlNode{typ: "Issue", repo: r, issue: is}, "issue"), true
	case "createPullRequest":
		repoID := flat("repositoryId")
		_, r, _, _ := s.lookupGID(repoID)
		if r == nil {
			*errs = append(*errs, "repository not found")
			return "", false
		}
		p, errR := s.createPullLocked(r, flat("title"), flat("body"), flat("headRefName"), flat("baseRefName"))
		if errR != nil {
			*errs = append(*errs, "createPullRequest failed")
			return "", false
		}
		return s.mutationResult(f, vars, errs, gqlNode{typ: "PullRequest", repo: r, pull: p}, "pullRequest"), true
	case "mergePullRequest":
		id := flat("pullRequestId")
		_, r, _, p := s.lookupGID(id)
		if p == nil {
			*errs = append(*errs, "pull request not found")
			return "", false
		}
		if _, errR := s.mergePullLocked(r, p); errR != nil {
			*errs = append(*errs, "mergePullRequest failed")
			return "", false
		}
		return s.mutationResult(f, vars, errs, gqlNode{typ: "PullRequest", repo: r, pull: p}, "pullRequest"), true
	case "addComment":
		id := flat("subjectId")
		body := flat("body")
		_, r, is, p := s.lookupGID(id)
		if r == nil || (is == nil && p == nil) {
			*errs = append(*errs, "subject not found")
			return "", false
		}
		num := 0
		var subj gqlNode
		if is != nil {
			num = is.Number
			subj = gqlNode{typ: "Issue", repo: r, issue: is}
		} else {
			num = p.Number
			subj = gqlNode{typ: "PullRequest", repo: r, pull: p}
		}
		c := &Comment{ID: r.NextCommentID, Body: body}
		r.NextCommentID++
		if r.Comments == nil {
			r.Comments = make(map[int][]*Comment)
		}
		r.Comments[num] = append(r.Comments[num], c)
		created := isoTime(s.store.nextSeq())
		cnode := gqlNode{typ: "IssueComment", repo: r, issue: is, pull: p, comment: c, created: created}
		if is == nil {
			// comment on a pull request: reuse the issue-shaped node with
			// the pull's number for the URL
			cnode.issue = &Issue{Number: num}
		}
		var b strings.Builder
		b.WriteByte('{')
		for i, sf := range f.sel {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(quote(sf.alias))
			b.WriteByte(':')
			switch sf.name {
			case "commentEdge":
				// {"node": {...}}
				var eb strings.Builder
				eb.WriteByte('{')
				for j, ef := range sf.sel {
					if j > 0 {
						eb.WriteByte(',')
					}
					eb.WriteString(quote(ef.alias))
					eb.WriteByte(':')
					if ef.name == "node" {
						eb.WriteString(s.execFields(ef.sel, cnode, vars, errs))
					} else {
						eb.WriteString("null")
					}
				}
				eb.WriteByte('}')
				b.WriteString(eb.String())
			case "subject":
				b.WriteString(s.execFields(sf.sel, subj, vars, errs))
			case "clientMutationId":
				if input != nil {
					if v, ok := input["clientMutationId"].(string); ok {
						b.WriteString(quote(v))
						continue
					}
				}
				b.WriteString("null")
			default:
				b.WriteString("null")
			}
		}
		b.WriteByte('}')
		return b.String(), true
	}
	*errs = append(*errs, "Cannot query field \""+f.name+"\" on type \"Mutation\".")
	return "", false
}

// mutationResult renders a mutation payload whose main object lives under
// key ("issue" or "pullRequest").
func (s *server) mutationResult(f *gqlField, vars map[string]any, errs *[]string, node gqlNode, key string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, sf := range f.sel {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(quote(sf.alias))
		b.WriteByte(':')
		if sf.name == key {
			b.WriteString(s.execFields(sf.sel, node, vars, errs))
		} else if sf.name == "clientMutationId" {
			b.WriteString("null")
		} else {
			b.WriteString("null")
		}
	}
	b.WriteByte('}')
	return b.String()
}
