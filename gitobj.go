// Git object model: hashing, tree/commit/tag coding, reachability, packfile
// building and parsing, pkt-line framing and shallow-clone bookkeeping.
// Packs carry full objects only (no deltas); incoming packs may also carry
// deltas, which are resolved against the pack itself and the store.
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/adler32"
	"io"
	"sort"
)

const zeroOID = "0000000000000000000000000000000000000000"

func isOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func hashObject(typ string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(typ))
	h.Write([]byte(" "))
	h.Write([]byte(fmt.Sprintf("%d", len(data))))
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// ---------------------------------------------------------------------------
// trees, commits, tags

type treeEntry struct {
	mode string
	name string
	oid  string
}

func parseTree(data []byte) []treeEntry {
	var out []treeEntry
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp < 0 {
			break
		}
		mode := string(data[:sp])
		rest := data[sp+1:]
		nul := bytes.IndexByte(rest, 0)
		if nul < 0 || len(rest) < nul+21 {
			break
		}
		name := string(rest[:nul])
		oid := hex.EncodeToString(rest[nul+1 : nul+21])
		out = append(out, treeEntry{mode, name, oid})
		data = rest[nul+21:]
	}
	return out
}

// buildTree encodes entries in git order: by name, directories sorting as
// if their name carried a trailing slash.
func buildTree(entries []treeEntry) []byte {
	sorted := make([]treeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		an, bn := a.name, b.name
		if a.mode == "40000" {
			an += "/"
		}
		if b.mode == "40000" {
			bn += "/"
		}
		return an < bn
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		oid, _ := hex.DecodeString(e.oid)
		buf.WriteString(e.mode)
		buf.WriteByte(' ')
		buf.WriteString(e.name)
		buf.WriteByte(0)
		buf.Write(oid)
	}
	return buf.Bytes()
}

type commitView struct {
	tree      string
	parents   []string
	author    string
	committer string
	message   string
}

func parseCommit(data []byte) commitView {
	var c commitView
	// Headers end at the first blank line.
	head := data
	if i := bytes.Index(data, []byte("\n\n")); i >= 0 {
		head = data[:i]
		c.message = string(data[i+2:])
	}
	for _, line := range bytes.Split(head, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		sp := bytes.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		key, val := string(line[:sp]), string(line[sp+1:])
		switch key {
		case "tree":
			c.tree = val
		case "parent":
			c.parents = append(c.parents, val)
		case "author":
			c.author = val
		case "committer":
			c.committer = val
		}
	}
	return c
}

func buildCommit(tree string, parents []string, author, committer, message string) []byte {
	var b bytes.Buffer
	b.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	b.WriteString("author " + author + "\n")
	b.WriteString("committer " + committer + "\n")
	b.WriteString("\n")
	b.WriteString(message)
	return b.Bytes()
}

type tagView struct {
	object string
	typ    string
	tag    string
	tagger string
}

func parseTag(data []byte) tagView {
	var t tagView
	head := data
	if i := bytes.Index(data, []byte("\n\n")); i >= 0 {
		head = data[:i]
	}
	for _, line := range bytes.Split(head, []byte("\n")) {
		sp := bytes.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		key, val := string(line[:sp]), string(line[sp+1:])
		switch key {
		case "object":
			t.object = val
		case "type":
			t.typ = val
		case "tag":
			t.tag = val
		case "tagger":
			t.tagger = val
		}
	}
	return t
}

// identityLine builds "Name <email> <epoch> +0000".
func identityLine(epoch int) string {
	return viewerName + " <" + viewerEmail + "> " + fmt.Sprintf("%d", epoch) + " +0000"
}

// ---------------------------------------------------------------------------
// reachability

type objLookup func(oid string) *gitObject

