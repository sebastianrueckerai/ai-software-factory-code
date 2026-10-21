package console

// The editor's save (roadmap 277, the 2026-10-15 plan, item 3): the
// member's whole-text save of a document, one commit on master of the
// project's document repository as the member, with their own Forgejo
// token, through the same pieces the chat's write_doc passes — the
// repository from the project's chat block, the path allow-list, the size
// cap, the roadmap's structure check, the repository's one commit at a
// time and the content-hash compare-and-swap. The door's one doc_written
// event, via console, follows the commit (item 4). A stale base hash no
// longer refuses outright: the door merges the change master took over
// the text at the commit the save names, and only the same lines changed
// on both sides refuse (item 5).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// docSave is the editor's save: the document's whole text, the content
// hash the editor based it on — empty writes a new file — and the head
// commit the editor based it on, which only the editor's facts repeat.
type docSave struct {
	Text   string `json:"text"`
	Base   string `json:"base"`
	Commit string `json:"commit"`
}

// docSaved is the save's answer: the document's new content hash, the
// commit that landed it on master, and that commit's time. A save that
// found the document changed under it and merged that change also names
// it and answers the merged text, which the editor lays in.
type docSaved struct {
	SHA256 string    `json:"sha256"`
	Commit string    `json:"commit"`
	At     time.Time `json:"at"`
	// Merged names the change a stale base merged — "merged with
	// Sebastian's change" — empty when the save landed alone.
	Merged string `json:"merged,omitempty"`
	// Text is the merged text the door committed, so the editor's own
	// becomes it.
	Text string `json:"text,omitempty"`
}

// docRefusal is the save's refusal with the document's current state when
// it changed under the editor: the text and its hash, which the editor
// lays beside the member's own for a merge by eye. When the change met
// the member's own on the same lines, Who, At and Lines also name the
// head commit's author and time and the clashing lines' 1-based [first,
// last] ranges in Current.
type docRefusal struct {
	refusal
	SHA256  string
	Current string
	Who     string
	At      time.Time
	Lines   [][2]int
}

