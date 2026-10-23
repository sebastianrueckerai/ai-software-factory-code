package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// Visioner is one look at an image on a task's account, purpose vision:
// OperatorModel.Vision, the chat's look.
type Visioner interface {
	Vision(ctx context.Context, taskID, alias, thread, ask string, image []byte, contentType string) (string, error)
}

// imageReadTimeout bounds the read of the image a look names, from
// Forgejo or the session's pod; the vision call has its own.
const imageReadTimeout = 20 * time.Second

// imageExts are the file name extensions of a card's images.
var imageExts = []string{".png", ".jpg", ".jpeg", ".webp", ".gif"}

// attachmentUUID finds an attachment's uuid in a card_image, its
// /attachments/ link or the uuid alone.
var attachmentUUID = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// lookRefusal is a look factoryd refuses: status, and the one sentence
// the agent corrects its call by.
type lookRefusal struct {
	status   int
	sentence string
}

func (e *lookRefusal) Error() string { return e.sentence }

// ImageLookHandler is POST /internal/image-look, the gateway's hand-over
// of a session's analyze_image: {task_id, path or card_image, ask} answers
// {"text"}, the image's name and what the task's vision model saw. A card
// image is one attached to the card's issue, its comments, or its parent
// card's; a path is read from the task's running session's workspace.
// Every refusal is one sentence.
func (r *Runner) ImageLookHandler(v Visioner) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			TaskID    string `json:"task_id"`
			Path      string `json:"path"`
			CardImage string `json:"card_image"`
			Ask       string `json:"ask"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 64<<10)).Decode(&in); err != nil {
			http.Error(w, "body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if in.TaskID == "" || (in.Path == "") == (in.CardImage == "") {
			http.Error(w, "task_id and exactly one of path and card_image are required", http.StatusBadRequest)
			return
		}
		ctx := req.Context()
		t, ok, err := r.store.Task(ctx, in.TaskID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "no task "+in.TaskID, http.StatusNotFound)
			return
		}
		if Final(t.Status) {
			http.Error(w, "task "+in.TaskID+" is "+t.Status, http.StatusConflict)
			return
		}
		alias := r.cfg.Policy().Document.FactoryModel(t.RequiredCapabilities, policy.PurposeVision)
		if alias == "" {
			http.Error(w, fmt.Sprintf("the classes %q of task %s name no vision_model, which a look calls", t.RequiredCapabilities, t.ID), http.StatusConflict)
			return
		}
		rctx, cancel := context.WithTimeout(ctx, imageReadTimeout)
		var name, from string
		var data []byte
		if in.Path != "" {
			from = "the session's sandbox"
			name, data, err = r.workspaceImage(rctx, t, in.Path)
		} else {
			from = "Forgejo"
			name, data, err = r.cardImage(rctx, t, in.CardImage)
		}
		cancel()
		var no *lookRefusal
		if errors.As(err, &no) {
			http.Error(w, no.sentence, no.status)
			return
		}
		if err != nil {
			log.Printf("image look of task %s at %s: %v", t.ID, name, err)
			http.Error(w, fmt.Sprintf("%s did not answer the read of %s: try again shortly", from, name), http.StatusBadGateway)
			return
		}
		ct := http.DetectContentType(data)
		if !slices.Contains(policy.LookTypes, ct) {
			http.Error(w, fmt.Sprintf("%s is %s, not a PNG, JPEG, WebP or GIF image", name, ct), http.StatusUnsupportedMediaType)
			return
		}
		text, err := v.Vision(ctx, t.ID, alias, "", in.Ask, data, ct)
		var refused *policy.Refusal
		switch {
		case errors.As(err, &refused):
			http.Error(w, fmt.Sprintf("the look at %s was refused (%s): %s", name, refused.Decision, refused.Reason), http.StatusConflict)
			return
		case err != nil:
			log.Printf("image look of task %s at %s: %v", t.ID, name, err)
			http.Error(w, fmt.Sprintf("the vision model did not answer on %s: try again shortly", name), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"text": name + ": " + text}) // #nosec G104 -- best-effort response write
	}
}

// cardImage is the image ref names among those attached to task t's issue
// and its comments, then its parent card's: by uuid when ref holds one,
// else by name, with or without its extension.
func (r *Runner) cardImage(ctx context.Context, t Task, ref string) (string, []byte, error) {
	repo, err := SplitRepo(t.Repository)
	if err != nil {
		return ref, nil, err
	}
	var issues []int64
	if t.IssueNumber != 0 {
		issues = append(issues, t.IssueNumber)
	}
	if t.Parent != "" {
		p, ok, err := r.store.Task(ctx, t.Parent)
		if err != nil {
			return ref, nil, err
		}
		if ok && p.IssueNumber != 0 && p.Repository == t.Repository && p.IssueNumber != t.IssueNumber {
			issues = append(issues, p.IssueNumber)
		}
	}
	if len(issues) == 0 {
		return ref, nil, &lookRefusal{http.StatusConflict, fmt.Sprintf("task %s has no issue, so no card image: look at a workspace file with path", t.ID)}
	}
	var images []IssueAsset
	for _, n := range issues {
		assets, err := r.forgejo.IssueAssets(ctx, repo.Owner, repo.Name, n)
		if err != nil {
			return ref, nil, err
		}
		comments, err := r.forgejo.ListIssueComments(ctx, repo.Owner, repo.Name, n)
		if errors.Is(err, ErrAnswerTooLong) {
			return ref, nil, &lookRefusal{http.StatusConflict, fmt.Sprintf("the comments of card %s#%d are longer than 1 MiB, so their images are not read: look at an image of the issue itself or a workspace file", t.Repository, n)}
		}
		if err != nil {
			return ref, nil, err
		}
		for _, c := range comments {
			assets = append(assets, c.Assets...)
		}
		for _, a := range assets {
			if slices.Contains(imageExts, strings.ToLower(path.Ext(a.Name))) {
				images = append(images, a)
			}
		}
	}
	card := fmt.Sprintf("card %s#%d", t.Repository, issues[0])
	if len(images) == 0 {
		return ref, nil, &lookRefusal{http.StatusNotFound, fmt.Sprintf("%s carries no image: a workspace file is looked at with path", card)}
	}
	uuid := attachmentUUID.FindString(ref)
	var found []IssueAsset
	for _, a := range images {
		if uuid != "" && a.UUID == uuid || uuid == "" && (a.Name == ref || strings.TrimSuffix(a.Name, path.Ext(a.Name)) == ref) {
			found = append(found, a)
		}
	}
	switch {
	case len(found) == 0:
		return ref, nil, &lookRefusal{http.StatusNotFound, fmt.Sprintf("no image %s on %s: its images are %s", ref, card, imageNames(images))}
	case len(found) > 1:
		links := make([]string, len(found))
		for i, a := range found {
			links[i] = "/attachments/" + a.UUID
		}
		return ref, nil, &lookRefusal{http.StatusConflict, fmt.Sprintf("%d images on %s are named %s: name one by its link, %s", len(found), card, ref, strings.Join(links, ", "))}
	}
	a := found[0]
	if a.Size > policy.MaxLookBytes {
		return a.Name, nil, &lookRefusal{http.StatusRequestEntityTooLarge, fmt.Sprintf("%s on %s is larger than 8 MiB", a.Name, card)}
	}
	data, err := r.forgejo.Attachment(ctx, a.UUID, policy.MaxLookBytes)
	if errors.Is(err, ErrAnswerTooLong) {
		return a.Name, nil, &lookRefusal{http.StatusRequestEntityTooLarge, fmt.Sprintf("%s on %s is larger than 8 MiB", a.Name, card)}
	}
	return a.Name, data, err
}

// imageNames lists a card's images by name, a name several share with
// each one's link.
func imageNames(images []IssueAsset) string {
	n := map[string]int{}
	for _, a := range images {
		n[a.Name]++
	}
	out := make([]string, len(images))
	for i, a := range images {
		out[i] = a.Name
		if n[a.Name] > 1 {
			out[i] += " (/attachments/" + a.UUID + ")"
		}
	}
	return strings.Join(out, ", ")
}

// workspaceImage is file p of task t's running session's workspace, which
// the pod's worker reads (POST /image): its refusal is the worker's
// sentence with its status.
func (r *Runner) workspaceImage(ctx context.Context, t Task, p string) (string, []byte, error) {
	live, ok, err := r.store.LiveSession(ctx, t.ID)
	if err != nil {
		return p, nil, err
	}
	if !ok {
		return p, nil, &lookRefusal{http.StatusConflict, fmt.Sprintf("task %s runs no session now: a workspace file is read from the running session's sandbox", t.ID)}
	}
	ip, err := r.kube.PodIP(ctx, live.Namespace, live.Pod)
	if err != nil {
		return p, nil, fmt.Errorf("pod address: %w", err)
	}
	body, err := json.Marshal(map[string]string{"path": p})
	if err != nil {
		return p, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+net.JoinHostPort(ip, strconv.Itoa(r.messagePort()))+"/image", bytes.NewReader(body))
	if err != nil {
		return p, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+live.MessageToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := workerReads.Do(req)
	if err != nil {
		return p, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, policy.MaxLookBytes+1))
	switch {
	case err != nil:
		return p, nil, err
	case resp.StatusCode == http.StatusOK && len(data) > policy.MaxLookBytes:
		return p, nil, &lookRefusal{http.StatusRequestEntityTooLarge, p + " is larger than 8 MiB"}
	case resp.StatusCode == http.StatusOK:
		return p, data, nil
	case resp.StatusCode == http.StatusNotFound && strings.TrimSpace(string(data)) == "404 page not found":
		return p, nil, &lookRefusal{http.StatusConflict, fmt.Sprintf("task %s's session runs a worker without the workspace read: its next session has it", t.ID)}
	case resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusUnauthorized:
		return p, nil, &lookRefusal{resp.StatusCode, strings.TrimSpace(string(data))}
	}
	return p, nil, fmt.Errorf("worker answered %d: %s", resp.StatusCode, textutil.Truncate(string(bytes.TrimSpace(data)), 300))
}