// collectReachable walks commits, trees and tags from the tips.
func collectReachable(lookup objLookup, tips []string) map[string]bool {
	seen := make(map[string]bool)
	var stack []string
	stack = append(stack, tips...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] || oid == zeroOID {
			continue
		}
		seen[oid] = true
		obj := lookup(oid)
		if obj == nil {
			continue
		}
		switch obj.typ {
		case "commit":
			c := parseCommit(obj.data)
			if c.tree != "" {
				stack = append(stack, c.tree)
			}
			stack = append(stack, c.parents...)
		case "tree":
			for _, e := range parseTree(obj.data) {
				stack = append(stack, e.oid)
			}
		case "tag":
			t := parseTag(obj.data)
			if t.object != "" {
				stack = append(stack, t.object)
			}
		}
	}
	return seen
}

// isAncestor reports whether ancestor is reachable from descendant by
// following parent links (tag objects are peeled first).
func isAncestor(lookup objLookup, ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	seen := make(map[string]bool)
	stack := []string{descendant}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := lookup(oid)
		if obj == nil {
			continue
		}
		switch obj.typ {
		case "commit":
			c := parseCommit(obj.data)
			for _, p := range c.parents {
				if p == ancestor {
					return true
				}
				stack = append(stack, p)
			}
		case "tag":
			t := parseTag(obj.data)
			if t.object == ancestor {
				return true
			}
			stack = append(stack, t.object)
		}
	}
	return false
}

// peelToCommit follows tag objects to the commit they point at.
func peelToCommit(lookup objLookup, oid string) string {
	for i := 0; i < 16; i++ {
		obj := lookup(oid)
		if obj == nil || obj.typ != "tag" {
			return oid
		}
		t := parseTag(obj.data)
		if t.object == "" {
			return oid
		}
		oid = t.object
	}
	return oid
}

// ---------------------------------------------------------------------------
// pkt-line framing

func pktLine(payload []byte) []byte {
	n := len(payload) + 4
	out := make([]byte, 0, n)
	out = append(out, hex4(n)...)
	out = append(out, payload...)
	return out
}

func pktStr(s string) []byte { return pktLine([]byte(s)) }

func hex4(n int) []byte {
	const digits = "0123456789abcdef"
	return []byte{digits[(n>>12)&0xf], digits[(n>>8)&0xf], digits[(n>>4)&0xf], digits[n&0xf]}
}

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

type pkt struct {
	kind    byte // 'd' data, 'f' flush, '1' delim, '2' response-end
	payload []byte
}

// parsePktLines decodes a whole buffer of pkt-lines. Trailing bytes that
// are not a pkt-line (a raw pack after the flush) stop the parse cleanly
// and are returned as the remainder.
func parsePktLines(data []byte) ([]pkt, []byte, error) {
	var out []pkt
	i := 0
	for i < len(data) {
		if len(data)-i < 4 {
			return out, data[i:], errors.New("truncated pkt-line")
		}
		head := string(data[i : i+4])
		if head == "0000" {
			out = append(out, pkt{kind: 'f'})
			i += 4
			continue
		}
		if head == "0001" {
			out = append(out, pkt{kind: '1'})
			i += 4
			continue
		}
		if head == "0002" {
			out = append(out, pkt{kind: '2'})
			i += 4
			continue
		}
		n, ok := parseHex4(data[i : i+4])
		if !ok || n < 4 {
			// Not a pkt-line: the rest is raw data (a packfile).
			return out, data[i:], nil
		}
		if i+n > len(data) {
			return out, data[i:], errors.New("truncated pkt-line")
		}
		out = append(out, pkt{kind: 'd', payload: data[i+4 : i+n]})
		i += n
	}
	return out, nil, nil
}

func parseHex4(b []byte) (int, bool) {
	n := 0
	for _, c := range b {
		n <<= 4
		switch {
		case c >= '0' && c <= '9':
			n |= int(c - '0')
		case c >= 'a' && c <= 'f':
			n |= int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			n |= int(c-'A') + 10
		default:
			return 0, false
		}
	}
	return n, true
}

