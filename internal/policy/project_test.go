package policy

import (
	"reflect"
	"strings"
	"testing"
)

func TestProjectDefaultAbsentTakesBuiltIns(t *testing.T) {
	d, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	want := ProjectDefault{MergeRule: "human", Quota: Quota{CPU: "2", Memory: "4Gi", Storage: "10Gi"},
		Standing: Standing{ServerTypes: []string{}}}
	if got := d.Project(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Project() = %+v, want %+v", got, want)
	}
	env := d.DefaultEnvelope()
	if env.MonthlyEUR != 10 || !reflect.DeepEqual(env.Standing, want.Standing) {
		t.Fatalf("DefaultEnvelope() = %+v", env)
	}
}

const withProject = valid + `project_default:
  merge_rule: auto
  quota: {cpu: 1500m, memory: 3Gi, storage: 20Gi}
  standing: {monthly_eur: 12.5, server_types: [cx23], max_count: 1}
`

func TestProjectDefaultParsed(t *testing.T) {
	d, err := Parse([]byte(withProject))
	if err != nil {
		t.Fatal(err)
	}
	want := ProjectDefault{MergeRule: "auto", Quota: Quota{CPU: "1500m", Memory: "3Gi", Storage: "20Gi"},
		Standing: Standing{MonthlyEUR: 12.5, ServerTypes: []string{"cx23"}, MaxCount: 1}}
	if got := d.Project(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Project() = %+v, want %+v", got, want)
	}
	if got := d.DefaultEnvelope(); !reflect.DeepEqual(got.Standing, want.Standing) || got.MaxCount != 2 {
		t.Fatalf("DefaultEnvelope() = %+v", got)
	}
}

func TestProjectDefaultRejects(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, want string }{
		"unknown key":       {valid + "project_default: {merge_rule: human, quota: {cpu: '1', memory: 1Gi, storage: 1Gi}, standing: {monthly_eur: 0, server_types: [], max_count: 0}, colour: red}\n", "colour"},
		"merge rule":        {strings.Replace(withProject, "merge_rule: auto", "merge_rule: sometimes", 1), "merge_rule"},
		"quota missing":     {strings.Replace(withProject, "memory: 3Gi, ", "", 1), "quota.memory"},
		"quota not a size":  {strings.Replace(withProject, "storage: 20Gi", "storage: lots", 1), "quota.storage"},
		"standing negative": {strings.Replace(withProject, "monthly_eur: 12.5", "monthly_eur: -1", 1), "standing.monthly_eur"},
		"standing count":    {strings.Replace(withProject, "max_count: 1}", "max_count: -1}", 1), "standing.max_count"},
		"standing type":     {strings.Replace(withProject, "server_types: [cx23]", "server_types: ['']", 1), "standing.server_types"},
		"standing in task":  {strings.Replace(valid, "volume_max_gb: 20", "volume_max_gb: 20\n  standing: {monthly_eur: 1}", 1), "standing"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

func TestQuantity(t *testing.T) {
	for q, ok := range map[string]bool{"2": true, "1500m": true, "0.5": true, "4Gi": true, "10G": true, "": false, "4 Gi": false, "-1": false, "1e3": false, "Gi": false} {
		if got := Quantity(q); got != ok {
			t.Errorf("Quantity(%q) = %v, want %v", q, got, ok)
		}
	}
}
