package i18n

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]Lang{"": En, "en": En, " JA ": Ja, "ja": Ja} {
		if got, err := Parse(in); err != nil || got != want {
			t.Errorf("Parse(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := Parse("fr"); err == nil {
		t.Error("Parse(fr) must fail")
	}
}

func TestT(t *testing.T) {
	t.Parallel()
	if got := En.T("🟢 %s → start", "web"); got != "🟢 web → start" {
		t.Errorf("En = %q", got)
	}
	if got := Ja.T("🟢 %s → start", "web"); got != "🟢 web → 起動" {
		t.Errorf("Ja = %q", got)
	}
	if got := Ja.T("no translation for %s", "x"); got != "no translation for x" {
		t.Errorf("fallback = %q", got)
	}
	if got := Lang("").T("Close"); got != "Close" {
		t.Errorf("empty lang = %q", got)
	}
}

var verbRe = regexp.MustCompile(`%(\[\d+\])?[a-z]`)

// verbs returns the verbs of a format as "index:verb", resolving implicit indexes.
func verbs(format string) []string {
	var out []string
	next := 1
	for _, m := range verbRe.FindAllStringSubmatch(format, -1) {
		idx := next
		if m[1] != "" {
			idx, _ = strconv.Atoi(strings.Trim(m[1], "[]"))
		}
		out = append(out, strconv.Itoa(idx)+":"+m[0][len(m[0])-1:])
		next = idx + 1
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func TestJapanese_KeepsTheVerbsOfTheKey(t *testing.T) {
	t.Parallel()
	for en, ja := range ja {
		if !slices.Equal(verbs(en), verbs(ja)) {
			t.Errorf("%q → %q: verbs %v, want %v", en, ja, verbs(ja), verbs(en))
		}
	}
}

// TestJapanese_CoversEveryMessage parses the translated packages and checks that every literal
// passed to t / T / printf has a Japanese entry.
func TestJapanese_CoversEveryMessage(t *testing.T) {
	t.Parallel()
	roots := []string{"..", filepath.Join("..", "..", "workload", "rollout")}
	for _, root := range roots {
		err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(token.NewFileSet(), p, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok || len(c.Args) == 0 {
					return true
				}
				var name string
				switch fn := c.Fun.(type) {
				case *ast.SelectorExpr:
					name = fn.Sel.Name
				case *ast.Ident:
					name = fn.Name
				}
				lit, ok := c.Args[0].(*ast.BasicLit)
				if (name != "t" && name != "T" && name != "printf") || !ok || lit.Kind != token.STRING {
					return true
				}
				key, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Errorf("%s: %v", p, err)
					return true
				}
				if _, ok := ja[key]; !ok {
					t.Errorf("%s: no Japanese for %q", p, key)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