// ---------------------------------------------------------------------------
// packfiles

type packObject struct {
	typ  string
	data []byte
}

var packTypeCode = map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var packCodeType = map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag", 6: "ofs-delta", 7: "ref-delta"}

// buildPack writes a version-2 packfile of full objects plus the SHA-1
// trailer. Objects are emitted in oid order for determinism.
func buildPack(objects map[string]*gitObject, oids []string) []byte {
	sorted := make([]string, len(oids))
	copy(sorted, oids)
	sort.Strings(sorted)
	var body bytes.Buffer
	body.WriteString("PACK")
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], 2)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(sorted)))
	body.Write(hdr[:])
	var tmp bytes.Buffer
	for _, oid := range sorted {
		obj := objects[oid]
		if obj == nil {
			continue
		}
		body.Write(packObjectHeader(obj.typ, len(obj.data)))
		tmp.Reset()
		zw, _ := zlib.NewWriterLevel(&tmp, zlib.BestSpeed)
		zw.Write(obj.data)
		zw.Close()
		body.Write(tmp.Bytes())
	}
	sum := sha1.Sum(body.Bytes())
	body.Write(sum[:])
	return body.Bytes()
}

func packObjectHeader(typ string, size int) []byte {
	code := packTypeCode[typ]
	var out []byte
	first := byte((code << 4) | (size & 0x0f))
	size >>= 4
	for size > 0 {
		out = append(out, first|0x80)
		first = byte(size & 0x7f)
		size >>= 7
	}
	out = append(out, first)
	return out
}

var errBadPack = errors.New("bad packfile")

// parsePack reads a packfile and returns its objects keyed by oid. Delta
// entries are resolved against earlier entries in the same pack and the
// store lookup.
func parsePack(data []byte, lookup objLookup) (map[string]*gitObject, error) {
	if len(data) < 32 || string(data[:4]) != "PACK" {
		return nil, errBadPack
	}
	version := binary.BigEndian.Uint32(data[4:8])
	count := binary.BigEndian.Uint32(data[8:12])
	if version != 2 && version != 3 {
		return nil, errBadPack
	}
	// Verify the trailer checksum.
	if len(data) < 20 {
		return nil, errBadPack
	}
	sum := sha1.Sum(data[:len(data)-20])
	if !bytes.Equal(sum[:], data[len(data)-20:]) {
		return nil, errBadPack
	}
	out := make(map[string]*gitObject, count)
	// Offsets of entries already read, for OFS_DELTA resolution.
	type entry struct {
		typ  string
		data []byte
		off  int
	}
	entries := make([]entry, 0, count)
	pos := 12
	for i := uint32(0); i < count; i++ {
		start := pos
		if pos >= len(data)-20 {
			return nil, errBadPack
		}
		b0 := data[pos]
		pos++
		typCode := int(b0>>4) & 7
		size := int(b0 & 0x0f)
		shift := 4
		for b0&0x80 != 0 {
			if pos >= len(data)-20 {
				return nil, errBadPack
			}
			b0 = data[pos]
			pos++
			size |= int(b0&0x7f) << shift
			shift += 7
		}
		typ, ok := packCodeType[typCode]
		if !ok {
			return nil, errBadPack
		}
		var base []byte
		if typCode == 6 { // OFS_DELTA
			if pos >= len(data)-20 {
				return nil, errBadPack
			}
			b := data[pos]
			pos++
			off := int(b & 0x7f)
			for b&0x80 != 0 {
				if pos >= len(data)-20 {
					return nil, errBadPack
				}
				b = data[pos]
				pos++
				off = ((off + 1) << 7) | int(b&0x7f)
			}
			baseOff := start - off
			var baseObj *gitObject
			for _, e := range entries {
				if e.off == baseOff {
					baseObj = &gitObject{e.typ, e.data}
					break
				}
			}
			if baseObj == nil {
				return nil, errBadPack
			}
			base = baseObj.data
			typ = baseObj.typ
		} else if typCode == 7 { // REF_DELTA
			if pos+20 > len(data)-20 {
				return nil, errBadPack
			}
			baseOID := hex.EncodeToString(data[pos : pos+20])
			pos += 20
			baseObj := lookup(baseOID)
			if baseObj == nil {
				if e, ok2 := out[baseOID]; ok2 {
					baseObj = e
				}
			}
			if baseObj == nil {
				return nil, errBadPack
			}
			base = baseObj.data
			typ = baseObj.typ
		}
		// Inflate the (possibly delta) data, learning exactly how many
		// input bytes the zlib stream occupied.
		raw, consumed, err := inflateZlib(data[pos:])
		if err != nil {
			return nil, errBadPack
		}
		pos += consumed
		if typCode == 6 || typCode == 7 {
			raw, err = applyDelta(base, raw)
			if err != nil {
				return nil, errBadPack
			}
		}
		if len(raw) != size {
			return nil, errBadPack
		}
		oid := hashObject(typ, raw)
		obj := &gitObject{typ, raw}
		out[oid] = obj
		entries = append(entries, entry{typ, raw, start})
	}
	return out, nil
}

