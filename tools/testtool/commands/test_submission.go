package commands

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/perf/benchstat"

	"gitlab.com/slon/shad-go/tools/testtool"
)

const (
	problemFlag     = "problem"
	studentRepoFlag = "student-repo"
	privateRepoFlag = "private-repo"

	testdataDir      = "testdata"
	moduleImportPath = "gitlab.com/slon/shad-go"
)

var testSubmissionCmd = &cobra.Command{
	Use:   "check-task",
	Short: "test single task",
	Run: func(cmd *cobra.Command, args []string) {
		problem, err := cmd.Flags().GetString(problemFlag)
		if err != nil {
			log.Fatal(err)
		}

		studentRepo := mustParseDirFlag(studentRepoFlag, cmd)
		if !problemDirExists(studentRepo, problem) {
			log.Fatalf("%s does not have %s directory", studentRepo, problem)
		}

		privateRepo := mustParseDirFlag(privateRepoFlag, cmd)
		if !problemDirExists(privateRepo, problem) {
			log.Fatalf("%s does not have %s directory", privateRepo, problem)
		}

		if err := testSubmission(studentRepo, privateRepo, problem); err != nil {
			log.Fatal(err)
		}
	},
}

func init() {
	rootCmd.AddCommand(testSubmissionCmd)

	testSubmissionCmd.Flags().String(problemFlag, "", "problem directory name (required)")
	_ = testSubmissionCmd.MarkFlagRequired(problemFlag)

	testSubmissionCmd.Flags().String(studentRepoFlag, ".", "path to student repo root")
	testSubmissionCmd.Flags().String(privateRepoFlag, ".", "path to shad-go-private repo root")
}

// mustParseDirFlag parses string directory flag with given name.
//
// Exits on any error.
func mustParseDirFlag(name string, cmd *cobra.Command) string {
	dir, err := cmd.Flags().GetString(name)
	if err != nil {
		log.Fatal(err)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		log.Fatal(err)
	}
	return dir
}

// Check that repo dir contains problem subdir.
func problemDirExists(repo, problem string) bool {
	info, err := os.Stat(path.Join(repo, problem))
	if err != nil {
		return false
	}
	return info.IsDir()
}

func testSubmission(studentRepo, privateRepo, problem string) error {
	// Create temp directory to store all files required to test the solution.
	tmpRepo, err := os.MkdirTemp("/tmp", problem+"-")
	if err != nil {
		log.Fatal(err)
	}
	if err := os.Chmod(tmpRepo, 0755); err != nil {
		log.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmpRepo) }()
	log.Printf("testing submission in %s", tmpRepo)

	// Path to private problem folder.
	privateProblem := path.Join(privateRepo, problem)

	// Copy student repo files to temp dir.
	log.Printf("copying student repo")
	copyContents(studentRepo, ".", tmpRepo)

	// Copy tests from private repo to temp dir.
	log.Printf("copying tests")
	tests := listTestFiles(privateProblem)
	copyFiles(privateRepo, relPaths(privateRepo, tests), tmpRepo)

	// Copy !change files from private repo to temp dir.
	log.Printf("copying !change files")
	protected := listProtectedFiles(privateProblem)
	copyFiles(privateRepo, relPaths(privateRepo, protected), tmpRepo)

	// Copy testdata directory from private repo to temp dir.
	log.Printf("copying testdata directory")
	copyDir(privateRepo, path.Join(problem, testdataDir), tmpRepo)

	// Copy go.mod and go.sum from private repo to temp dir.
	log.Printf("copying go.mod, go.sum and .golangci.yml")
	copyFiles(privateRepo, []string{"go.mod", "go.sum", ".golangci.yml"}, tmpRepo)

	log.Printf("running tests")
	if err := runTests(tmpRepo, privateRepo, problem); err != nil {
		return err
	}

	log.Printf("running linter")
	if err := runLinter(tmpRepo, problem); err != nil {
		return err
	}

	return nil
}

// copyDir recursively copies src directory to dst.
func copyDir(baseDir, src, dst string) {
	_, err := os.Stat(filepath.Join(baseDir, src))
	if os.IsNotExist(err) {
		return
	}

	cmd := exec.Command("rsync", "-prR", src, dst)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Dir = baseDir

	if err := cmd.Run(); err != nil {
		log.Fatalf("directory copying failed: %s", err)
	}
}

