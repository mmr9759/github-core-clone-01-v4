// In-memory store: repositories, git objects and refs, issues, pull
// requests, labels and comments. One RWMutex guards everything; reads
// share it, mutations take it exclusively. All server-generated timestamps
// come from a counter, never the wall clock, so two runs of the same
// request sequence leave identical state.
package main

import (
	"sort"
	"strconv"
	"strings"
	"sync"
)

const viewerLogin = "adaiversen"
const viewerID = 1000
const viewerName = "ada iversen"
const viewerEmail = "adaiversen@example.invalid"

// Deterministic clock: every timestamp is the base plus a sequence number.
const timeBaseEpoch = 1735689600 // 2025-01-01T00:00:00Z
const commitBaseEpoch = 1700000000

func isoTime(seq int) string {
	// Format timeBaseEpoch+seq as ISO-8601 UTC without time.Format (which
	// is comparatively slow and needless here: the base is a fixed date).
	total := timeBaseEpoch + seq
	days := total / 86400
	secs := total % 86400
	// Days since epoch for 2025-01-01 is 20089; walk forward.
	y, m, d := civilFromDays(days)
	return pad4(y) + "-" + pad2(m) + "-" + pad2(d) + "T" + pad2(secs/3600) + ":" + pad2((secs/60)%60) + ":" + pad2(secs%60) + "Z"
}

func civilFromDays(z int) (y, m, d int) {
	// Howard Hinnant's civil-from-days algorithm.
	z += 719468
	era := z / 146097
	if z < 0 {
		era = (z - 146096) / 146097
	}
	doe := z - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	y = yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d = doy - (153*mp+2)/5 + 1
	m = mp + 3
	if mp >= 10 {
		m = mp - 9
	}
	if m <= 2 {
		y++
	}
	return y, m, d
}

func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

func pad4(n int) string {
	s := strconv.Itoa(n)
	for len(s) < 4 {
		s = "0" + s
	}
	return s
}

// ---------------------------------------------------------------------------
// objects

type gitObject struct {
	typ  string // commit, tree, blob, tag
	data []byte
}

type Label struct {
	Name  string
	Color string
}

type Comment struct {
	ID   int
	Body string
}

type Issue struct {
	ID     int
	Number int
	Title  string
	Body   string
	State  string // open, closed
	Labels []*Label
	Seq    int
	cache  []byte // serialized REST form, cleared on change
}

type Pull struct {
	ID         int
	Number     int
	Title      string
	Body       string
	State      string // open, closed
	HeadRef    string
	BaseRef    string
	HeadSHA    string
	BaseSHA    string // base tip when the pull was opened
	Merged     bool
	MergedAt   string
	MergeSHA   string
	Seq        int
	cache      []byte // serialized REST form, cleared on change
	filesCache []byte // serialized files list, cleared on change
	mergeable  *bool  // cached mergeability verdict
}

type Repo struct {
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	DBID          int
	Seq           int
	Objects       map[string]*gitObject
	Refs          map[string]string // full ref name -> oid
	Issues        []*Issue
	IssuesByNum   map[int]*Issue
	Pulls         []*Pull
	PullsByNum    map[int]*Pull
	Labels        map[string]*Label
	LabelOrder    []*Label
	NextNum       int
	NextCommentID int
	Comments      map[int][]*Comment
	PackCache     map[string][]byte
}

type Store struct {
	mu          sync.RWMutex
	repos       map[string]*Repo
	order       []*Repo
	nextRepoID  int
	nextIssueID int
	seq         int
	commitSeq   int
}

func newStore() *Store {
	return &Store{
		repos:       make(map[string]*Repo),
		nextRepoID:  1,
		nextIssueID: 1,
	}
}

// nextSeq hands out the next deterministic timestamp sequence number.
func (s *Store) nextSeq() int {
	s.seq++
	return s.seq
}

func (s *Store) nextCommitSeq() int {
	s.commitSeq++
	return s.commitSeq
}

func (s *Store) createRepo(name, description string, priv bool) (*Repo, bool) {
	if _, ok := s.repos[name]; ok {
		return nil, false
	}
	r := &Repo{
		Name:          name,
		Description:   description,
		Private:       priv,
		DefaultBranch: "main",
		DBID:          s.nextRepoID,
		Seq:           s.nextSeq(),
		Objects:       make(map[string]*gitObject),
		Refs:          make(map[string]string),
		IssuesByNum:   make(map[int]*Issue),
		PullsByNum:    make(map[int]*Pull),
		Labels:        make(map[string]*Label),
		NextNum:       1,
		NextCommentID: 1,
		Comments:      make(map[int][]*Comment),
		PackCache:     make(map[string][]byte),
	}
	s.nextRepoID++
	s.repos[name] = r
	s.order = append(s.order, r)
	return r, true
}

func (s *Store) getRepo(name string) *Repo {
	return s.repos[name]
}

