// Git object model: trees, commits, tags, packfiles, and fetch planning.
// Behaviour mirrors the reference forge exactly; the code is written fresh
// in Go, not translated line by line.
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
)

type GitObject struct {
	Type string // commit | tree | blob | tag
	Data []byte
}

// ObjectLookup returns the object for an oid, or nil when absent.
type ObjectLookup func(oid string) *GitObject

var typeCodes = map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var codeTypes = map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

type TreeEntry struct {
	Mode string
	Name string
	Oid  string
}

// parseTree splits a tree object into entries, in stored order.
func parseTree(data []byte) []TreeEntry {
	var entries []TreeEntry
	off := 0
	for off < len(data) {
		space := bytes.IndexByte(data[off:], ' ')
		if space < 0 {
			break
		}
		space += off
		nul := bytes.IndexByte(data[space+1:], 0)
		if nul < 0 {
			break
		}
		nul += space + 1
		if nul+21 > len(data) {
			break
		}
		entries = append(entries, TreeEntry{
			Mode: string(data[off:space]),
			Name: string(data[space+1 : nul]),
			Oid:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		off = nul + 21
	}
	return entries
}

// compareTreeNames is git's tree sort: byte order, a directory compared as
// if its name ended in '/'.
func compareTreeNames(a, b TreeEntry) int {
	left, right := a.Name, b.Name
	if a.Mode == "40000" {
		left += "/"
	}
	if b.Mode == "40000" {
		right += "/"
	}
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

// buildTree serializes entries, sorted git's way.
func buildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return compareTreeNames(sorted[i], sorted[j]) < 0 })
	var out []byte
	for _, e := range sorted {
		out = append(out, []byte(e.Mode+" "+e.Name+"\x00")...)
		raw, _ := hex.DecodeString(e.Oid)
		out = append(out, raw...)
	}
	return out
}

type CommitView struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

// parseCommit scans the header lines for tree/parent/author/committer.
func parseCommit(data []byte) CommitView {
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
	}
	var message string
	if blank >= 0 {
		message = string(data[blank+2:])
	}
	cv := CommitView{}
	start := 0
	for start < headerEnd {
		end := bytes.IndexByte(data[start:], '\n')
		var line string
		if end < 0 || start+end > headerEnd {
			line = string(data[start:headerEnd])
			start = headerEnd
		} else {
			line = string(data[start : start+end])
			start += end + 1
		}
		switch {
		case len(line) > 5 && line[:5] == "tree ":
			cv.Tree = line[5:]
		case len(line) > 7 && line[:7] == "parent ":
			cv.Parents = append(cv.Parents, line[7:])
		case len(line) > 7 && line[:7] == "author ":
			cv.Author = line[7:]
		case len(line) > 10 && line[:10] == "committer ":
			cv.Committer = line[10:]
		}
	}
	cv.Message = message
	return cv
}

// peelToCommitWith follows tag objects (at most 10 hops) to a commit.
func peelToCommitWith(lookup ObjectLookup, oid string) (string, bool) {
	current := oid
	for i := 0; i < 10; i++ {
		obj := lookup(current)
		if obj == nil {
			return "", false
		}
		if obj.Type == "commit" {
			return current, true
		}
		if obj.Type == "tag" {
			current = parseTag(obj.Data).Object
			continue
		}
		return "", false
	}
	return "", false
}

type TagView struct {
	Object  string
	Type    string
	Tag     string
	Tagger  string
	Message string
}

func parseTag(data []byte) TagView {
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
	}
	var message string
	if blank >= 0 {
		message = string(data[blank+2:])
	}
	tv := TagView{Type: "commit"}
	start := 0
	for start < headerEnd {
		end := bytes.IndexByte(data[start:], '\n')
		var line string
		if end < 0 || start+end > headerEnd {
			line = string(data[start:headerEnd])
			start = headerEnd
		} else {
			line = string(data[start : start+end])
			start += end + 1
		}
		switch {
		case len(line) > 7 && line[:7] == "object ":
			tv.Object = line[7:]
		case len(line) > 5 && line[:5] == "type ":
			tv.Type = line[5:]
		case len(line) > 4 && line[:4] == "tag ":
			tv.Tag = line[4:]
		case len(line) > 7 && line[:7] == "tagger ":
			tv.Tagger = line[7:]
		}
	}
	tv.Message = message
	return tv
}

