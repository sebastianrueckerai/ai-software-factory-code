package gateway

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The latency window reads an alias's successful calls of the last hour that
// a deployment answered; the switch rows keep the newest state per alias and
// store a decision once.
func TestPGAliasSwitches(t *testing.T) {
	dsn := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE audit, alias_switches`); err != nil {
		t.Fatal(err)
	}
	insert := func(age time.Duration, alias, deployment, decision, status, tool string) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO audit (ts, task_id, decision, reason, model_alias, deployment, output_tokens, duration_ms, status, tool)
			 VALUES (now() - $1::interval, 't', $2, '', $3, $4, 1000, 39000, $5, $6)`,
			age.String(), decision, alias, deployment, status, tool); err != nil {
			t.Fatal(err)
		}
	}
	insert(10*time.Minute, "big", "zai/glm-5.3", "allowed", "200", "")
	insert(2*time.Hour, "big", "zai/glm-5.3", "allowed", "200", "")
	insert(10*time.Minute, "big", "zai/glm-5.3", "allowed", "503", "")
	insert(10*time.Minute, "big", "zai/glm-5.3", "refused_day_cap", "", "")
	insert(10*time.Minute, "big", "", "allowed", "200", "")
	insert(10*time.Minute, "big", "zai/glm-5.3", "allowed", "200", "web_search")
	insert(10*time.Minute, "small", "zai/glm-5.3-flash", "allowed", "200", "")
	insert(10*time.Minute, "big-alibaba", "dashscope/qwen3.8-max", "allowed", "200", "")
	got, err := s.LatencySamples(ctx, []string{"big", "big-alibaba"}, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("samples = %+v, want the hour's successful big call and the twin's", got)
	}

	rec := func(kind string, median float64) {
		t.Helper()
		if err := s.RecordAliasSwitch(ctx, policy.AliasSwitch{Alias: "big", Twin: "big-alibaba", Kind: kind, Rate: median, Deployment: "zai/glm-5.3"}); err != nil {
			t.Fatal(err)
		}
	}
	rec(policy.SwitchReturn, 20) // a return with nothing open says nothing
	rec(policy.SwitchOn, 39)
	rec(policy.SwitchOn, 41) // a second replica's decision
	open, err := s.OpenAliasSwitches(ctx)
	if err != nil || len(open) != 1 || open[0].Rate != 39 || open[0].Twin != "big-alibaba" || open[0].At.IsZero() {
		t.Fatalf("open = %+v, %v, want the first switch at 39", open, err)
	}
	rec(policy.SwitchReturn, 30)
	if open, err := s.OpenAliasSwitches(ctx); err != nil || len(open) != 0 {
		t.Fatalf("open after the return = %+v, %v, want none", open, err)
	}
	var rows int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM alias_switches`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("rows = %d, %v, want the switch and the return", rows, err)
	}
}
