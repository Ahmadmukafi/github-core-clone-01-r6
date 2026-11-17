// Git smart HTTP: info/refs advertisements, upload-pack v2 (ls-refs/fetch),
// and receive-pack v0 (push). Byte-identical wire behaviour to the
// reference forge.
package main

import (
	"sort"
	"strings"
)

type HttpResult struct {
	Status  int
	Headers map[string]string
	Body    []byte
}

func textResult(status int, body string, contentType string) HttpResult {
	if contentType == "" {
		contentType = "text/plain"
	}
	return HttpResult{Status: status, Headers: map[string]string{"Content-Type": contentType}, Body: []byte(body)}
}

func gitNotFound(message string) HttpResult {
	return textResult(404, message, "")
}

const uploadPackService = "git-upload-pack"
const receivePackService = "git-receive-pack"

var v2Capabilities = []string{
	"ls-refs=unborn",
	"fetch=shallow filter",
	"object-format=sha1",
	"agent=github-core-clone/1.0.0",
}

func sortedRefs(repo *Repo) [][2]string {
	var out [][2]string
	for name, oid := range repo.Refs {
		out = append(out, [2]string{name, oid})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// advertisementV2 renders the v2 capability advertisement (cached by version).
func advertisementV2(repo *Repo) []byte {
	repo.CacheMu.Lock()
	defer repo.CacheMu.Unlock()
	if repo.Adv2Body != nil && repo.Adv2Version == repo.Version {
		return repo.Adv2Body
	}
	var items []any
	items = append(items, "version 2\n")
	for _, cap := range v2Capabilities {
		items = append(items, cap+"\n")
	}
	items = append(items, "flush")
	body := append(pktLineStr("# service="+uploadPackService+"\n"), pktFlush...)
	body = append(body, pktLines(items)...)
	repo.Adv2Version = repo.Version
	repo.Adv2Body = body
	return body
}

// advertisementV0 renders the v0 ref advertisement (cached per service by
// version).
func advertisementV0(repo *Repo, service string) []byte {
	repo.CacheMu.Lock()
	defer repo.CacheMu.Unlock()
	if cached, ok := repo.Adv0[service]; ok && cached.Version == repo.Version {
		return cached.Body
	}
	defaultRef := "refs/heads/" + repo.DefaultBranch
	var capabilities string
	if service == uploadPackService {
		capabilities = "multi_ack thin-pack side-band side-band-64k ofs-delta shallow deepen-since deepen-not deepen-relative no-progress include-tag multi_ack_detailed allow-tip-sha1-in-want allow-reachable-sha1-in-want no-done symref=HEAD:" + defaultRef + " object-format=sha1"
	} else {
		capabilities = "report-status side-band-64k delete-refs object-format=sha1 symref=HEAD:" + defaultRef
	}
	var lines []any
	refs := sortedRefs(repo)
	if len(refs) == 0 {
		lines = append(lines, zeroOid+" capabilities^{}\x00"+capabilities+"\n")
	} else {
		headOid, ok := repo.Refs[defaultRef]
		if !ok {
			headOid = zeroOid
		}
		lines = append(lines, headOid+" HEAD\x00"+capabilities+"\n")
		for _, r := range refs {
			name, oid := r[0], r[1]
			lines = append(lines, oid+" "+name+"\n")
			if strings.HasPrefix(name, "refs/tags/") {
				if obj := repo.Objects[oid]; obj != nil && obj.Type == "tag" {
					if target, ok := peelToCommit(repo, oid); ok {
						lines = append(lines, target+" "+name+"^{}\n")
					}
				}
			}
		}
	}
	lines = append(lines, "flush")
	body := append(pktLineStr("# service="+service+"\n"), pktFlush...)
	body = append(body, pktLines(lines)...)
	repo.Adv0[service] = adv0Entry{Version: repo.Version, Body: body}
	return body
}

// infoRefs answers GET /{owner}/{repo}.git/info/refs?service=... — v2 when
// the Git-Protocol header asks for it.
func infoRefs(repo *Repo, service string, gitProtocol string) HttpResult {
	if service != uploadPackService && service != receivePackService {
		return textResult(400, "unsupported service "+service, "")
	}
	if service == uploadPackService && gitProtocol == "version=2" {
		return HttpResult{Status: 200, Headers: map[string]string{"Content-Type": "application/x-" + uploadPackService + "-advertisement"}, Body: advertisementV2(repo)}
	}
	return HttpResult{Status: 200, Headers: map[string]string{"Content-Type": "application/x-" + service + "-advertisement"}, Body: advertisementV0(repo, service)}
}

// --- upload-pack v2 ---

type FetchRequest struct {
	Wants   []string
	Haves   []string
	Depth   *int
	Filter  *string
	Shallow []string
	Done    bool
}

type uploadPackRequest struct {
	Kind    string // error | ls-refs | fetch
	Message string
	Unborn  bool
	Peel    bool
	Symrefs bool
	Prefixes []string
	Fetch   *FetchRequest
}

func parseUploadPackRequest(packets []Packet) uploadPackRequest {
	raw := packetLines(packets)
	lines := make([]string, len(raw))
	for i, l := range raw {
		lines[i] = stripTrailingNewline(l)
	}
	if len(lines) == 0 {
		return uploadPackRequest{Kind: "error", Message: "empty upload-pack request"}
	}
	first := lines[0]
	command := first
	if i := strings.IndexByte(first, ' '); i >= 0 {
		command = first[:i]
	}
	if command == "command=ls-refs" {
		args := lines[1:]
		var prefixes []string
		for _, arg := range args {
			if strings.HasPrefix(arg, "ref-prefix ") {
				prefixes = append(prefixes, strings.TrimSpace(arg[len("ref-prefix "):]))
			}
		}
		has := func(s string) bool {
			for _, a := range args {
				if a == s {
					return true
				}
			}
			return false
		}
		return uploadPackRequest{Kind: "ls-refs", Unborn: has("unborn"), Peel: has("peel"), Symrefs: has("symrefs"), Prefixes: prefixes}
	}
	if command == "command=fetch" {
		req := &FetchRequest{}
		for _, line := range lines[1:] {
			switch {
			case line == "done":
				req.Done = true
			case line == "":
			case strings.HasPrefix(line, "want "):
				oid := strings.TrimSpace(line[5:])
				if isOid(oid) {
					req.Wants = append(req.Wants, oid)
				}
			case strings.HasPrefix(line, "have "):
				oid := strings.TrimSpace(line[5:])
				if isOid(oid) {
					req.Haves = append(req.Haves, oid)
				}
			case strings.HasPrefix(line, "deepen "):
				if v, ok := jsParseInt(strings.TrimSpace(line[7:])); ok && v > 0 {
					d := int(v)
					req.Depth = &d
				}
			case strings.HasPrefix(line, "depth "):
				if v, ok := jsParseInt(strings.TrimSpace(line[6:])); ok && v > 0 {
					d := int(v)
					req.Depth = &d
				}
			case strings.HasPrefix(line, "filter "):
				f := strings.TrimSpace(line[7:])
				req.Filter = &f
			case strings.HasPrefix(line, "shallow "):
				oid := strings.TrimSpace(line[8:])
				if isOid(oid) {
					req.Shallow = append(req.Shallow, oid)
				}
			}
		}
		return uploadPackRequest{Kind: "fetch", Fetch: req}
	}
	return uploadPackRequest{Kind: "error", Message: "unsupported upload-pack command " + command}
}

func lsRefsResponse(repo *Repo, req uploadPackRequest) []byte {
	cacheKey := "lsrefs:" + intStr(repo.Version) + ":" + boolStr(req.Peel) + ":" + boolStr(req.Symrefs) + ":" + strings.Join(req.Prefixes, ",") + ":" + boolStr(req.Unborn)
	repo.CacheMu.Lock()
	if cached, ok := repo.LsRefsCache[cacheKey]; ok {
		repo.CacheMu.Unlock()
		return cached.Body
	}
	repo.CacheMu.Unlock()

	keep := func(name string) bool {
		if len(req.Prefixes) == 0 {
			return true
		}
		for _, p := range req.Prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	defaultRef := "refs/heads/" + repo.DefaultBranch
	headOid, hasHead := repo.Refs[defaultRef]
	var lines []any
	if req.Symrefs && hasHead && keep("HEAD") {
		lines = append(lines, headOid+" HEAD symref-target:"+defaultRef+"\n")
	}
	for _, r := range sortedRefs(repo) {
		name, oid := r[0], r[1]
		if !keep(name) {
			continue
		}
		line := oid + " " + name
		if req.Peel {
			if obj := repo.Objects[oid]; obj != nil && obj.Type == "tag" {
				if target, ok := peelToCommit(repo, oid); ok {
					line += " peeled:" + target
				}
			}
		}
		lines = append(lines, line+"\n")
	}
	if len(repo.Refs) == 0 && req.Unborn && keep("HEAD") {
		lines = append(lines, "unborn HEAD symref-target:"+defaultRef+"\n")
	}
	lines = append(lines, "flush")
	body := pktLines(lines)

	repo.CacheMu.Lock()
	if len(repo.LsRefsCache) > 64 {
		repo.LsRefsCache = map[string]lsRefsEntry{}
	}
	repo.LsRefsCache[cacheKey] = lsRefsEntry{Version: repo.Version, Body: body}
	repo.CacheMu.Unlock()
	return body
}

func fetchResponse(repo *Repo, request *FetchRequest) []byte {
	// Negotiation: the server answers with an acknowledgments section first
	// when haves arrived without `done`.
	var acks []string
	if len(request.Haves) > 0 && !request.Done {
		for _, have := range request.Haves {
			if _, ok := repo.Objects[have]; ok {
				acks = append(acks, have)
			}
		}
		if len(acks) == 0 {
			return pktLines([]any{"acknowledgments\n", "NAK\n", "flush"})
		}
	}
	sortedCopy := func(in []string) []string {
		out := append([]string{}, in...)
		sort.Strings(out)
		return out
	}
	depthStr := ""
	if request.Depth != nil {
		depthStr = intStr(*request.Depth)
	}
	cacheKey := "pack:" + strings.Join(sortedCopy(request.Wants), ",") + "|" +
		strings.Join(sortedCopy(request.Haves), ",") + "|" + depthStr + "|" +
		strings.Join(sortedCopy(request.Shallow), ",") + "|" + boolStr(request.Done, "d", "")
	repo.CacheMu.Lock()
	if cached, ok := repo.PackCache[cacheKey]; ok {
		repo.CacheMu.Unlock()
		return cached.Encoded
	}
	repo.CacheMu.Unlock()

	plan := planFetch(request.Wants, request.Haves, request.Depth, request.Shallow, lookupOf(repo))
	pack := writePack(plan.Objects)
	encoded := encodeFetchResponse(request.Wants, pack, plan.Shallow, plan.Unshallow, acks)

	repo.CacheMu.Lock()
	if len(repo.PackCache) >= 128 {
		first := repo.PackCacheOrder[0]
		delete(repo.PackCache, first)
		repo.PackCacheOrder = repo.PackCacheOrder[1:]
	}
	repo.PackCache[cacheKey] = &PackCacheEntry{Pack: pack, Shallow: plan.Shallow, Unshallow: plan.Unshallow, Encoded: encoded}
	repo.PackCacheOrder = append(repo.PackCacheOrder, cacheKey)
	repo.CacheMu.Unlock()
	return encoded
}

func encodeFetchResponse(wants []string, pack []byte, shallow, unshallow, acks []string) []byte {
	_ = wants
	var sections [][]byte
	if len(acks) > 0 {
		var ackLines []any
		ackLines = append(ackLines, "acknowledgments\n")
		for _, ack := range acks {
			ackLines = append(ackLines, "ACK "+ack+"\n")
		}
		ackLines = append(ackLines, "ready\n", "delim")
		sections = append(sections, pktLines(ackLines))
	}
	if len(shallow) > 0 || len(unshallow) > 0 {
		var shallowLines []any
		shallowLines = append(shallowLines, "shallow-info\n")
		for _, oid := range shallow {
			shallowLines = append(shallowLines, "shallow "+oid+"\n")
		}
		for _, oid := range unshallow {
			shallowLines = append(shallowLines, "unshallow "+oid+"\n")
		}
		shallowLines = append(shallowLines, "delim")
		sections = append(sections, pktLines(shallowLines))
	}
	if len(pack) > 0 {
		var packets [][]byte
		packets = append(packets, pktLineStr("packfile\n"))
		const chunk = 65515
		for offset := 0; offset < len(pack); offset += chunk {
			end := offset + chunk
			if end > len(pack) {
				end = len(pack)
			}
			framed := make([]byte, 0, end-offset+1)
			framed = append(framed, 1)
			framed = append(framed, pack[offset:end]...)
			packets = append(packets, pktLine(framed))
		}
		packets = append(packets, pktFlush)
		var section []byte
		for _, p := range packets {
			section = append(section, p...)
		}
		sections = append(sections, section)
	}
	var out []byte
	for _, s := range sections {
		out = append(out, s...)
	}
	return out
}

func uploadPackError(message string) []byte {
	return pktLines([]any{"ERR " + message + "\n", "flush"})
}

// handleUploadPack answers POST /{owner}/{repo}.git/git-upload-pack.
func handleUploadPack(repo *Repo, body []byte) HttpResult {
	packets, _ := decodePackets(body)
	request := parseUploadPackRequest(packets)
	if request.Kind == "error" {
		return HttpResult{Status: 200, Headers: map[string]string{"Content-Type": "application/x-git-upload-pack-result"}, Body: uploadPackError(request.Message)}
	}
	if request.Kind == "ls-refs" {
		return HttpResult{Status: 200, Headers: map[string]string{"Content-Type": "application/x-git-upload-pack-result"}, Body: lsRefsResponse(repo, request)}
	}
	for _, want := range request.Fetch.Wants {
		if repo.Objects[want] == nil {
			return HttpResult{Status: 200, Headers: map[string]string{"Content-Type": "application/x-git-upload-pack-result"}, Body: uploadPackError("want " + want + " not found")}
		}
	}
	return HttpResult{Status: 200, Headers: map[string]string{"Content-Type": "application/x-git-upload-pack-result"}, Body: fetchResponse(repo, request.Fetch)}
}

// --- receive-pack v0 ---

type receiveRef struct {
	OldOid string
	NewOid string
	Name   string
}

type receivePackRequest struct {
	Refs []receiveRef
	Pack []byte
}

func parseReceivePackRequest(body []byte) receivePackRequest {
	packets, trailing := decodePackets(body)
	var refs []receiveRef
	for _, line := range packetLines(packets) {
		parts := strings.Split(strings.TrimSpace(line), " ")
		if len(parts) >= 3 {
			oldOid, newOid, rawName := parts[0], parts[1], parts[2]
			name := rawName
			if i := strings.IndexByte(rawName, 0); i >= 0 {
				name = rawName[:i]
			}
			if isOid(oldOid) && isOid(newOid) && strings.HasPrefix(name, "refs/") {
				refs = append(refs, receiveRef{OldOid: oldOid, NewOid: newOid, Name: name})
				continue
			}
		}
	}
	return receivePackRequest{Refs: refs, Pack: trailing}
}

func reportStatus(results []receiveRefResult) []byte {
	var lines [][]byte
	lines = append(lines, pktLineStr("unpack ok\n"))
	for _, r := range results {
		if r.Status == "ok" {
			lines = append(lines, pktLineStr("ok "+r.Name+"\n"))
		} else {
			reason := r.Reason
			if reason == "" {
				reason = "failed"
			}
			lines = append(lines, pktLineStr("ng "+r.Name+" "+reason+"\n"))
		}
	}
	lines = append(lines, pktFlush)
	var inner []byte
	for _, l := range lines {
		inner = append(inner, l...)
	}
	var parts [][]byte
	const chunk = 65515
	for offset := 0; offset < len(inner); offset += chunk {
		end := offset + chunk
		if end > len(inner) {
			end = len(inner)
		}
		framed := make([]byte, 0, end-offset+1)
		framed = append(framed, 1)
		framed = append(framed, inner[offset:end]...)
		parts = append(parts, pktLine(framed))
	}
	parts = append(parts, pktFlush)
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

type receiveRefResult struct {
	Name   string
	Status string // ok | ng
	Reason string
}

// handleReceivePack answers POST /{owner}/{repo}.git/git-receive-pack.
func handleReceivePack(repo *Repo, body []byte) HttpResult {
	request := parseReceivePackRequest(body)
	var objects []*GitObject
	var oids []string
	if len(request.Pack) > 0 {
		read, err := readPack(request.Pack, lookupOf(repo))
		if err != nil {
			var results []receiveRefResult
			for _, r := range request.Refs {
				results = append(results, receiveRefResult{Name: r.Name, Status: "ng", Reason: "unpack failed"})
			}
			return HttpResult{Status: 200, Headers: map[string]string{"Content-Type": "application/x-git-receive-pack-result"}, Body: reportStatus(results)}
		}
		for i, obj := range read.Objects {
			objects = append(objects, &obj)
			oids = append(oids, read.Oids[i])
		}
	}
	// Store the pack objects first: every valid ref update below needs them.
	for i, object := range objects {
		oid := oids[i]
		if _, exists := repo.Objects[oid]; !exists {
			repo.Objects[oid] = object
			if object.Type == "commit" {
				repo.CommitSeq[oid] = repo.NextCommitSeq
				repo.NextCommitSeq++
			}
		}
	}
	lookup := lookupOf(repo)
	var results []receiveRefResult
	applied := false
	for _, ref := range request.Refs {
		current, hasCurrent := repo.Refs[ref.Name]
		if ref.NewOid == zeroOid {
			if !hasCurrent {
				results = append(results, receiveRefResult{Name: ref.Name, Status: "ng", Reason: "ref does not exist"})
				continue
			}
			delete(repo.Refs, ref.Name)
			applied = true
			results = append(results, receiveRefResult{Name: ref.Name, Status: "ok"})
			continue
		}
		target := repo.Objects[ref.NewOid]
		if target == nil {
			results = append(results, receiveRefResult{Name: ref.Name, Status: "ng", Reason: "object not found"})
			continue
		}
		if hasCurrent {
			if current != ref.OldOid {
				results = append(results, receiveRefResult{Name: ref.Name, Status: "ng", Reason: "old value mismatch"})
				continue
			}
			oldCommit, okOld := peelToCommitWith(lookup, current)
			newCommit, okNew := peelToCommitWith(lookup, ref.NewOid)
			if okOld && okNew && !isAncestor(oldCommit, newCommit, lookup) {
				results = append(results, receiveRefResult{Name: ref.Name, Status: "ng", Reason: "non-fast-forward"})
				continue
			}
		}
		repo.Refs[ref.Name] = ref.NewOid
		applied = true
		results = append(results, receiveRefResult{Name: ref.Name, Status: "ok"})
	}
	if applied {
		repo.Version++
	}
	return HttpResult{Status: 200, Headers: map[string]string{"Content-Type": "application/x-git-receive-pack-result"}, Body: reportStatus(results)}
}

// listRefs returns HEAD + every ref (peel tags), sorted by ref name.
func listRefs(repo *Repo) []struct {
	Ref string
	Sha string
} {
	var out []struct {
		Ref string
		Sha string
	}
	for name, oid := range repo.Refs {
		out = append(out, struct {
			Ref string
			Sha string
		}{Ref: name, Sha: oid})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}

func intStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func boolStr(b bool, trueVal ...string) string {
	if b {
		if len(trueVal) > 0 {
			return trueVal[0]
		}
		return "true"
	}
	if len(trueVal) > 1 {
		return trueVal[1]
	}
	return "false"
}
