package pg

import (
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The rungRig methods the act family's moved files reach and factoryd's
// rung files still hold; tripCap, projectTree and the file fixtures they
// need moved before, with the trip rig into helpers_2d_test.go.

// settle waits for the rung 1 conversations the rig launched, through the
// runner's rungs surface.
func (g rungRig) settle() { g.r.PgWaitRungs() }

func (g rungRig) steps(t *testing.T, id int64) []CaseStep { return caseStepsOf(t, g.s, id) }

// atAutonomy puts the rig's project at state: full auto is autonomous with
// merge rule auto.
func (g rungRig) atAutonomy(state string) {
	v := g.r.PgCfg().Policy()
	p := v.Projects["demo-app"]
	p.Autonomy, p.MergeRule = state, policy.MergeHuman
	if state == policy.AutonomyFullAuto {
		p.Autonomy, p.MergeRule = policy.AutonomyAutonomous, policy.MergeAuto
	}
	v.Projects = map[string]policy.ProjectConfig{"demo-app": p}
	g.r.PgSetPolicy(func() policy.Version { return v })
}
