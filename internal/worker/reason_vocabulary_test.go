package worker

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// The point of a closed reason vocabulary is that --redrive --reason X reaches
// every message of a problem class. That only holds if one class has exactly one
// code, which in turn only holds if nobody can invent a code in passing.
//
// Declaring domain.FailureReason does not achieve that on its own. Go's untyped
// string constants are assignable to any named string type, so
// Fatal("OCR_FALIED", err) compiles cleanly and quietly creates a class of one
// that no filter will ever match — verified, not assumed. The named type buys
// documentation and one place to enumerate; this test buys the guarantee.
func TestFailureReasonsAreNeverStringLiterals(t *testing.T) {
	root := filepath.Join("..", "..", "internal")
	fset := token.NewFileSet()

	var offenders []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := calleeName(call.Fun)
			if name != "Fatal" && name != "Decline" {
				return true
			}
			for _, arg := range call.Args {
				lit, ok := arg.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				// Decline's first argument is a doc id, not a reason; reasons
				// are the SCREAMING_SNAKE_CASE ones.
				if v := strings.Trim(lit.Value, `"`); isReasonShaped(v) {
					offenders = append(offenders,
						fset.Position(lit.Pos()).String()+": "+lit.Value)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(offenders) > 0 {
		t.Errorf("reason codes passed as string literals — declare them in domain instead:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

func calleeName(f ast.Expr) string {
	switch fn := f.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

// isReasonShaped matches SCREAMING_SNAKE_CASE, which is what every reason code
// looks like and what no doc id or message ever does.
func isReasonShaped(s string) bool {
	if len(s) < 3 {
		return false
	}
	for _, r := range s {
		if r != '_' && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}
