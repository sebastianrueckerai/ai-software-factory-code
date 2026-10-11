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
	"@@ -0,0 +1,10 @@\n" +
	"+DATABASE_URL=postgres://app:s3cretpass@db:5432/app\n" +
	"+AWS_KEY=AKIAIOSFODNN7EXAMPLE\n" +
	"+GH=ghp_aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789\n" +
	"+GL=glpat-aBcDeFgHiJkLmNoPqRsT\n" +
	"+OPENAI=sk-proj-aBcDeFgHiJkLmNoPqRsTuV\n" +
	"+curl -H 'Authorization: Bearer abcdef0123456789xyz' host\n" +
	"+DB_PASSWORD=envpass0001\n" +
	"+  password: yamlpass0001\n" +
	"+api-key: \"apikeyvalue01\"\n" +
	"+client_secret: \"clientsecret01\"\n" +
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
		{File: "cfg/app.env", Line: 7, Kind: KindAssignment},
		{File: "cfg/app.env", Line: 8, Kind: KindAssignment},
		{File: "cfg/app.env", Line: 9, Kind: KindAssignment},
		{File: "cfg/app.env", Line: 10, Kind: KindAssignment},
	}
	if got := ScanDiff(plantedDiff); !slices.Equal(got, want) {
		t.Fatalf("ScanDiff =\n%+v\nwant\n%+v", got, want)
	}
}

// git ends the +++ line of a path with a space with a tab: the hit's file
// has none.
func TestScanDiffPathWithASpace(t *testing.T) {
	diff := "diff --git a/my cfg.env b/my cfg.env\n--- a/my cfg.env\t\n+++ b/my cfg.env\t\n@@ -1 +1,2 @@\n x\n+AWS_KEY=AKIAIOSFODNN7EXAMPLE\n"
	if got, want := ScanDiff(diff), []Hit{{File: "my cfg.env", Line: 2, Kind: KindAWS}}; !slices.Equal(got, want) {
		t.Fatalf("ScanDiff = %+v, want %+v", got, want)
	}
}

// A GitHub token is a hit in each shape GitHub issues, a fine-grained one
// and one glued to a word too; a classic prefix with 35 characters is
// none.
func TestScanDiffGitHubTokens(t *testing.T) {
	diff := "diff --git a/t.env b/t.env\n--- a/t.env\n+++ b/t.env\n@@ -0,0 +1,3 @@\n" +
		"+FG=github_pat_11ABCDEFG0123456789abc_aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789aBcDeFgHiJkLmNoPqRsTuVw\n" +
		"+my_ghp_aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789\n" +
		"+ghp_aBcDeFgHiJkLmNoPqRsTuVwXyZ012345678\n"
	want := []Hit{{File: "t.env", Line: 1, Kind: KindGitHub}, {File: "t.env", Line: 2, Kind: KindGitHub}}
	if got := ScanDiff(diff); !slices.Equal(got, want) {
		t.Fatalf("ScanDiff = %+v, want %+v", got, want)
	}
}

func TestScanDiffCleanDiff(t *testing.T) {
	clean := "diff --git a/internal/f/f.go b/internal/f/f.go\n" +
		"--- a/internal/f/f.go\n" +
		"+++ b/internal/f/f.go\n" +
		"@@ -1,2 +1,21 @@\n" +
		" package f\n" +
		"+\treq.Header.Set(\"Authorization\", \"Bearer \"+m.Token)\n" +
		"+\t// missing bearer token\n" +
		"+\tremote := \"ssh://git@forgejo.example/sf/code.git\"\n" +
		"+\thead := \"3f786850e387550fdab836ed7e6dc881de23001b\"\n" +
		"+\tpassword := os.Getenv(\"PASSWORD\")\n" +
		"+\t\tPassword: cfg.Password,\n" +
		"+  password: ${DB_PASSWORD}\n" +
		"+PASSWORD=<changeme>\n" +
		"+  secretName: forgejo-merge-token\n" +
		"+\t\tPassword: cfg.Password\n" +
		"+password_file: /run/secrets/db\n" +
		"+ENV POSTGRES_PASSWORD_FILE=/run/secrets/pg\n" +
		"+existingPasswordSecret: my-db-secret\n" +
		"+password = \"<your-password>\"\n" +
		"+secretName: \"forgejo-merge-token\"\n" +
		"+tokenURL: \"https://example.com/token\"\n" +
		"+token_url = \"https://example.com/token\"\n" +
		"+token: \"${GITHUB_TOKEN_VALUE}\"\n" +
		"+api_key: \"{{.Values.apiKey}}\"\n" +
		"+passwordSecretRef: my-db-secret\n"
	if got := ScanDiff(clean); len(got) != 0 {
		t.Fatalf("ScanDiff = %+v", got)
	}
}

// DiffSecrets is every secret value ScanDiff sees on an added line, for a
// scrubber: the value alone, not its key or its quotes; a PEM block's
// body lines too.
func TestDiffSecrets(t *testing.T) {
	want := []string{"hunter2hunter2", "MIIEowIBAAKCAQEA", "s3cretpass", "AKIAIOSFODNN7EXAMPLE",
		"ghp_aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789", "glpat-aBcDeFgHiJkLmNoPqRsT", "sk-proj-aBcDeFgHiJkLmNoPqRsTuV", "abcdef0123456789xyz",
		"envpass0001", "yamlpass0001", "apikeyvalue01", "clientsecret01"}
	if got := DiffSecrets(plantedDiff); !slices.Equal(got, want) {
		t.Fatalf("DiffSecrets =\n%q\nwant\n%q", got, want)
	}
	if got := New(DiffSecrets(plantedDiff)...).String("it quotes hunter2hunter2 and envpass0001"); got != "it quotes [redacted] and [redacted]" {
		t.Fatalf("scrubbed = %q", got)
	}
}