func buildCommit(tree string, parents []string, message, author, committer string, timestamp int64) []byte {
	stamp := fmt.Sprintf("%d +0000", timestamp)
	var b bytes.Buffer
	fmt.Fprintf(&b, "tree %s\n", tree)
	for _, p := range parents {
		fmt.Fprintf(&b, "parent %s\n", p)
	}
	fmt.Fprintf(&b, "author %s %s\ncommitter %s %s\n\n%s", author, stamp, committer, stamp, message)
	return b.Bytes()
}

func buildTag(object, typ, tag, message, tagger string, timestamp int64) []byte {
	stamp := fmt.Sprintf("%d +0000", timestamp)
	return []byte(fmt.Sprintf("object %s\ntype %s\ntag %s\ntagger %s %s\n\n%s", object, typ, tag, tagger, stamp, message))
}

// flattenTree walks a tree to path -> {oid, mode}, recursively.
func flattenTree(rootOid string, lookup ObjectLookup, out map[string]TreeEntry) {
	type frame struct {
		oid    string
		prefix string
	}
	stack := []frame{{oid: rootOid, prefix: ""}}
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		obj := lookup(f.oid)
		if obj == nil || obj.Type != "tree" {
			continue
		}
		for _, entry := range parseTree(obj.Data) {
			path := entry.Name
			if f.prefix != "" {
				path = f.prefix + "/" + entry.Name
			}
			if entry.Mode == "40000" {
				stack = append(stack, frame{oid: entry.Oid, prefix: path})
			} else if entry.Mode != "160000" {
				out[path] = entry
			}
		}
	}
}

type TreeDiffEntry struct {
	Path    string
	OldOid  string
	NewOid  string
	OldMode string
	NewMode string
	HasOld  bool
	HasNew  bool
}

