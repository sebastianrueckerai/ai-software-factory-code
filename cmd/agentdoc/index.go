package main

// The map's index: the inventory table, the loop's shared trunk, the
// duplication matrix, the self-check — everything computed from the
// captures, nothing judged. The mermaid block is the generated skeleton
// of every agent as a node until the authored diagram is picked and
// embedded in its place.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"

	"encoding/json"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/agentdoc"
)

// indexRow is one agent's facts the index reads: the registry line, the
// capture's own numbers, and the spans the duplication matrix hashes.
type indexRow struct {
	agent        Agent
	model        string // the captured request's model alias; "" when no request exists
	purpose      string
	task         string
	tools        int
	requestBytes int
	spans        []agentdoc.Span
}

// rowOf reads one capture's row facts from the wire body itself.
func rowOf(a Agent, rec agentdoc.Recorded, spans []agentdoc.Span) (indexRow, error) {
	b, err := agentdoc.ParseBody(rec.Raw)
	if err != nil {
		return indexRow{}, err
	}
	return indexRow{agent: a, model: b.Model, purpose: rec.Purpose, task: rec.Task,
		tools: len(b.Tools), requestBytes: len(rec.Raw), spans: spans}, nil
}

// configRow is the config page's row: no request exists, the fragment is
// its one span.
func configRow(a Agent, frag json.RawMessage) indexRow {
	return indexRow{agent: a, purpose: "config", spans: []agentdoc.Span{{
		Name: "the agent block", Symbol: "buildOpencodeConfig",
		Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser, Text: string(frag),
	}}}
}

// index renders index.md from every agent's row, at code sha and policy
// commit, naming the pinned harness. Failures are rot findings, like the
// pages'.
func index(rows []indexRow, sha, policyCommit, opencodeVersion string) (string, []string) {
	d := &doc{sha: sha, policy: policyCommit}
	var md strings.Builder
	md.WriteString("# The agent context map\n\n")
	md.WriteString("One page per agent: the first model request verbatim, every part pinned to the builder that wrote it, the offered tools under their server's provenance. This index is computed from the same captures as the pages — sizes, hashes and counts, no verdicts.\n\n")
	d.writeSelfCheck(&md, rows, opencodeVersion)
	d.writeInventory(&md, rows)
	d.writeOneLiners(&md, rows)
	d.writeTrunk(&md, rows)
	d.writeMatrix(&md, rows)
	d.writeMermaid(&md)
	return md.String(), d.failures
}

func (d *doc) writeSelfCheck(md *strings.Builder, rows []indexRow, opencodeVersion string) {
	md.WriteString("## The self-check\n\n")
	fmt.Fprintf(md, "- code SHA `%s`; every permalink points there.\n", d.sha)
	fmt.Fprintf(md, "- policy `%s`, the tree the captures read.\n", d.policy)
	fmt.Fprintf(md, "- harness `%s`, the pinned opencode the loop captures drove.\n", opencodeVersion)
	fmt.Fprintf(md, "- unlabelled spans: 0 — the tool exits non-zero on any unlabelled byte, so an index that shipped means none.\n")
	fmt.Fprintf(md, "- %d pages. Regenerate: `go run ./cmd/agentdoc -out docs/agents -policy <factory tree> -opencode <pinned binary>`; no LLM runs.\n\n", len(rows))
	md.WriteString("Fixture ids: ")
	byTask := map[string][]string{}
	var order []string
	for _, r := range rows {
		if r.task == "" {
			continue
		}
		if _, seen := byTask[r.task]; !seen {
			order = append(order, r.task)
		}
		byTask[r.task] = append(byTask[r.task], r.agent.ID)
	}
	var parts []string
	for _, task := range order {
		parts = append(parts, fmt.Sprintf("`%s` (%s)", task, strings.Join(byTask[task], ", ")))
	}
	if len(parts) == 0 {
		md.WriteString("none crossed a wire.\n\n")
		return
	}
	fmt.Fprintf(md, "%s. Every capture is a fixture task, not live data.\n\n", strings.Join(parts, "; "))
}

func (d *doc) writeInventory(md *strings.Builder, rows []indexRow) {
	md.WriteString("## The inventory\n\n")
	md.WriteString("| agent | family | job | model | purpose | runs | tools | request bytes |\n|---|---|---|---|---|---|---|---|\n")
	for _, r := range rows {
		model := r.model
		if model == "" {
			model = "—"
		}
		requestBytes := fmt.Sprintf("%d", r.requestBytes)
		if r.requestBytes == 0 {
			requestBytes = "—"
		}
		fmt.Fprintf(md, "| [%s](%s.md) | %s | %s | `%s` | `%s` | %s | %d | %s |\n",
			r.agent.ID, r.agent.ID, r.agent.Family, r.agent.Job, model, r.purpose, r.agent.Runs, r.tools, requestBytes)
	}
	md.WriteString("\n")
}

