// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package test

import (
	"bufio"
	"fmt"
	"internal/testenv"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// testPGOIntendedInlining tests that specific functions are inlined.
func testPGOIntendedInlining(t *testing.T, dir string) {
	testenv.MustHaveGoRun(t)
	t.Parallel()

	const pkg = "example.com/pgo/inline"

	// Add a go.mod so we have a consistent symbol names in this temp dir.
	goMod := fmt.Sprintf(`module %s
go 1.19
`, pkg)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatalf("error writing go.mod: %v", err)
	}

	want := []string{
		"(*BS).NS",
	}

	// The functions which are not expected to be inlined are as follows.
	wantNot := []string{
		// The calling edge main->A is hot and the cost of A is large
		// than inlineHotCalleeMaxBudget.
		"A",
		// The calling edge BenchmarkA" -> benchmarkB is cold and the
		// cost of A is large than inlineMaxBudget.
		"benchmarkB",
	}

	must := map[string]bool{
		"(*BS).NS": true,
	}

	notInlinedReason := make(map[string]string)
	for _, fname := range want {
		fullName := pkg + "." + fname
		if _, ok := notInlinedReason[fullName]; ok {
			t.Errorf("duplicate func: %s", fullName)
		}
		notInlinedReason[fullName] = "unknown reason"
	}

	// If the compiler emit "cannot inline for function A", the entry A
	// in expectedNotInlinedList will be removed.
	expectedNotInlinedList := make(map[string]struct{})
	for _, fname := range wantNot {
		fullName := pkg + "." + fname
		expectedNotInlinedList[fullName] = struct{}{}
	}

	// Build the test with the profile. Use a smaller threshold to test.
	// TODO: maybe adjust the test to work with default threshold.
	pprof := filepath.Join(dir, "inline_hot.pprof")
	gcflag := fmt.Sprintf("-gcflags=-m -m -pgoprofile=%s -d=pgoinlinebudget=160,pgoinlinecdfthreshold=90", pprof)
	out := filepath.Join(dir, "test.exe")
	cmd := testenv.CleanCmdEnv(testenv.Command(t, testenv.GoToolPath(t), "test", "-c", "-o", out, gcflag, "."))
	cmd.Dir = dir

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("error creating pipe: %v", err)
	}
	defer pr.Close()
	cmd.Stdout = pw
	cmd.Stderr = pw

	err = cmd.Start()
	pw.Close()
	if err != nil {
		t.Fatalf("error starting go test: %v", err)
	}

	scanner := bufio.NewScanner(pr)
	curPkg := ""
	canInline := regexp.MustCompile(`: can inline ([^ ]*)`)
	haveInlined := regexp.MustCompile(`: inlining call to ([^ ]*)`)
	cannotInline := regexp.MustCompile(`: cannot inline ([^ ]*): (.*)`)
	for scanner.Scan() {
		line := scanner.Text()
		t.Logf("child: %s", line)
		if strings.HasPrefix(line, "# ") {
			curPkg = line[2:]
			splits := strings.Split(curPkg, " ")
			curPkg = splits[0]
			continue
		}
		if m := haveInlined.FindStringSubmatch(line); m != nil {
			fname := m[1]
			delete(notInlinedReason, curPkg+"."+fname)
			continue
		}
		if m := canInline.FindStringSubmatch(line); m != nil {
			fname := m[1]
			fullname := curPkg + "." + fname
			// If function must be inlined somewhere, being inlinable is not enough
			if _, ok := must[fullname]; !ok {
				delete(notInlinedReason, fullname)
				continue
			}
		}
		if m := cannotInline.FindStringSubmatch(line); m != nil {
			fname, reason := m[1], m[2]
			fullName := curPkg + "." + fname
			if _, ok := notInlinedReason[fullName]; ok {
				// cmd/compile gave us a reason why
				notInlinedReason[fullName] = reason
			}
			delete(expectedNotInlinedList, fullName)
			continue
		}
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("error running go test: %v", err)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("error reading go test output: %v", err)
	}
	for fullName, reason := range notInlinedReason {
		t.Errorf("%s was not inlined: %s", fullName, reason)
	}

	// If the list expectedNotInlinedList is not empty, it indicates
	// the functions in the expectedNotInlinedList are marked with caninline.
	for fullName, _ := range expectedNotInlinedList {
		t.Errorf("%s was expected not inlined", fullName)
	}
}

// TestPGOIntendedInlining tests that specific functions are inlined when PGO
// is applied to the exact source that was profiled.
func TestPGOIntendedInlining(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("error getting wd: %v", err)
	}
	srcDir := filepath.Join(wd, "testdata/pgo/inline")

	// Copy the module to a scratch location so we can add a go.mod.
	dir := t.TempDir()

	for _, file := range []string{"inline_hot.go", "inline_hot_test.go", "inline_hot.pprof"} {
		if err := copyFile(filepath.Join(dir, file), filepath.Join(srcDir, file)); err != nil {
			t.Fatalf("error copying %s: %v", file, err)
		}
	}

	testPGOIntendedInlining(t, dir)
}

// TestPGOIntendedInlining tests that specific functions are inlined when PGO
// is applied to the modified source.
func TestPGOIntendedInliningShiftedLines(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("error getting wd: %v", err)
	}
	srcDir := filepath.Join(wd, "testdata/pgo/inline")

	// Copy the module to a scratch location so we can modify the source.
	dir := t.TempDir()

	// Copy most of the files unmodified.
	for _, file := range []string{"inline_hot_test.go", "inline_hot.pprof"} {
		if err := copyFile(filepath.Join(dir, file), filepath.Join(srcDir, file)); err != nil {
			t.Fatalf("error copying %s : %v", file, err)
		}
	}

	// Add some comments to the top of inline_hot.go. This adjusts the line
	// numbers of all of the functions without changing the semantics.
	src, err := os.Open(filepath.Join(srcDir, "inline_hot.go"))
	if err != nil {
		t.Fatalf("error opening src inline_hot.go: %v", err)
	}
	defer src.Close()

	dst, err := os.Create(filepath.Join(dir, "inline_hot.go"))
	if err != nil {
		t.Fatalf("error creating dst inline_hot.go: %v", err)
	}
	defer dst.Close()

	if _, err := io.WriteString(dst, `// Autogenerated
// Lines
`); err != nil {
		t.Fatalf("error writing comments to dst: %v", err)
	}

	if _, err := io.Copy(dst, src); err != nil {
		t.Fatalf("error copying inline_hot.go: %v", err)
	}

	dst.Close()

	testPGOIntendedInlining(t, dir)
}

func copyFile(dst, src string) error {
	s, err := os.Open(src)
	if err != nil {
		return err
	}
	defer s.Close()

	d, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer d.Close()

	_, err = io.Copy(d, s)
	return err
}
