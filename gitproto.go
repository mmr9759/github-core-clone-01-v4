// Git smart HTTP: protocol v2 (upload-pack) and v0 (upload-pack and
// receive-pack). Advertisements, ls-refs, fetch with shallow support and
// receive-pack with per-ref report-status over side-band-64k.
package main

import (
	"bytes"
	"sort"
	"strings"
)

const gitAgent = "forge/1.0"

// sortedRefNames returns all ref names of a repo in sorted order.
func sortedRefNames(r *Repo) []string {
	names := make([]string, 0, len(r.Refs))
	for name := range r.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// refTip returns the oid a ref points at, or "" when absent.
func (r *Repo) refTip(name string) string { return r.Refs[name] }

// headTarget is the default branch's ref name.
func (r *Repo) headTarget() string { return "refs/heads/" + r.DefaultBranch }

// headOID is the default branch tip, or the zero oid when unborn.
func (r *Repo) headOID() string {
	if oid := r.Refs[r.headTarget()]; oid != "" {
		return oid
	}
	return zeroOID
}

// peeledTarget peels a tag object to its commit, or "" when the ref does
// not point at a tag object.
func (r *Repo) peeledTarget(oid string) string {
	obj := r.Objects[oid]
	if obj == nil || obj.typ != "tag" {
		return ""
	}
	return peelToCommit(r.lookup, parseTag(obj.data).object)
}

func (r *Repo) lookup(oid string) *gitObject { return r.Objects[oid] }

// ---------------------------------------------------------------------------
// advertisements

func (s *server) gitInfoRefs(req *request, owner, name, service string) response {
	if owner != viewerLogin {
		return jsonErr(404, "Not Found")
	}
	s.store.mu.RLock()
	r := s.store.getRepo(name)
	if r == nil {
		s.store.mu.RUnlock()
		return jsonErr(404, "Not Found")
	}
	var body []byte
	var ctype string
	if service == "git-upload-pack" {
		if v, ok := req.header("git-protocol"); ok && strings.Contains(v, "version=2") {
			body = v2Advertisement()
			ctype = "application/x-git-upload-pack-advertisement"
		} else {
			body = v0UploadAdvertisement(r)
			ctype = "application/x-git-upload-pack-advertisement"
		}
	} else {
		body = v0ReceiveAdvertisement(r)
		ctype = "application/x-git-receive-pack-advertisement"
	}
	s.store.mu.RUnlock()
	return response{status: 200, ctype: ctype, body: body}
}

func v2Advertisement() []byte {
	var b bytes.Buffer
	b.Write(pktStr("version 2\n"))
	b.Write(pktStr("agent=" + gitAgent + "\n"))
	b.Write(pktStr("ls-refs\n"))
	b.Write(pktStr("fetch=shallow wait-for-done\n"))
	b.Write(pktStr("object-format=sha1\n"))
	b.Write(pktFlush)
	return b.Bytes()
}

// v0Advertisement builds a v0 ref advertisement with a "# service="
// preamble, a flush, the ref lines (capabilities on the first line after a
// NUL) and a trailing flush.
func v0Advertisement(r *Repo, service string, caps string) []byte {
	var b bytes.Buffer
	b.Write(pktStr("# service=" + service + "\n"))
	b.Write(pktFlush)
	names := sortedRefNames(r)
	first := true
	writeLine := func(oid, name string) {
		line := oid + " " + name
		if first {
			line += "\x00" + caps
			first = false
		}
		b.Write(pktStr(line + "\n"))
	}
	if len(names) == 0 {
		writeLine(zeroOID, "capabilities^{}")
	} else {
		writeLine(r.headOID(), "HEAD")
		for _, name := range names {
			oid := r.Refs[name]
			writeLine(oid, name)
			if peeled := r.peeledTarget(oid); peeled != "" {
				b.Write(pktStr(peeled + " " + name + "^{}\n"))
			}
		}
	}
	b.Write(pktFlush)
	return b.Bytes()
}

func v0UploadAdvertisement(r *Repo) []byte {
	caps := "symref=HEAD:" + r.headTarget() + " side-band-64k thin-pack ofs-delta agent=" + gitAgent
	return v0Advertisement(r, "git-upload-pack", caps)
}

func v0ReceiveAdvertisement(r *Repo) []byte {
	caps := "report-status side-band-64k delete-refs agent=" + gitAgent
	return v0Advertisement(r, "git-receive-pack", caps)
}

// ---------------------------------------------------------------------------
// upload-pack POST: v2 ls-refs / fetch

func (s *server) gitUploadPack(req *request, owner, name string) response {
	if owner != viewerLogin {
		return jsonErr(404, "Not Found")
	}
	packets, _, err := parsePktLines(req.body)
	if err != nil || len(packets) == 0 || packets[0].kind != 'd' {
		return response{status: 400, body: []byte{}}
	}
	cmd := string(packets[0].payload)
	// v2 commands are "command=ls-refs" / "command=fetch". Anything else
	// (including a v0-style body) is not a command this server speaks.
	if !strings.HasPrefix(cmd, "command=") {
		return response{status: 400, body: []byte{}}
	}
	command := strings.TrimSuffix(strings.TrimPrefix(cmd, "command="), "\n")

	s.store.mu.RLock()
	r := s.store.getRepo(name)
	s.store.mu.RUnlock()
	if r == nil {
		return jsonErr(404, "Not Found")
	}
	switch command {
	case "ls-refs":
		return s.lsRefs(req, r, packets[1:])
	case "fetch":
		return s.fetch(req, r, packets[1:])
	default:
		return response{status: 400, body: []byte{}}
	}
}

// lsRefsArgs parses the argument lines of a v2 ls-refs request.
func lsRefsArgs(packets []pkt) (peel, symrefs bool, prefixes []string) {
	peel, symrefs = false, false
	for _, p := range packets {
		if p.kind != 'd' {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		switch {
		case line == "peel":
			peel = true
		case line == "symrefs":
			symrefs = true
		case strings.HasPrefix(line, "ref-prefix "):
			prefixes = append(prefixes, line[len("ref-prefix "):])
		}
	}
	return peel, symrefs, prefixes
}

func (s *server) lsRefs(req *request, r *Repo, packets []pkt) response {
	peel, symrefs, prefixes := lsRefsArgs(packets)
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	var b bytes.Buffer
	match := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	if len(prefixes) == 0 || match("HEAD") {
		// HEAD is listed when no prefix filter applies, or when it
		// matches one of the requested prefixes (a clone asks for it
		// explicitly via "ref-prefix HEAD").
		line := r.headOID() + " HEAD"
		if symrefs {
			line += " symref-target:" + r.headTarget()
		}
		b.Write(pktStr(line + "\n"))
	}
	for _, name := range sortedRefNames(r) {
		if !match(name) {
			continue
		}
		oid := r.Refs[name]
		line := oid + " " + name
		if peel {
			if peeled := r.peeledTarget(oid); peeled != "" {
				line += " peeled:" + peeled
			}
		}
		b.Write(pktStr(line + "\n"))
	}
	b.Write(pktFlush)
	return response{status: 200, ctype: "application/x-git-upload-pack-result", body: b.Bytes()}
}

// ---------------------------------------------------------------------------
// fetch

type fetchArgs struct {
	wants    []string
	haves    []string
	shallow  []string
	depth    int
	hasDepth bool
	done     bool
}

func parseFetchArgs(packets []pkt) fetchArgs {
	var a fetchArgs
	for _, p := range packets {
		if p.kind != 'd' {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			a.wants = append(a.wants, line[5:])
		case strings.HasPrefix(line, "have "):
			a.haves = append(a.haves, line[5:])
		case strings.HasPrefix(line, "shallow "):
			a.shallow = append(a.shallow, line[8:])
		case strings.HasPrefix(line, "deepen "):
			var d int
			for _, c := range line[7:] {
				if c >= '0' && c <= '9' {
					d = d*10 + int(c-'0')
				}
			}
			a.depth = d
			a.hasDepth = true
		case line == "done":
			a.done = true
		}
	}
	return a
}

func (s *server) fetch(req *request, r *Repo, packets []pkt) response {
	args := parseFetchArgs(packets)
	// Negotiation round (wait-for-done): the client sent "have" lines
	// but no "done" yet. Answer with the acknowledgments section only;
	// the client then sends a second request carrying "done", and only
	// that round receives the pack.
	if len(args.haves) > 0 && !args.done {
		var b bytes.Buffer
		b.Write(pktStr("acknowledgments\n"))
		b.Write(pktStr("NAK\n"))
		b.Write(pktFlush)
		return response{status: 200, ctype: "application/x-git-upload-pack-result", body: b.Bytes()}
	}
	s.store.mu.RLock()
	oids, shallow, unshallow := r.computeFetch(args)
	var pack []byte
	if len(oids) == 0 {
		pack = emptyPack()
	} else {
		key := fetchCacheKey(args, oids)
		if key != "" {
			if cached, ok := r.PackCache[key]; ok {
				pack = cached
			}
		}
		if pack == nil {
			pack = buildPack(r.Objects, oids)
			if key != "" {
				// Upgrade to a write lock briefly to store the pack.
				s.store.mu.RUnlock()
				s.store.mu.Lock()
				if _, ok := r.PackCache[key]; !ok {
					r.PackCache[key] = pack
				}
				s.store.mu.Unlock()
				s.store.mu.RLock()
			}
		}
	}
	s.store.mu.RUnlock()

	var b bytes.Buffer
	// Final round (client sent "done", or a fresh clone with no haves):
	// the response starts with the optional shallow-info section and
	// then the packfile section. No acknowledgments here: the client
	// already consumed that section in the negotiation round.
	if len(shallow) > 0 || len(unshallow) > 0 {
		b.Write(pktStr("shallow-info\n"))
		for _, oid := range shallow {
			b.Write(pktStr("shallow " + oid + "\n"))
		}
		for _, oid := range unshallow {
			b.Write(pktStr("unshallow " + oid + "\n"))
		}
		b.Write(pktFlush)
	}
	b.Write(pktStr("packfile\n"))
	for off := 0; off < len(pack); {
		end := off + 32768
		if end > len(pack) {
			end = len(pack)
		}
		chunk := make([]byte, 0, end-off+1)
		chunk = append(chunk, 1)
		chunk = append(chunk, pack[off:end]...)
		b.Write(pktLine(chunk))
		off = end
	}
	b.Write(pktFlush)
	return response{status: 200, ctype: "application/x-git-upload-pack-result", body: b.Bytes()}
}

// fetchCacheKey names the cache slot for a fetch with no haves and no
// previous shallow boundary: full clones and depth-1 clones of one tip.
func fetchCacheKey(args fetchArgs, oids []string) string {
	if len(args.haves) > 0 || len(args.shallow) > 0 {
		return ""
	}
	if args.hasDepth && args.depth == 1 && len(args.wants) == 1 {
		return "d1:" + args.wants[0]
	}
	if !args.hasDepth {
		sorted := make([]string, len(args.wants))
		copy(sorted, args.wants)
		sort.Strings(sorted)
		return "full:" + strings.Join(sorted, ",")
	}
	return ""
}

// haveReachableBounded lists the objects a client already holds, given
// its "have" commits and its shallow boundary: from each have, follow
// parents, but never past a shallow commit (the client has that commit
// and its tree, not its parents). Non-commit haves contribute their
// whole reachable closure.
func haveReachableBounded(lookup objLookup, haves []string, shallow map[string]bool) map[string]bool {
	out := make(map[string]bool)
	seen := make(map[string]bool)
	queue := append([]string{}, haves...)
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := lookup(oid)
		if obj == nil {
			continue
		}
		if obj.typ != "commit" {
			for extra := range collectReachable(lookup, []string{oid}) {
				out[extra] = true
			}
			continue
		}
		out[oid] = true
		parsed := parseCommit(obj.data)
		for extra := range collectReachable(lookup, []string{parsed.tree}) {
			out[extra] = true
		}
		if shallow[oid] {
			continue
		}
		queue = append(queue, parsed.parents...)
	}
	return out
}

// computeFetch selects the objects a fetch sends, plus the shallow and
// unshallow boundary lines. Caller holds at least a read lock.
func (r *Repo) computeFetch(args fetchArgs) (oids, shallow, unshallow []string) {
	lookup := r.lookup
	// Directly wanted objects (tags included) always travel.
	included := make(map[string]bool)
	for _, want := range args.wants {
		if want == zeroOID {
			continue
		}
		included[want] = true
	}
	// Commit generations from the wants, peeling tags.
	gen := make(map[string]int)
	var queue []string
	for _, want := range args.wants {
		if want == zeroOID {
			continue
		}
		commit := peelToCommit(lookup, want)
		if obj := lookup(commit); obj == nil || obj.typ != "commit" {
			continue
		}
		if _, ok := gen[commit]; !ok {
			gen[commit] = 0
			queue = append(queue, commit)
		}
	}
	for head := 0; head < len(queue); head++ {
		oid := queue[head]
		g := gen[oid]
		if args.hasDepth && g+1 >= args.depth {
			continue // parents are beyond the depth cut
		}
		obj := lookup(oid)
		if obj == nil || obj.typ != "commit" {
			continue
		}
		for _, p := range parseCommit(obj.data).parents {
			if _, ok := gen[p]; !ok {
				gen[p] = g + 1
				queue = append(queue, p)
			}
		}
	}
	// Included commits: within the depth cut when one applies.
	includedCommits := make(map[string]bool)
	for oid, g := range gen {
		if args.hasDepth && g >= args.depth {
			continue
		}
		includedCommits[oid] = true
		included[oid] = true
	}
	// Everything reachable from the included commits' trees.
	for oid := range includedCommits {
		obj := lookup(oid)
		if obj == nil {
			continue
		}
		tree := parseCommit(obj.data).tree
		if tree != "" {
			for extra := range collectReachable(lookup, []string{tree}) {
				included[extra] = true
			}
		}
	}
	// Haves are never re-sent — but only the objects the client really
	// has. A "shallow" line names a commit the client holds whose
	// parents it lacks (a shallow boundary), so the traversal from a
	// have stops at shallow commits: their own tree travels with them,
	// their parents do not. Without this, a deepen would delete the
	// very parent it is supposed to deliver.
	if len(args.haves) > 0 {
		shallowSet := make(map[string]bool, len(args.shallow))
		for _, s := range args.shallow {
			shallowSet[s] = true
		}
		for old := range haveReachableBounded(lookup, args.haves, shallowSet) {
			delete(included, old)
		}
	}
	// Shallow boundary: included commits with a parent outside the set.
	for oid := range includedCommits {
		if !included[oid] {
			continue
		}
		obj := lookup(oid)
		if obj == nil {
			continue
		}
		for _, p := range parseCommit(obj.data).parents {
			if !includedCommits[p] {
				shallow = append(shallow, oid)
				break
			}
		}
	}
	// Unshallow: previously shallow commits whose parents are all in now.
	oldShallow := make(map[string]bool, len(args.shallow))
	for _, oid := range args.shallow {
		oldShallow[oid] = true
	}
	for oid := range oldShallow {
		if !included[oid] {
			continue
		}
		obj := lookup(oid)
		if obj == nil || obj.typ != "commit" {
			continue
		}
		allIn := true
		for _, p := range parseCommit(obj.data).parents {
			if !includedCommits[p] {
				allIn = false
				break
			}
		}
		if allIn {
			unshallow = append(unshallow, oid)
		}
	}
	sort.Strings(shallow)
	sort.Strings(unshallow)
	for oid := range included {
		oids = append(oids, oid)
	}
	sort.Strings(oids)
	return oids, shallow, unshallow
}

// ---------------------------------------------------------------------------
// receive-pack

type pushCommand struct {
	oldOID string
	newOID string
	ref    string
}

func (s *server) gitReceivePack(req *request, owner, name string) response {
	if owner != viewerLogin {
		return jsonErr(404, "Not Found")
	}
	packets, rest, err := parsePktLines(req.body)
	if err != nil || len(packets) == 0 {
		return response{status: 400, body: []byte{}}
	}
	var commands []pushCommand
	for _, p := range packets {
		if p.kind == 'f' {
			break
		}
		if p.kind != 'd' {
			continue
		}
		line := string(p.payload)
		if i := strings.IndexByte(line, 0); i >= 0 {
			line = line[:i] // capabilities ride the first line
		}
		line = strings.TrimSuffix(line, "\n")
		parts := strings.SplitN(line, " ", 3)
		if len(parts) != 3 || !isOID(parts[0]) || !isOID(parts[1]) || !strings.HasPrefix(parts[2], "refs/") {
			return response{status: 400, body: []byte{}}
		}
		commands = append(commands, pushCommand{parts[0], parts[1], parts[2]})
	}
	if len(commands) == 0 {
		return response{status: 400, body: []byte{}}
	}

	s.store.mu.Lock()
	r := s.store.getRepo(name)
	if r == nil {
		s.store.mu.Unlock()
		return jsonErr(404, "Not Found")
	}
	lookup := r.lookup
	// Unpack the pack, if one was sent.
	unpackOK := true
	unpackDetail := "ok"
	if len(rest) > 0 {
		objects, perr := parsePack(rest, lookup)
		if perr != nil {
			unpackOK = false
			unpackDetail = "unpack error"
		} else {
			for oid, obj := range objects {
				r.Objects[oid] = obj
			}
		}
	}
	// Decide every ref, then apply the accepted ones.
	type verdict struct {
		ok     bool
		reason string
	}
	verdicts := make([]verdict, len(commands))
	for i, cmd := range commands {
		if !unpackOK {
			verdicts[i] = verdict{false, "unpack error"}
			continue
		}
		current, exists := r.Refs[cmd.ref]
		switch {
		case cmd.newOID == zeroOID: // delete
			if !exists {
				verdicts[i] = verdict{false, "remote ref does not exist"}
			} else if current != cmd.oldOID {
				verdicts[i] = verdict{false, "non-fast-forward"}
			} else {
				verdicts[i] = verdict{true, ""}
			}
		case cmd.oldOID == zeroOID: // create
			if exists {
				verdicts[i] = verdict{false, "already exists"}
			} else if lookup(cmd.newOID) == nil {
				verdicts[i] = verdict{false, "object not found"}
			} else {
				verdicts[i] = verdict{true, ""}
			}
		default: // update
			if !exists || current != cmd.oldOID {
				verdicts[i] = verdict{false, "non-fast-forward"}
			} else if lookup(cmd.newOID) == nil {
				verdicts[i] = verdict{false, "object not found"}
			} else if cmd.newOID == cmd.oldOID {
				verdicts[i] = verdict{true, ""}
			} else if strings.HasPrefix(cmd.ref, "refs/tags/") {
				verdicts[i] = verdict{true, ""}
			} else if isAncestor(lookup, cmd.oldOID, cmd.newOID) {
				verdicts[i] = verdict{true, ""}
			} else {
				verdicts[i] = verdict{false, "non-fast-forward"}
			}
		}
	}
	changed := false
	for i, cmd := range commands {
		if !verdicts[i].ok {
			continue
		}
		if cmd.newOID == zeroOID {
			delete(r.Refs, cmd.ref)
		} else {
			r.Refs[cmd.ref] = cmd.newOID
		}
		changed = true
	}
	if changed {
		r.dropPackCache()
	}
	s.store.mu.Unlock()

	// Report over side-band-64k: band-1 packets carrying the pkt-lined
	// report-status stream, then a flush.
	var inner bytes.Buffer
	if unpackOK {
		inner.Write(pktStr("unpack ok\n"))
	} else {
		inner.Write(pktStr("unpack " + unpackDetail + "\n"))
	}
	for i, cmd := range commands {
		if verdicts[i].ok {
			inner.Write(pktStr("ok " + cmd.ref + "\n"))
		} else {
			inner.Write(pktStr("ng " + cmd.ref + " " + verdicts[i].reason + "\n"))
		}
	}
	inner.Write(pktFlush)
	var b bytes.Buffer
	raw := inner.Bytes()
	for off := 0; off < len(raw); {
		end := off + 32768
		if end > len(raw) {
			end = len(raw)
		}
		chunk := make([]byte, 0, end-off+1)
		chunk = append(chunk, 1)
		chunk = append(chunk, raw[off:end]...)
		b.Write(pktLine(chunk))
		off = end
	}
	b.Write(pktFlush)
	return response{status: 200, ctype: "application/x-git-receive-pack-result", body: b.Bytes()}
}
