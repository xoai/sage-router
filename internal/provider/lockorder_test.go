package provider

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenUnderLock are method names that perform I/O (store reads/writes,
// refresh HTTP calls) and therefore MUST NOT be invoked while holding c.mu.
// Violating this risks the deadlock the auto-review M6 fix was designed to
// prevent: a method on Connection that takes c.mu, then calls into a store
// method that re-enters Connection (e.g., during a future refactor that
// makes store call back via callbacks).
//
// Keep this list in sync with internal/auth/store.go and internal/store/store.go.
// Adding a new I/O method on either should also add an entry here.
var forbiddenUnderLock = map[string]bool{
	// internal/auth (AuthStore)
	"GetCredential":       true,
	"PutCredential":       true,
	"BumpRefreshFailure":  true,
	"ResetRefreshFailures": true,
	"GetRefreshFailures":  true,

	// internal/store (Store)
	"GetConnection":                  true,
	"UpdateConnection":               true,
	"DeleteConnection":               true,
	"CreateConnection":               true,
	"BumpConnectionRefreshFailures":  true,
	"GetConnectionRefreshFailures":   true,
}

// TestLockOrderDiscipline asserts that no method anywhere in internal/ calls
// a store/refresh method while holding c.mu (via Lock or RLock). Test-only.
//
// Living here in internal/provider rather than a standalone tools/lockorder
// binary keeps the project's go.mod dep count fixed (the spec's hard rule)
// and uses `go test` as the CI vehicle. The directory walk catches future
// packages added under internal/ — anything that takes a mutex on a field
// literally named "mu" gets scanned.
//
// Approach: parse each .go file (not _test.go) under ../, walk every
// function body, track when we're "inside" a c.mu.Lock() or c.mu.RLock()
// region, and flag any CallExpr inside that region whose selector name
// appears in forbiddenUnderLock.
//
// Limitations:
//   - Only matches by method name, not receiver type. A local helper method
//     named "GetConnection" would false-positive. None exist today; review
//     adds to forbiddenUnderLock when they do.
//   - Doesn't track Unlock matching with defer chains beyond "is there a
//     defer Unlock at the top of the locked region." Good enough for the
//     small surface we have.
//   - The "mu" field name is the convention across this codebase; methods
//     using a differently-named mutex are not scanned.
func TestLockOrderDiscipline(t *testing.T) {
	// Walk the entire internal/ tree starting from the parent of this
	// package, so future packages get the check for free.
	root := "../"
	var paths []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip vendored and testdata directories.
			name := d.Name()
			if name == "testdata" || name == "vendor" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	var violations []string
	fset := token.NewFileSet()
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		f, err := parser.ParseFile(fset, path, data, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			scanFunc(fset, path, fn, &violations)
			return true
		})
	}

	if len(violations) > 0 {
		t.Fatalf("lock-order discipline violated (caller must not hold c.mu when invoking these methods — release c.mu before the call and reacquire after):\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// scanFunc walks fn.Body. Once it sees a Lock/RLock on c.mu, every subsequent
// statement up to the matching Unlock (or end of function for deferred Unlocks)
// is "under lock." We flag forbidden method calls in that region.
func scanFunc(fset *token.FileSet, path string, fn *ast.FuncDecl, violations *[]string) {
	if fn.Body == nil {
		return
	}
	inLock := false
	deferredUnlock := false

	for _, stmt := range fn.Body.List {
		// Detect Lock/RLock acquisition.
		if !inLock {
			if isMutexCall(stmt, "Lock") || isMutexCall(stmt, "RLock") {
				inLock = true
				continue
			}
			continue
		}
		// We are under lock. Watch for:
		//   - deferred Unlock (region extends to end of function)
		//   - immediate Unlock (region ends here)
		//   - forbidden CallExpr
		if d, ok := stmt.(*ast.DeferStmt); ok {
			if callMatchesMutex(d.Call, "Unlock") || callMatchesMutex(d.Call, "RUnlock") {
				deferredUnlock = true
				continue
			}
		}
		if isMutexCall(stmt, "Unlock") || isMutexCall(stmt, "RUnlock") {
			inLock = false
			continue
		}
		// Inspect this statement for forbidden CallExprs.
		ast.Inspect(stmt, func(n ast.Node) bool {
			ce, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := ce.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if forbiddenUnderLock[sel.Sel.Name] {
				pos := fset.Position(ce.Pos())
				*violations = append(*violations,
					path+":"+itoa(pos.Line)+": "+fn.Name.Name+" calls "+sel.Sel.Name+" while holding c.mu",
				)
			}
			return true
		})
	}

	// If we left the loop still "under lock" via deferred Unlock, the region
	// extended to function end and we've already inspected all statements.
	_ = deferredUnlock
}

// isMutexCall reports whether stmt is an expression statement of the form
// `c.mu.<method>()` or `c.mu.<method>(...)`.
func isMutexCall(stmt ast.Stmt, method string) bool {
	es, ok := stmt.(*ast.ExprStmt)
	if !ok {
		return false
	}
	ce, ok := es.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	return callMatchesMutex(ce, method)
}

func callMatchesMutex(ce *ast.CallExpr, method string) bool {
	sel, ok := ce.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != method {
		return false
	}
	// sel.X must be `c.mu` — a SelectorExpr whose name is "mu".
	inner, ok := sel.X.(*ast.SelectorExpr)
	if !ok || inner.Sel.Name != "mu" {
		return false
	}
	// inner.X should be an ident (the receiver, typically "c").
	_, ok = inner.X.(*ast.Ident)
	return ok
}

// TestLockOrderDiscipline_DetectsInjectedViolation parses a synthetic file
// containing a deliberate violation and asserts the analyzer flags it. This
// guards against the analyzer silently regressing into a no-op (e.g., if
// someone refactors and breaks the AST matcher without noticing).
func TestLockOrderDiscipline_DetectsInjectedViolation(t *testing.T) {
	src := `package provider

type bad struct{}

func (c *bad) DoBadThing() {
	c.mu.Lock()
	defer c.mu.Unlock()
	// This should fire the analyzer:
	s.GetConnection("x")
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var violations []string
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			return true
		}
		scanFunc(fset, "synthetic.go", fn, &violations)
		return true
	})
	if len(violations) == 0 {
		t.Fatal("analyzer failed to detect injected violation — it has regressed into a no-op")
	}
	if !strings.Contains(violations[0], "GetConnection") || !strings.Contains(violations[0], "DoBadThing") {
		t.Errorf("violation message lacks expected context: %q", violations[0])
	}
}

// itoa converts a small positive int to its decimal string without strconv.
// Tiny helper to keep the analyzer dep-free.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