// writeTrunk names the parts every loop page's first request carries byte
// for byte — the intersection of the loop rows' blocks by content hash,
// sized from the first loop page — so a slot every page fills differently,
// like the mode's guide, is not trunk.
func (d *doc) writeTrunk(md *strings.Builder, rows []indexRow) {
	var loop []indexRow
	for _, r := range rows {
		if r.agent.Loop {
			loop = append(loop, r)
		}
	}
	if len(loop) < 2 {
		return
	}
	hashOf := func(sp agentdoc.Span) string {
		sum := sha256.Sum256([]byte(sp.Text))
		return hex.EncodeToString(sum[:])
	}
	shared := map[string]agentdoc.Span{}
	for _, sp := range loop[0].spans {
		if sp.Text != "" {
			shared[hashOf(sp)] = sp
		}
	}
	for _, r := range loop[1:] {
		have := map[string]bool{}
		for _, sp := range r.spans {
			if sp.Text != "" {
				have[hashOf(sp)] = true
			}
		}
		for h := range shared {
			if !have[h] {
				delete(shared, h)
			}
		}
	}
	var spans []agentdoc.Span
	for _, sp := range shared {
		spans = append(spans, sp)
	}
	slices.SortFunc(spans, func(a, b agentdoc.Span) int { return strings.Compare(a.Name, b.Name) })
	md.WriteString("## The one loop\n\n")
	fmt.Fprintf(md, "The %d loop pages are one session shape on the same trunk: the harness system prompt around the repository's AGENTS.md, the brief builders, the offered tools. What every page carries byte for byte, sized from the first loop page's capture:\n\n", len(loop))
	for _, sp := range spans {
		fmt.Fprintf(md, "- %s — %d bytes\n", sp.Name, len(sp.Text))
	}
	md.WriteString("\nWhat the pages do not share, each carries as its own: the mode's guide, the class's sandbox facts and tool list, the harness preamble's model line and sandbox path.\n\n")
}

// writeMatrix groups the pages' spans by content hash; a block two or
// more agents carry names each carrier and the part name it travels under.
func (d *doc) writeMatrix(md *strings.Builder, rows []indexRow) {
	md.WriteString("## The duplication matrix\n\n")
	type carrier struct{ agent, name, symbol string }
	blocks := map[string][]carrier{}
	sizes := map[string]int{}
	var hashes []string
	for _, r := range rows {
		for _, sp := range r.spans {
			if sp.Text == "" {
				continue
			}
			sum := sha256.Sum256([]byte(sp.Text))
			h := hex.EncodeToString(sum[:])[:12]
			if _, seen := blocks[h]; !seen {
				hashes = append(hashes, h)
			}
			blocks[h] = append(blocks[h], carrier{r.agent.ID, sp.Name, sp.Symbol})
			sizes[h] = len(sp.Text)
		}
	}
	slices.Sort(hashes)
	var dupes int
	for _, h := range hashes {
		agents := map[string]bool{}
		for _, c := range blocks[h] {
			agents[c.agent] = true
		}
		if len(agents) < 2 {
			continue
		}
		if dupes == 0 {
			md.WriteString("Blocks several agents carry byte for byte, by content hash:\n\n| block | bytes | carried by |\n|---|---|---|\n")
		}
		var who []string
		symbols := map[string]bool{}
		for _, c := range blocks[h] {
			who = append(who, fmt.Sprintf("[%s](%s.md) — %s", c.agent, c.agent, c.name))
			if c.symbol != "" {
				symbols[c.symbol] = true
			}
		}
		block := "`" + h + "`"
		if len(symbols) == 1 {
			for _, s := range slices.Sorted(maps.Keys(symbols)) {
				block += " · `" + s + "`"
			}
		}
		fmt.Fprintf(md, "| %s | %d | %s |\n", block, sizes[h], strings.Join(who, "; "))
		dupes++
	}
	if dupes == 0 {
		md.WriteString("No block crosses an agent boundary byte for byte.\n")
	}
	md.WriteString("\n")
}

// writeOneLiners carries the authored one-liner of every agent that has
// one; the shape test holds the registry to full coverage.
func (d *doc) writeOneLiners(md *strings.Builder, rows []indexRow) {
	md.WriteString("## The agents in one line each\n\n")
	for _, r := range rows {
		if b, ok := blurbOf(r.agent.ID); ok {
			fmt.Fprintf(md, "- [%s](%s.md): %s\n", r.agent.ID, r.agent.ID, b)
		}
	}
	md.WriteString("\n")
}

// writeMermaid embeds the authored master diagram, picked from the
// drafts; a test holds it to a node for every registry agent.
func (d *doc) writeMermaid(md *strings.Builder) {
	md.WriteString("## The factory at a glance\n\n```mermaid\n")
	md.WriteString(masterDiagram())
	md.WriteString("\n```\n")
}