// inflateZlib inflates one zlib stream and reports the input bytes it
// occupied. flate/zlib readers over-read past the end of a stream, so the
// consumed length cannot be learned from the reader: instead the stream's
// trailing Adler-32 checksum (the last 4 bytes of a zlib stream) is used
// to find the exact end, verified by re-inflating exactly that prefix.
func inflateZlib(data []byte) ([]byte, int, error) {
	if len(data) < 2 {
		return nil, 0, errBadPack
	}
	zr, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, 0, err
	}
	raw, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return nil, 0, err
	}
	want := adler32.Checksum(raw)
	for end := 2; end+4 <= len(data); end++ {
		if binary.BigEndian.Uint32(data[end:end+4]) != want {
			continue
		}
		zr2, err := zlib.NewReader(bytes.NewReader(data[:end+4]))
		if err != nil {
			continue
		}
		raw2, err := io.ReadAll(zr2)
		zr2.Close()
		if err != nil || !bytes.Equal(raw2, raw) {
			continue
		}
		return raw, end + 4, nil
	}
	return nil, 0, errBadPack
}

// applyDelta applies a git delta (copy/insert instructions) to a base.
func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() (int, error) {
		v := 0
		shift := 0
		for {
			if pos >= len(delta) {
				return 0, errBadPack
			}
			b := delta[pos]
			pos++
			v |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				return v, nil
			}
		}
	}
	srcSize, err := readVarint()
	if err != nil || srcSize != len(base) {
		return nil, errBadPack
	}
	dstSize, err := readVarint()
	if err != nil {
		return nil, errBadPack
	}
	var out bytes.Buffer
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			off, size := 0, 0
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, errBadPack
					}
					off |= int(delta[pos]) << (8 * uint(i))
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, errBadPack
					}
					size |= int(delta[pos]) << (8 * uint(i))
					pos++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if off+size > len(base) {
				return nil, errBadPack
			}
			out.Write(base[off : off+size])
		} else if op != 0 {
			if pos+int(op) > len(delta) {
				return nil, errBadPack
			}
			out.Write(delta[pos : pos+int(op)])
			pos += int(op)
		} else {
			return nil, errBadPack
		}
	}
	if out.Len() != dstSize {
		return nil, errBadPack
	}
	return out.Bytes(), nil
}

// emptyPack is a valid packfile with zero objects.
func emptyPack() []byte {
	var body bytes.Buffer
	body.WriteString("PACK")
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], 2)
	binary.BigEndian.PutUint32(hdr[4:8], 0)
	body.Write(hdr[:])
	sum := sha1.Sum(body.Bytes())
	body.Write(sum[:])
	return body.Bytes()
}