// docWrite answers POST /projects/{name}/docs/{path...}: the editor's
// save, as the member, through the write door's pieces. Whoever sees the
// project's board may try it; the member's own Forgejo token judges the
// commit.
func (c *Console) docWrite(w http.ResponseWriter, r *http.Request) {
	p, ok := c.pathProject(w, r)
	if !ok {
		return
	}
	sees, _, err := c.follows(r, p)
	switch {
	case err != nil:
		http.Error(w, "cannot check the project's writers", http.StatusServiceUnavailable)
		return
	case !sees:
		http.NotFound(w, r)
		return
	}
	var save docSave
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, int64(docMax)*4)).Decode(&save); err != nil {
		http.Error(w, "the save does not read: "+err.Error(), http.StatusBadRequest)
		return
	}
	v, _ := c.inForce(r.Context())
	saved, err := c.doSaveDoc(r.Context(), actor{s: current(r), via: factoryd.ViaConsole},
		v.Document.ChatOf(v.Projects[p.Name]).DocRepo, p.Repository, r.PathValue("path"), save)
	if err != nil {
		c.docRefused(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(saved); err != nil {
		log.Printf("console: the saved document's answer: %v", err)
	}
}

// docRefused answers a save that did not land as JSON the editor shows:
// the reason, and the document's current text and hash when it changed
// under the editor — with the head commit's author and time and the
// clashing lines' ranges when the change met the member's own on the
// same lines.
func (c *Console) docRefused(w http.ResponseWriter, r *http.Request, err error) {
	out := struct {
		Reason  string     `json:"reason"`
		SHA256  string     `json:"sha256,omitempty"`
		Current string     `json:"current,omitempty"`
		Who     string     `json:"who,omitempty"`
		At      *time.Time `json:"at,omitempty"`
		Lines   [][2]int   `json:"lines,omitempty"`
	}{}
	status := http.StatusBadGateway
	var stale *docRefusal
	var fj *forgejoRefusal
	var no *refusal
	switch {
	case errors.As(err, &stale):
		out.Reason, out.SHA256, out.Current, status = stale.msg, stale.SHA256, stale.Current, stale.status
		if stale.Who != "" {
			out.Who, out.At, out.Lines = stale.Who, &stale.At, stale.Lines
		}
	case errors.As(err, &fj):
		log.Printf("console: %s could not %s: %v", current(r).user, fj.what, fj.err)
		out.Reason = "Forgejo refused to " + fj.what + "; see the factoryd log"
	case errors.As(err, &no):
		out.Reason, status = no.msg, no.status
	default:
		log.Printf("console: %s: %v", current(r).user, err)
		out.Reason = "the save did not land; see the factoryd log"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(out); err != nil {
		log.Printf("console: the save's refusal: %v", err)
	}
}

// doSaveDoc lands the editor's save of docPath as one commit on master of
// the project's document repository, made with the member's own Forgejo
// token: the write door's pieces around it, then the door's one
// doc_written event. project is the repository of the project the save
// belongs to, which that event names.
func (c *Console) doSaveDoc(ctx context.Context, a actor, docRepo, project, docPath string, save docSave) (*docSaved, error) {
	repo, err := c.docWriteRepoOf(docRepo, docPath)
	if err != nil {
		return nil, err
	}
	l := docLock(docRepo)
	l.Lock()
	defer l.Unlock()
	current, blob, present, err := c.docFile(ctx, repo, docPath)
	if err != nil {
		return nil, err
	}
	hash := factoryd.ContentHash(current)
	merged := ""
	stale := &docRefusal{refusal{http.StatusConflict,
		fmt.Sprintf("%s changed since it was read: its base_sha256 is now %s, and its current text follows", docPath, hash)}, hash, current, "", time.Time{}, nil}
	switch {
	case present && save.Base != hash:
		// the save's base hash is stale: merge the change master took
		// over the text at the commit the save names (item 5), and only
		// refuse when both changed the same lines
		var base string
		var ok bool
		if base, _, ok, err = c.docFileAt(ctx, repo, docPath, save.Commit); err != nil {
			return nil, err
		}
		if save.Commit == "" || !ok {
			// an editor that named no base commit, or one whose commit
			// the file was not at: no base to merge over
			return nil, stale
		}
		who, at, err := c.docAuthor(ctx, repo, docPath)
		if err != nil {
			return nil, err
		}
		text, clashes, err := docMerge(ctx, save.Text, base, current)
		switch {
		case err != nil:
			log.Printf("console: %s's save of %s on %s does not merge: %v", a.s.user, docPath, docRepo, err)
			return nil, refuse(http.StatusBadGateway, "the save's merge did not run; see the factoryd log")
		case len(clashes) > 0:
			return nil, &docRefusal{refusal{http.StatusConflict,
				fmt.Sprintf("%s changed the same lines (%s)", who, at.Format("15:04"))}, hash, current, who, at, clashes}
		}
		save.Text, merged = text, fmt.Sprintf("merged with %s's change", who)
	case !present && save.Base != "":
		return nil, refuse(http.StatusConflict, docPath+" does not exist on master: save it without a base hash")
	}
	switch {
	case strings.TrimSpace(save.Text) == "":
		return nil, refuse(http.StatusBadRequest, "the document would be empty; the editor does not delete documents")
	case len(save.Text) > docMax:
		return nil, refuse(http.StatusBadRequest, fmt.Sprintf("the document would be %d bytes, over the cap of %d", len(save.Text), docMax))
	}
	if docPath == "docs/roadmap.md" {
		if f := roadmapFindings(save.Text); len(f) > 0 {
			return nil, refuse(http.StatusUnprocessableEntity, "The roadmap save breaks the file's structure: "+strings.Join(f, "; ")+".")
		}
	}
	commit, at := "", time.Time{}
	if present && save.Text == current {
		if commit, at, err = c.docCommit(ctx, repo, docPath); err != nil {
			return nil, err
		}
		return &docSaved{SHA256: hash, Commit: commit, At: at, Merged: merged}, nil
	}
	change := map[string]string{"operation": "create", "path": docPath, "content": base64.StdEncoding.EncodeToString([]byte(save.Text))}
	if present {
		change["operation"], change["sha"] = "update", blob
	}
	var landed struct {
		Commit struct {
			SHA     string    `json:"sha"`
			Created time.Time `json:"created"`
		} `json:"commit"`
	}
	if err := c.as(ctx, a.s, http.MethodPost, docAPI(repo)+"/contents", map[string]any{
		"branch": "master", "message": a.attribution().Message(docPath + ": edited in the console"), "files": []map[string]string{change}},
		http.StatusCreated, &landed); err != nil {
		return nil, &forgejoRefusal{"commit the document", err}
	}
	// The commit is on master: whatever the head-commit read answers a
	// beat later, the save has landed and owes its event, so it answers
	// success; a docCommit that fails leaves the contents answer's own
	// time in its place.
	if _, at, err = c.docCommit(ctx, repo, docPath); err != nil {
		log.Printf("console: %s saved %s on %s; its commit's facts do not read: %v", a.s.user, docPath, docRepo, err)
		at = landed.Commit.Created
	}
	c.docWritten(ctx, a, project, docPath, factoryd.ContentHash(save.Text), landed.Commit.SHA)
	if merged != "" {
		return &docSaved{SHA256: factoryd.ContentHash(save.Text), Commit: landed.Commit.SHA, At: at,
			Merged: merged, Text: save.Text}, nil
	}
	return &docSaved{SHA256: factoryd.ContentHash(save.Text), Commit: landed.Commit.SHA, At: at}, nil
}
