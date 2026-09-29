package redact

import (
	"slices"
	"testing"
)

const plantedDiff = "diff --git a/internal/x/x_test.go b/internal/x/x_test.go\n" +
	"index 1111111..2222222 100644\n" +
	"--- a/internal/x/x_test.go\n" +
	"+++ b/internal/x/x_test.go\n" +
	"@@ -10,4 +10,7 @@ func TestX(t *testing.T) {\n" +
	" \ta := 1\n" +
	"-\tpassword = \"oldpassword1\"\n" +
	"+\tb := 3\n" +
	"+\tpassword = \"hunter2hunter2\"\n" +
	" \tc := 4\n" +
	"+const key = `-----BEGIN RSA PRIVATE KEY-----\n" +
	"+MIIEowIBAAKCAQEA\n" +
	"+-----END RSA PRIVATE KEY-----`\n" +
	" \td := `sk-context0000000000000000000000`\n" +
	"diff --git a/cfg/app.env b/cfg/app.env\n" +
	"new file mode 100644\n" +
	"--- /dev/null\n" +
	"+++ b/cfg/app.env\n" +
	"@@ -0,0 +1,6 @@\n" +
	"+DATABASE_URL=postgres://app:s3cretpass@db:5432/app\n" +
	"+AWS_KEY=AKIAIOSFODNN7EXAMPLE\n" +
	"+GH=ghp_aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789\n" +
	"+GL=glpat-aBcDeFgHiJkLmNoPqRsT\n" +
	"+OPENAI=sk-proj-aBcDeFgHiJkLmNoPqRsTuV\n" +
	"+curl -H 'Authorization: Bearer abcdef0123456789xyz' host\n" +
	"diff --git a/old.txt b/old.txt\n" +
	"deleted file mode 100644\n" +
	"--- a/old.txt\n" +
	"+++ /dev/null\n" +
	"@@ -1 +0,0 @@\n" +
	"-token = \"ghp_aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789\"\n"

// Each added line with a secret's shape is a hit at its file and its line
// in the new file; removed and context lines are not.
func TestScanDiffFindsPlantedSecrets(t *testing.T) {
	want := []Hit{
		{File: "internal/x/x_test.go", Line: 12, Kind: KindAssignment},
		{File: "internal/x/x_test.go", Line: 14, Kind: KindPrivateKey},
		{File: "cfg/app.env", Line: 1, Kind: KindConnection},
		{File: "cfg/app.env", Line: 2, Kind: KindAWS},
		{File: "cfg/app.env", Line: 3, Kind: KindGitHub},
		{File: "cfg/app.env", Line: 4, Kind: KindGitLab},
		{File: "cfg/app.env", Line: 5, Kind: KindAPIKey},
		{File: "cfg/app.env", Line: 6, Kind: KindCredential},
	}
	if got := ScanDiff(plantedDiff); !slices.Equal(got, want) {
		t.Fatalf("ScanDiff =\n%+v\nwant\n%+v", got, want)
	}
}

func TestScanDiffCleanDiff(t *testing.T) {
	clean := "diff --git a/internal/f/f.go b/internal/f/f.go\n" +
		"--- a/internal/f/f.go\n" +
		"+++ b/internal/f/f.go\n" +
		"@@ -1,2 +1,6 @@\n" +
		" package f\n" +
		"+\treq.Header.Set(\"Authorization\", \"Bearer \"+m.Token)\n" +
		"+\t// missing bearer token\n" +
		"+\tremote := \"ssh://git@forgejo.example/sf/code.git\"\n" +
		"+\thead := \"3f786850e387550fdab836ed7e6dc881de23001b\"\n" +
		"+\tpassword := os.Getenv(\"PASSWORD\")\n"
	if got := ScanDiff(clean); len(got) != 0 {
		t.Fatalf("ScanDiff = %+v", got)
	}
}