// diffTrees returns one entry per path that differs, sorted by path.
func diffTrees(baseRoot string, hasBase bool, headRoot string, lookup ObjectLookup) []TreeDiffEntry {
	base := map[string]TreeEntry{}
	head := map[string]TreeEntry{}
	if hasBase {
		flattenTree(baseRoot, lookup, base)
	}
	flattenTree(headRoot, lookup, head)
	paths := map[string]bool{}
	for p := range base {
		paths[p] = true
	}
	for p := range head {
		paths[p] = true
	}
	var out []TreeDiffEntry
	for path := range paths {
		b, inBase := base[path]
		h, inHead := head[path]
		if inBase && inHead && b.Oid == h.Oid {
			continue
		}
		e := TreeDiffEntry{Path: path}
		if inBase {
			e.OldOid, e.OldMode, e.HasOld = b.Oid, b.Mode, true
		}
		if inHead {
			e.NewOid, e.NewMode, e.HasNew = h.Oid, h.Mode, true
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// mergeBase finds the nearest common ancestor of two commits.
func mergeBase(a, b string, lookup ObjectLookup) (string, bool) {
	ancestorsA := map[string]bool{}
	queueA := []string{a}
	for len(queueA) > 0 {
		oid := queueA[len(queueA)-1]
		queueA = queueA[:len(queueA)-1]
		if ancestorsA[oid] {
			continue
		}
		ancestorsA[oid] = true
		obj := lookup(oid)
		if obj == nil || obj.Type != "commit" {
			continue
		}
		for _, p := range parseCommit(obj.Data).Parents {
			queueA = append(queueA, p)
		}
	}
	queueB := []string{b}
	seenB := map[string]bool{}
	for len(queueB) > 0 {
		oid := queueB[0]
		queueB = queueB[1:]
		if seenB[oid] {
			continue
		}
		seenB[oid] = true
		if ancestorsA[oid] {
			return oid, true
		}
		obj := lookup(oid)
		if obj == nil || obj.Type != "commit" {
			continue
		}
		for _, p := range parseCommit(obj.Data).Parents {
			queueB = append(queueB, p)
		}
	}
	return "", false
}

// isAncestor is true when descendant is ancestor, or has it among its
// commit ancestors.
func isAncestor(ancestor, descendant string, lookup ObjectLookup) bool {
	if ancestor == descendant {
		return true
	}
	queue := []string{descendant}
	seen := map[string]bool{}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := lookup(oid)
		if obj == nil || obj.Type != "commit" {
			continue
		}
		for _, p := range parseCommit(obj.Data).Parents {
			if p == ancestor {
				return true
			}
			queue = append(queue, p)
		}
	}
	return false
}

// --- packfile write ---

func writePackObjectHeader(typ string, size int) []byte {
	code := typeCodes[typ]
	remaining := size
	first := (code << 4) | (remaining & 0x0f)
	remaining >>= 4
	var out []byte
	for remaining > 0 {
		out = append(out, byte(first|0x80))
		first = remaining & 0x7f
		remaining >>= 7
	}
	out = append(out, byte(first))
	return out
}

// writePack builds a version-2 pack from plain objects, in the given order.
// Object bytes differ from the reference's zlib output; only validity counts.
func writePack(objects []GitObject) []byte {
	var body bytes.Buffer
	header := make([]byte, 12)
	copy(header, "PACK")
	binary.BigEndian.PutUint32(header[4:], 2)
	binary.BigEndian.PutUint32(header[8:], uint32(len(objects)))
	body.Write(header)
	for _, obj := range objects {
		body.Write(writePackObjectHeader(obj.Type, len(obj.Data)))
		zw, _ := zlib.NewWriterLevel(&body, 1)
		zw.Write(obj.Data)
		zw.Close()
	}
	sum := sha1.Sum(body.Bytes())
	out := append(body.Bytes(), sum[:]...)
	return out
}

// --- delta application ---

func applyDelta(base, delta []byte) ([]byte, error) {
	off := 0
	readSize := func() (int, error) {
		value := 0
		shift := 0
		for {
			if off >= len(delta) {
				return 0, errors.New("truncated delta size")
			}
			b := delta[off]
			off++
			value += int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return value, nil
	}
	sourceSize, err := readSize()
	if err != nil {
		return nil, err
	}
	targetSize, err := readSize()
	if err != nil {
		return nil, err
	}
	if sourceSize != len(base) {
		return nil, fmt.Errorf("delta expects a %d-byte base, got %d", sourceSize, len(base))
	}
	out := make([]byte, targetSize)
	written := 0
	for off < len(delta) {
		op := delta[off]
		off++
		if op&0x80 != 0 {
			copyOffset := 0
			copySize := 0
			shift := 0
			for bit := 0; bit < 4; bit++ {
				if op&(1<<bit) != 0 {
					if off >= len(delta) {
						return nil, errors.New("truncated delta copy")
					}
					copyOffset += int(delta[off]) << shift
					off++
				}
				shift += 8
			}
			shift = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<bit) != 0 {
					if off >= len(delta) {
						return nil, errors.New("truncated delta copy")
					}
					copySize += int(delta[off]) << shift
					off++
				}
				shift += 8
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			copy(out[written:written+copySize], base[copyOffset:copyOffset+copySize])
			written += copySize
		} else if op != 0 {
			literal := int(op & 0x7f)
			copy(out[written:written+literal], delta[off:off+literal])
			off += literal
			written += literal
		} else {
			return nil, errors.New("delta opcode 0 is invalid")
		}
	}
	if written != targetSize {
		return nil, fmt.Errorf("delta produced %d of %d bytes", written, targetSize)
	}
	return out, nil
}

// --- packfile read ---

func readVarintSize(buf []byte, start int) (size int, next int, first byte, err error) {
	if start >= len(buf) {
		return 0, 0, 0, errors.New("truncated pack header")
	}
	b := buf[start]
	first = b
	size = int(b & 0x0f)
	shift := 4
	off := start + 1
	for b&0x80 != 0 {
		if off >= len(buf) {
			return 0, 0, 0, errors.New("truncated pack varint")
		}
		b = buf[off]
		off++
		size += int(b&0x7f) << shift
		shift += 7
	}
	return size, off, first, nil
}

func readOfsDeltaBase(buf []byte, start int) (baseOffset int, next int, err error) {
	if start >= len(buf) {
		return 0, 0, errors.New("truncated ofs-delta base")
	}
	b := buf[start]
	off := start + 1
	value := int(b & 0x7f)
	for b&0x80 != 0 {
		if off >= len(buf) {
			return 0, 0, errors.New("truncated ofs-delta base")
		}
		b = buf[off]
		off++
		value = (value+1)*128 + int(b&0x7f)
	}
	return value, off, nil
}

// inflateAt inflates one zlib stream starting at offset, stopping at end.
// It returns the decompressed data and the offset just past the stream.
func inflateAt(buf []byte, offset, end int) ([]byte, int, error) {
	view := buf[offset:end]
	br := bytes.NewReader(view)
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return nil, 0, err
	}
	consumed := len(view) - br.Len()
	return data, offset + consumed, nil
}

type packEntry struct {
	offset     int
	typeCode   int
	size       int
	data       []byte // inflated payload (delta data for delta entries)
	baseOffset int
	baseOid    string
	resolved   *GitObject
	oid        string
}

type PackReadResult struct {
	Objects    []GitObject // in first-occurrence order
	Oids       []string
	DeltaCount int
}

// readPack reads a packfile, resolving OFS/REF deltas. resolveBase supplies
// thin-pack bases from the store.
func readPack(pack []byte, resolveBase ObjectLookup) (*PackReadResult, error) {
	if len(pack) < 32 || string(pack[0:4]) != "PACK" {
		return nil, errors.New("not a packfile")
	}
	version := binary.BigEndian.Uint32(pack[4:])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	declaredCount := binary.BigEndian.Uint32(pack[8:])
	trailerStart := len(pack) - 20

	var raw []*packEntry
	byOffset := map[int]*packEntry{}
	offset := 12
	for i := uint32(0); i < declaredCount; i++ {
		entryOffset := offset
		size, next, first, err := readVarintSize(pack, offset)
		if err != nil {
			return nil, err
		}
		offset = next
		typeCode := int((first >> 4) & 0x07)
		entry := &packEntry{offset: entryOffset, typeCode: typeCode, size: size}
		if typeCode == 6 {
			base, next2, err := readOfsDeltaBase(pack, offset)
			if err != nil {
				return nil, err
			}
			offset = next2
			entry.baseOffset = entryOffset - base
		} else if typeCode == 7 {
			if offset+20 > len(pack) {
				return nil, errors.New("truncated ref-delta base")
			}
			entry.baseOid = hex.EncodeToString(pack[offset : offset+20])
			offset += 20
		} else if codeTypes[typeCode] == "" {
			return nil, fmt.Errorf("pack entry %d has unknown type code %d", i, typeCode)
		}
		inflated, nextOff, err := inflateAt(pack, offset, trailerStart)
		if err != nil {
			return nil, err
		}
		if len(inflated) != size {
			return nil, fmt.Errorf("pack entry at %d declares %d bytes but inflated to %d", entryOffset, size, len(inflated))
		}
		entry.data = inflated
		offset = nextOff
		raw = append(raw, entry)
		byOffset[entryOffset] = entry
	}

	resolvedByOid := map[string]*packEntry{}
	resolving := map[int]bool{}
	deltaCount := 0
	var resolve func(e *packEntry) (*GitObject, string, error)
	resolve = func(e *packEntry) (*GitObject, string, error) {
		if e.resolved != nil {
			return e.resolved, e.oid, nil
		}
		if resolving[e.offset] {
			return nil, "", fmt.Errorf("delta cycle at pack offset %d", e.offset)
		}
		resolving[e.offset] = true
		var typ string
		var data []byte
		switch e.typeCode {
		case 6:
			baseEntry := byOffset[e.baseOffset]
			if baseEntry == nil {
				return nil, "", fmt.Errorf("ofs-delta base at offset %d is not in the pack", e.baseOffset)
			}
			base, _, err := resolve(baseEntry)
			if err != nil {
				return nil, "", err
			}
			typ = base.Type
			data, err = applyDelta(base.Data, e.data)
			if err != nil {
				return nil, "", err
			}
		case 7:
			if baseEntry, ok := resolvedByOid[e.baseOid]; ok {
				base, _, err := resolve(baseEntry)
				if err != nil {
					return nil, "", err
				}
				typ = base.Type
				data, err = applyDelta(base.Data, e.data)
				if err != nil {
					return nil, "", err
				}
			} else {
				var external *GitObject
				if resolveBase != nil {
					external = resolveBase(e.baseOid)
				}
				if external == nil {
					return nil, "", fmt.Errorf("ref-delta base %s is neither in the pack nor supplied", e.baseOid)
				}
				typ = external.Type
				var err error
				data, err = applyDelta(external.Data, e.data)
				if err != nil {
					return nil, "", err
				}
			}
		default:
			typ = codeTypes[e.typeCode]
			data = e.data
		}
		oid := hashObject(typ, data)
		obj := &GitObject{Type: typ, Data: data}
		e.resolved = obj
		e.oid = oid
		resolvedByOid[oid] = e
		delete(resolving, e.offset)
		return obj, oid, nil
	}

	result := &PackReadResult{}
	seen := map[string]bool{}
	for _, entry := range raw {
		resolved, oid, err := resolve(entry)
		if err != nil {
			return nil, err
		}
		if entry.typeCode == 6 || entry.typeCode == 7 {
			deltaCount++
		}
		if !seen[oid] {
			seen[oid] = true
			result.Objects = append(result.Objects, *resolved)
			result.Oids = append(result.Oids, oid)
		}
	}
	result.DeltaCount = deltaCount
	return result, nil
}

// --- fetch planning ---

type FetchPlan struct {
	Objects   []GitObject
	Shallow   []string
	Unshallow []string
}

// addTreeObjects adds a tree and everything under it to out (gitlinks skipped).
func addTreeObjects(rootOid string, lookup ObjectLookup, out map[string]bool) {
	stack := []string{rootOid}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if out[oid] {
			continue
		}
		obj := lookup(oid)
		if obj == nil {
			continue
		}
		out[oid] = true
		if obj.Type == "tree" {
			for _, entry := range parseTree(obj.Data) {
				if entry.Mode == "160000" {
					continue
				}
				stack = append(stack, entry.Oid)
			}
		}
	}
}