// copyContents recursively copies src contents to dst.
func copyContents(baseDir, src, dst string) {
	copyDir(baseDir, src+"/", dst)
}

// copyFiles copies files preserving directory structure relative to baseDir.
//
// Existing files get replaced.
func copyFiles(baseDir string, relPaths []string, dst string) {
	for _, p := range relPaths {
		cmd := exec.Command("rsync", "-prR", p, dst)
		cmd.Dir = baseDir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		if err := cmd.Run(); err != nil {
			log.Fatalf("file copying failed: %s", err)
		}
	}
}

func randomName() string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

type TestFailedError struct {
	E error
}

func (e *TestFailedError) Error() string {
	return fmt.Sprintf("test failed: %v", e.E)
}

func (e *TestFailedError) Unwrap() error {
	return e.E
}

func runLinter(testDir, problem string) error {
	cmd := exec.Command("golangci-lint", "run", "--modules-download-mode", "readonly", "--build-tags", "private", fmt.Sprintf("./%s/...", problem))
	cmd.Dir = testDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("linter failed: %w", err)
	}

	return nil
}

// runTests runs all tests in directory with race detector.
func runTests(testDir, privateRepo, problem string) error {
	binCache, err := os.MkdirTemp("/tmp", "bincache")
	if err != nil {
		log.Fatal(err)
	}
	if err = os.Chmod(binCache, 0755); err != nil {
		log.Fatal(err)
	}

	var goCache string
	goCache, err = os.MkdirTemp("/tmp", "gocache")
	if err != nil {
		log.Fatal(err)
	}
	if err = os.Chmod(goCache, 0777); err != nil {
		log.Fatal(err)
	}

	runGo := func(arg ...string) error {
		log.Printf("> go %s", strings.Join(arg, " "))

		cmd := exec.Command("go", arg...)
		cmd.Env = append(os.Environ(), "GOFLAGS=")
		cmd.Dir = testDir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	var (
		binaries     = make(map[string]string)
		testBinaries = make(map[string]string)
		raceBinaries = make(map[string]string)
	)

	coverageReq := getCoverageRequirements(path.Join(privateRepo, problem))
	if coverageReq.Enabled {
		log.Printf("required coverage: %.2f%%", coverageReq.Percent)
	}

	testListDir := testDir
	if !coverageReq.Enabled {
		testListDir = privateRepo
	}

	//binPkgs, testPkgs := listTestsAndBinaries(filepath.Join(testDir, problem), []string{"-tags", "private", "-mod", "readonly"}) // todo return readonly
	binPkgs, testPkgs := listTestsAndBinaries(filepath.Join(testListDir, problem), []string{"-tags", "private"})
	for binaryPkg := range binPkgs {
		binPath := filepath.Join(binCache, randomName())
		binaries[binaryPkg] = binPath

		if err := runGo("build", "-mod", "readonly", "-tags", "private", "-o", binPath, binaryPkg); err != nil {
			return fmt.Errorf("error building binary in %s: %w", binaryPkg, err)
		}
	}

	binariesJSON, _ := json.Marshal(binaries)

	for testPkg := range testPkgs {
		testPath := filepath.Join(binCache, randomName())
		testBinaries[testPkg] = testPath

		cmd := []string{"test", "-mod", "readonly", "-tags", "private", "-c", "-o", testPath, testPkg}
		if coverageReq.Enabled {
			pkgs := make([]string, len(coverageReq.Packages))
			for i, pkg := range coverageReq.Packages {
				pkgs[i] = path.Join(moduleImportPath, problem, pkg)
			}
			cmd = append(cmd, "-cover", "-coverpkg", strings.Join(pkgs, ","))
		}
		if err := runGo(cmd...); err != nil {
			return fmt.Errorf("error building test in %s: %w", testPkg, err)
		}

		racePath := filepath.Join(binCache, randomName())
		raceBinaries[testPkg] = racePath

		cmd = []string{"test", "-mod", "readonly", "-race", "-tags", "private", "-c", "-o", racePath, testPkg}
		if err := runGo(cmd...); err != nil {
			return fmt.Errorf("error building test in %s: %w", testPkg, err)
		}
	}

	coverProfiles := []string{}
	for testPkg, testBinary := range testBinaries {
		relPath := strings.TrimPrefix(testPkg, moduleImportPath)
		coverProfile := path.Join(os.TempDir(), randomName())

		{
			cmd := exec.Command(testBinary)
			if coverageReq.Enabled {
				cmd = exec.Command(testBinary, "-test.coverprofile", coverProfile)
				coverProfiles = append(coverProfiles, coverProfile)
			}
			if currentUserIsRoot() {
				if err := sandbox(cmd); err != nil {
					log.Fatal(err)
				}
			}

			cmd.Dir = filepath.Join(testDir, relPath)
			cmd.Env = []string{
				testtool.BinariesEnv + "=" + string(binariesJSON),
				"PATH=" + os.Getenv("PATH"),
				"HOME=" + os.Getenv("HOME"),
				"GOCACHE=" + goCache,
			}
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr

			if err := cmd.Run(); err != nil {
				return &TestFailedError{E: err}
			}
		}

		{
			cmd := exec.Command(raceBinaries[testPkg], "-test.bench=.")
			if currentUserIsRoot() {
				if err := sandbox(cmd); err != nil {
					log.Fatal(err)
				}
			}

			cmd.Dir = filepath.Join(testDir, relPath)
			cmd.Env = []string{
				testtool.BinariesEnv + "=" + string(binariesJSON),
				"PATH=" + os.Getenv("PATH"),
				"HOME=" + os.Getenv("HOME"),
				"GOCACHE=" + goCache,
			}
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr

			if err := cmd.Run(); err != nil {
				return &TestFailedError{E: err}
			}
		}

		{
			benchCmd := exec.Command(testBinary, "-test.bench=.", "-test.run=^$")
			if currentUserIsRoot() {
				if err := sandbox(benchCmd); err != nil {
					log.Fatal(err)
				}
			}

			var buf bytes.Buffer

			benchCmd.Dir = filepath.Join(testDir, relPath)
			benchCmd.Env = []string{
				testtool.BinariesEnv + "=" + string(binariesJSON),
				"PATH=" + os.Getenv("PATH"),
				"HOME=" + os.Getenv("HOME"),
				"GOCACHE=" + goCache,
			}
			benchCmd.Stdout = &buf
			benchCmd.Stderr = os.Stderr

			if err := benchCmd.Run(); err != nil {
				return &TestFailedError{E: err}
			}

			if strings.Contains(buf.String(), "no tests to run") {
				continue
			}

			if err := compareToBaseline(testPkg, privateRepo, buf.Bytes()); err != nil {
				return err
			}
		}
	}

	if coverageReq.Enabled {
		log.Printf("checking coverage is at least %.2f%%...", coverageReq.Percent)

		percent, err := calCoverage(coverProfiles)
		if err != nil {
			return err
		}
		log.Printf("coverage is %.2f%%", percent)

		if percent < coverageReq.Percent {
			return fmt.Errorf("poor coverage %.2f%%; expected at least %.2f%%",
				percent, coverageReq.Percent)
		}
	}

	return nil
}

func noMoreThanTwoTimesWorse(old, new *benchstat.Metrics) (float64, error) {
	if new.Mean > 1.99*old.Mean {
		return 0.0, nil
	}

	return 1.0, nil
}

func compareToBaseline(testPkg, privateRepo string, run []byte) error {
	var buf bytes.Buffer

	goTest := exec.Command("go", "test", "-tags", "private,solution", "-bench=.", "-run=^$", testPkg)
	goTest.Dir = privateRepo
	goTest.Stdout = &buf
	goTest.Stderr = os.Stderr
	if err := goTest.Run(); err != nil {
		return fmt.Errorf("baseline benchmark failed: %w", err)
	}

	c := &benchstat.Collection{
		DeltaTest: noMoreThanTwoTimesWorse,
	}
	c.AddConfig("baseline.txt", buf.Bytes())
	c.AddConfig("new.txt", run)

	tables := c.Tables()
	benchstat.FormatText(os.Stderr, tables)

	for _, c := range tables {
		for _, r := range c.Rows {
			if r.Change == -1 {
				return fmt.Errorf("solution is worse than baseline on benchmark %q", r.Benchmark)
			}
		}
	}

	return nil
}

// relPaths converts paths to relative (to the baseDir) ones.
func relPaths(baseDir string, paths []string) []string {
	ret := make([]string, len(paths))
	for i, p := range paths {
		relPath, err := filepath.Rel(baseDir, p)
		if err != nil {
			log.Fatal(err)
		}
		ret[i] = relPath
	}
	return ret
}