func (s *Store) deleteRepo(name string) bool {
	r, ok := s.repos[name]
	if !ok {
		return false
	}
	delete(s.repos, name)
	for i, other := range s.order {
		if other == r {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	return true
}

// dropPackCache clears a repository's cached packs after a mutation.
func (r *Repo) dropPackCache() {
	if len(r.PackCache) > 0 {
		r.PackCache = make(map[string][]byte)
	}
}

// ---------------------------------------------------------------------------
// JSON builders (hand-rolled for speed; the hot GET-issue path serves the
// cached bytes verbatim)

func (s *Store) repoJSON(r *Repo) []byte {
	var b strings.Builder
	b.WriteString(`{"id":`)
	b.WriteString(strconv.Itoa(r.DBID))
	b.WriteString(`,"name":`)
	b.WriteString(quote(r.Name))
	b.WriteString(`,"full_name":`)
	b.WriteString(quote(viewerLogin + "/" + r.Name))
	b.WriteString(`,"private":`)
	b.WriteString(strconv.FormatBool(r.Private))
	b.WriteString(`,"description":`)
	if r.Description == "" {
		b.WriteString("null")
	} else {
		b.WriteString(quote(r.Description))
	}
	b.WriteString(`,"default_branch":`)
	b.WriteString(quote(r.DefaultBranch))
	b.WriteString(`,"owner":{"login":`)
	b.WriteString(quote(viewerLogin))
	b.WriteString(`,"id":`)
	b.WriteString(strconv.Itoa(viewerID))
	b.WriteString(`,"type":"User"},"html_url":`)
	b.WriteString(quote("https://example.invalid/" + viewerLogin + "/" + r.Name))
	b.WriteString(`,"clone_url":`)
	b.WriteString(quote("https://example.invalid/" + viewerLogin + "/" + r.Name + ".git"))
	b.WriteString(`,"created_at":`)
	b.WriteString(quote(isoTime(r.Seq)))
	b.WriteString(`,"updated_at":`)
	b.WriteString(quote(isoTime(r.Seq)))
	b.WriteString(`,"has_issues":true,"has_projects":true,"has_wiki":true,"fork":false,"archived":false,"disabled":false}`)
	return []byte(b.String())
}

func issueJSON(s *Store, r *Repo, is *Issue) []byte {
	if is.cache != nil {
		return is.cache
	}
	var b strings.Builder
	b.WriteString(`{"id":`)
	b.WriteString(strconv.Itoa(is.ID))
	b.WriteString(`,"number":`)
	b.WriteString(strconv.Itoa(is.Number))
	b.WriteString(`,"title":`)
	b.WriteString(quote(is.Title))
	b.WriteString(`,"body":`)
	b.WriteString(quote(is.Body))
	b.WriteString(`,"state":`)
	b.WriteString(quote(is.State))
	b.WriteString(`,"labels":[`)
	for i, l := range is.Labels {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"name":`)
		b.WriteString(quote(l.Name))
		b.WriteString(`,"color":`)
		b.WriteString(quote(l.Color))
		b.WriteString(`}`)
	}
	b.WriteString(`],"user":{"login":`)
	b.WriteString(quote(viewerLogin))
	b.WriteString(`,"id":`)
	b.WriteString(strconv.Itoa(viewerID))
	b.WriteString(`},"created_at":`)
	b.WriteString(quote(isoTime(is.Seq)))
	b.WriteString(`,"updated_at":`)
	b.WriteString(quote(isoTime(is.Seq)))
	b.WriteString(`,"html_url":`)
	b.WriteString(quote("https://example.invalid/" + viewerLogin + "/" + r.Name + "/issues/" + strconv.Itoa(is.Number)))
	b.WriteString(`}`)
	is.cache = []byte(b.String())
	return is.cache
}

func (s *server) pullJSON(r *Repo, p *Pull) []byte {
	if p.cache != nil {
		return p.cache
	}
	var b strings.Builder
	b.WriteString(`{"id":`)
	b.WriteString(strconv.Itoa(p.ID))
	b.WriteString(`,"number":`)
	b.WriteString(strconv.Itoa(p.Number))
	b.WriteString(`,"state":`)
	b.WriteString(quote(p.State))
	b.WriteString(`,"title":`)
	b.WriteString(quote(p.Title))
	b.WriteString(`,"body":`)
	b.WriteString(quote(p.Body))
	b.WriteString(`,"head":{"ref":`)
	b.WriteString(quote(p.HeadRef))
	b.WriteString(`,"sha":`)
	b.WriteString(quote(p.HeadSHA))
	b.WriteString(`,"label":`)
	b.WriteString(quote(viewerLogin + ":" + p.HeadRef))
	b.WriteString(`},"base":{"ref":`)
	b.WriteString(quote(p.BaseRef))
	b.WriteString(`,"sha":`)
	b.WriteString(quote(p.BaseSHA))
	b.WriteString(`,"label":`)
	b.WriteString(quote(viewerLogin + ":" + p.BaseRef))
	b.WriteString(`},"merged":`)
	b.WriteString(strconv.FormatBool(p.Merged))
	b.WriteString(`,"mergeable":`)
	b.WriteString(strconv.FormatBool(s.pullMergeable(r, p)))
	b.WriteString(`,"merged_at":`)
	if p.MergedAt == "" {
		b.WriteString("null")
	} else {
		b.WriteString(quote(p.MergedAt))
	}
	b.WriteString(`,"merge_commit_sha":`)
	if p.MergeSHA == "" {
		b.WriteString("null")
	} else {
		b.WriteString(quote(p.MergeSHA))
	}
	b.WriteString(`,"user":{"login":`)
	b.WriteString(quote(viewerLogin))
	b.WriteString(`,"id":`)
	b.WriteString(strconv.Itoa(viewerID))
	b.WriteString(`},"created_at":`)
	b.WriteString(quote(isoTime(p.Seq)))
	b.WriteString(`,"updated_at":`)
	b.WriteString(quote(isoTime(p.Seq)))
	b.WriteString(`,"html_url":`)
	b.WriteString(quote("https://example.invalid/" + viewerLogin + "/" + r.Name + "/pull/" + strconv.Itoa(p.Number)))
	b.WriteString(`}`)
	p.cache = []byte(b.String())
	return p.cache
}

// sortedBranchNames lists refs/heads/* names in sorted order.
func (r *Repo) sortedBranchNames() []string {
	var names []string
	for name := range r.Refs {
		if strings.HasPrefix(name, "refs/heads/") {
			names = append(names, name[len("refs/heads/"):])
		}
	}
	sort.Strings(names)
	return names
}
