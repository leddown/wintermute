package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// unsetRepo clears WINTERMUTE_MODEL_REPO for one test, registering the cleanup
// that puts back whatever the environment really had — the env files these
// tests write are applied to the process environment when they are read.
func unsetRepo(t *testing.T) {
	t.Helper()
	t.Setenv("WINTERMUTE_MODEL_REPO", "placeholder")
	os.Unsetenv("WINTERMUTE_MODEL_REPO")
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// The point of the flag: a directory that is not there is made, and blessed,
// with no server running and no database to open. This is the reformatted
// drive, which is the case that used to have no way out.
func TestInitRepoCreatesAndBlessesTheDirectory(t *testing.T) {
	dir := t.TempDir()
	useServiceEnvFile(t, filepath.Join(dir, "service.env"))
	repo := filepath.Join(dir, "mount", "model_repo")
	t.Setenv("WINTERMUTE_MODEL_REPO", repo)

	if err := initRepo(quietLogger()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".wintermute-repo")); err != nil {
		t.Fatalf("marker: %v", err)
	}
}

// Running it twice is how an operator checks whether it worked, and must not
// be a way to break something.
func TestInitRepoIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	useServiceEnvFile(t, filepath.Join(dir, "service.env"))
	t.Setenv("WINTERMUTE_MODEL_REPO", dir)

	for i := 0; i < 2; i++ {
		if err := initRepo(quietLogger()); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
}

// The command has to act on what the *service* reads, by the same rule as the
// database commands: the service env file outranks a checkout's .env. Getting
// this backwards blesses a developer's directory and leaves the server's
// repository exactly as unusable as it was.
func TestInitRepoPrefersTheServiceEnvFile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	unsetRepo(t)

	fromService := filepath.Join(dir, "service-repo")
	fromCheckout := filepath.Join(dir, "checkout-repo")
	useServiceEnvFile(t, filepath.Join(dir, "service.env"))
	if err := os.WriteFile(serviceEnvFile, []byte("WINTERMUTE_MODEL_REPO="+fromService+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("WINTERMUTE_MODEL_REPO="+fromCheckout+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := initRepo(quietLogger()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fromService, ".wintermute-repo")); err != nil {
		t.Errorf("the service's repository was not initialised: %v", err)
	}
	if _, err := os.Stat(fromCheckout); err == nil {
		t.Errorf("the checkout's .env won, and %s was blessed instead", fromCheckout)
	}
}

// An unset path is the one case where doing nothing is right, and it has to
// say where it looked — the setting is usually in a file the operator's shell
// never sees.
func TestInitRepoRefusesWhenNothingIsConfigured(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	unsetRepo(t)
	useServiceEnvFile(t, filepath.Join(dir, "absent.env"))

	err := initRepo(quietLogger())
	if err == nil {
		t.Fatal("want an error when WINTERMUTE_MODEL_REPO is unset")
	}
	if !strings.Contains(err.Error(), serviceEnvFile) {
		t.Errorf("error does not say where it looked: %v", err)
	}
}

// A relative path is refused rather than resolved against whatever directory
// the command happened to run in, matching config.Load.
func TestInitRepoRefusesARelativePath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	useServiceEnvFile(t, filepath.Join(dir, "absent.env"))
	t.Setenv("WINTERMUTE_MODEL_REPO", "models")

	if err := initRepo(quietLogger()); err == nil {
		t.Fatal("want an error for a relative WINTERMUTE_MODEL_REPO")
	}
	if _, err := os.Stat(filepath.Join(dir, "models")); err == nil {
		t.Error("a relative path was created anyway")
	}
}