// planFetch decides which objects a fetch sends. Objects reachable from a
// have are excluded; a shallow boundary stops the have-walk. Commits are
// included up to `depth` generations from a want (depth 1 = the wants
// themselves); every included commit contributes its whole tree.
func planFetch(wants, haves []string, depth *int, oldShallow []string, lookup ObjectLookup) FetchPlan {
	oldShallowSet := map[string]bool{}
	for _, o := range oldShallow {
		oldShallowSet[o] = true
	}

	// 1. The have set: everything reachable from a have.
	haveObjects := map[string]bool{}
	haveQueue := append([]string{}, haves...)
	for len(haveQueue) > 0 {
		oid := haveQueue[len(haveQueue)-1]
		haveQueue = haveQueue[:len(haveQueue)-1]
		if haveObjects[oid] || !isOid(oid) {
			continue
		}
		obj := lookup(oid)
		if obj == nil {
			continue
		}
		haveObjects[oid] = true
		switch obj.Type {
		case "commit":
			commit := parseCommit(obj.Data)
			addTreeObjects(commit.Tree, lookup, haveObjects)
			if !oldShallowSet[oid] {
				haveQueue = append(haveQueue, commit.Parents...)
			}
		case "tag":
			haveQueue = append(haveQueue, parseTag(obj.Data).Object)
		case "tree":
			addTreeObjects(oid, lookup, haveObjects)
		}
	}

	// 2. BFS from the wants over commits; tags peel to their target.
	maxDist := int(^uint(0) >> 1) // Infinity
	if depth != nil {
		maxDist = *depth - 1
	}
	commitDist := map[string]int{}
	type qItem struct {
		oid  string
		dist int
	}
	var queue []qItem
	for _, want := range wants {
		if !isOid(want) {
			continue
		}
		obj := lookup(want)
		if obj == nil {
			continue
		}
		if obj.Type == "tag" {
			queue = append(queue, qItem{oid: parseTag(obj.Data).Object, dist: 0})
		} else if obj.Type == "commit" {
			queue = append(queue, qItem{oid: want, dist: 0})
		}
	}
	sort.Slice(queue, func(i, j int) bool { return queue[i].oid < queue[j].oid })
	included := map[string]bool{}
	var includedCommits []qItem
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if _, ok := commitDist[cur.oid]; ok || cur.dist > maxDist {
			continue
		}
		commitDist[cur.oid] = cur.dist
		included[cur.oid] = true
		includedCommits = append(includedCommits, cur)
		obj := lookup(cur.oid)
		if obj == nil || obj.Type != "commit" {
			continue
		}
		for _, p := range parseCommit(obj.Data).Parents {
			if _, ok := commitDist[p]; !ok {
				queue = append(queue, qItem{oid: p, dist: cur.dist + 1})
			}
		}
	}

	// 3. Trees + blobs of every included commit, plus tag objects for tag wants.
	for _, want := range wants {
		obj := lookup(want)
		if obj != nil && obj.Type == "tag" {
			included[want] = true
		}
	}
	for _, c := range includedCommits {
		obj := lookup(c.oid)
		if obj == nil {
			continue
		}
		addTreeObjects(parseCommit(obj.Data).Tree, lookup, included)
	}

	// 4. Subtract what the client holds.
	for oid := range haveObjects {
		delete(included, oid)
	}

	// 5. Shallow bookkeeping.
	var shallow, unshallow []string
	if depth != nil {
		for _, c := range includedCommits {
			if c.dist != maxDist {
				continue
			}
			obj := lookup(c.oid)
			if obj == nil || obj.Type != "commit" {
				continue
			}
			parents := parseCommit(obj.Data).Parents
			for _, p := range parents {
				if !included[p] && !haveObjects[p] {
					shallow = append(shallow, c.oid)
					break
				}
			}
		}
		for _, boundary := range oldShallow {
			obj := lookup(boundary)
			if obj == nil || obj.Type != "commit" {
				continue
			}
			parents := parseCommit(obj.Data).Parents
			allHeld := len(parents) == 0
			if len(parents) > 0 {
				allHeld = true
				for _, p := range parents {
					if !included[p] && !haveObjects[p] {
						allHeld = false
						break
					}
				}
			}
			if allHeld {
				if !containsStr(shallow, boundary) {
					unshallow = append(unshallow, boundary)
				}
			} else if !containsStr(shallow, boundary) {
				shallow = append(shallow, boundary)
			}
		}
	}

	oids := make([]string, 0, len(included))
	for oid := range included {
		oids = append(oids, oid)
	}
	sort.Strings(oids)
	objects := make([]GitObject, 0, len(oids))
	for _, oid := range oids {
		if obj := lookup(oid); obj != nil {
			objects = append(objects, *obj)
		}
	}
	sort.Strings(shallow)
	sort.Strings(unshallow)
	return FetchPlan{Objects: objects, Shallow: shallow, Unshallow: unshallow}
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// collectReachable returns every object reachable from the roots.
func collectReachable(roots []string, lookup ObjectLookup) map[string]bool {
	seen := map[string]bool{}
	var queue []string
	for _, r := range roots {
		if isOid(r) {
			queue = append(queue, r)
		}
	}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[oid] {
			continue
		}
		obj := lookup(oid)
		if obj == nil {
			continue
		}
		seen[oid] = true
		switch obj.Type {
		case "commit":
			commit := parseCommit(obj.Data)
			queue = append(queue, commit.Tree)
			queue = append(queue, commit.Parents...)
		case "tag":
			queue = append(queue, parseTag(obj.Data).Object)
		case "tree":
			for _, entry := range parseTree(obj.Data) {
				if entry.Mode != "160000" {
					queue = append(queue, entry.Oid)
				}
			}
		}
	}
	return seen
}
