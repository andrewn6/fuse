package fusefile

import (
	"os"
	"strings"
	"testing"
)

// docsFirstFusefilePath is the docs page whose first yaml block is the
// canonical minimal Fusefile. this test is the drift guard for it: the page
// is a fourth copy of a Fusefile in this repo, and nothing else would catch
// it going stale short of someone running `fuse up` by hand.
//
// the relative path couples this test to the docs tree layout. if the page
// moves, move this constant with it.
const docsFirstFusefilePath = "../../docs/content/docs/learn/first-fusefile.mdx"

// firstYAMLBlock returns the contents of the first ```yaml fenced block in md.
func firstYAMLBlock(t *testing.T, md string) string {
	t.Helper()

	const open = "```yaml\n"
	i := strings.Index(md, open)
	if i < 0 {
		t.Fatalf("no ```yaml block in %s", docsFirstFusefilePath)
	}
	rest := md[i+len(open):]
	j := strings.Index(rest, "\n```")
	if j < 0 {
		t.Fatalf("unterminated ```yaml block in %s", docsFirstFusefilePath)
	}
	return rest[:j+1]
}

func TestDocsFirstFusefileCompiles(t *testing.T) {
	data, err := os.ReadFile(docsFirstFusefilePath)
	if err != nil {
		t.Fatalf("read docs page: %v", err)
	}

	src := firstYAMLBlock(t, string(data))

	f, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("fusefile.Parse rejected the docs example: %v\n%s", err, src)
	}

	c, err := Compile(f)
	if err != nil {
		t.Fatalf("fusefile.Compile rejected the docs example: %v", err)
	}

	// the page's line-by-line table asserts each of these; if the example
	// changes, the prose has to change with it.
	want := ResourceSpec{CPUs: 2, RamMB: 2048}
	if c.Spec != want {
		t.Errorf("spec = %+v, want %+v", c.Spec, want)
	}

	// the page claims no --secret is needed and nothing is exposed.
	if len(c.RequiredSecrets) != 0 {
		t.Errorf("required secrets = %v, want none", c.RequiredSecrets)
	}
	if len(c.Expose) != 0 {
		t.Errorf("expose = %v, want none", c.Expose)
	}

	// the page prints this script verbatim under "Line by line".
	wantScript := "set -eu\n" +
		"if (set -o pipefail) 2>/dev/null; then set -o pipefail; fi\n" +
		"mkdir -p '/workspace'\n" +
		"cd '/workspace'\n" +
		"echo \"hello from $(hostname)\" > hello.txt\n"
	if c.StartupScript != wantScript {
		t.Errorf("startup script =\n%q\nwant\n%q", c.StartupScript, wantScript)
	}
	if !strings.Contains(string(data), wantScript) {
		t.Errorf("the page's printed startup script no longer matches the compiled one")
	}
}
