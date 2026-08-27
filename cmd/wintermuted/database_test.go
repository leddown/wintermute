package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// useServiceEnvFile points the package's idea of the service env file at a
// writable one and puts it back afterwards, so these tests do not depend on
// each other's order.
func useServiceEnvFile(t *testing.T, path string) {
	t.Helper()
	was := serviceEnvFile
	t.Cleanup(func() { serviceEnvFile = was })
	serviceEnvFile = path
}

// unsetDB clears WINTERMUTE_DB for one test. t.Setenv first, purely to
// register the cleanup that restores whatever the environment really had.
func unsetDB(t *testing.T) {
	t.Helper()
	t.Setenv("WINTERMUTE_DB", "placeholder")
	os.Unsetenv("WINTERMUTE_DB")
}

func writeEnv(t *testing.T, path, dbPath string) {
	t.Helper()
	body := "# a comment\nWINTERMUTE_ADDR=:80\nWINTERMUTE_DB=" + dbPath + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Naming a database explicitly is a statement about which one is meant, and
// nothing on disk may talk the caller out of it.
func TestEnvironmentBeatsEveryFile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeEnv(t, filepath.Join(dir, ".env"), "/from/dot-env.db")
	useServiceEnvFile(t, filepath.Join(dir, "service.env"))
	writeEnv(t, serviceEnvFile, "/from/service.db")
	t.Setenv("WINTERMUTE_DB", "/from/the/environment.db")

	path, source := resolveDatabase()
	if path != "/from/the/environment.db" {
		t.Errorf("path = %q, want the environment's", path)
	}
	if !strings.Contains(source, "environment") {
		t.Errorf("source = %q", source)
	}
}

// The point of these commands is to act on what the *service* reads, so its
// env file outranks a checkout's .env. Getting this backwards is the original
// bug wearing a different hat: the token would land in the developer's
// database instead of the server's.
func TestServiceEnvFileBeatsACheckoutDotEnv(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeEnv(t, filepath.Join(dir, ".env"), "/from/dot-env.db")
	useServiceEnvFile(t, filepath.Join(dir, "service.env"))
	writeEnv(t, serviceEnvFile, "/from/service.db")
	unsetDB(t)

	path, source := resolveDatabase()
	if path != "/from/service.db" {
		t.Errorf("path = %q, want the service's", path)
	}
	if source != serviceEnvFile {
		t.Errorf("source = %q, want %q", source, serviceEnvFile)
	}
}

// An exported but empty WINTERMUTE_DB used to be indistinguishable from an
// unset one to the caller and identical to a set one to the env loader, so it
// silently defeated every file.
func TestAnEmptyEnvironmentValueDoesNotBlockTheFiles(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	useServiceEnvFile(t, filepath.Join(dir, "service.env"))
	writeEnv(t, serviceEnvFile, "/from/service.db")
	t.Setenv("WINTERMUTE_DB", "   ")

	if path, _ := resolveDatabase(); path != "/from/service.db" {
		t.Errorf("path = %q, want the service's", path)
	}
}

func TestFallsBackToTheBuiltInDefault(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	useServiceEnvFile(t, filepath.Join(dir, "absent.env"))
	unsetDB(t)

	path, source := resolveDatabase()
	if path != "wintermute.db" {
		t.Errorf("path = %q", path)
	}
	if !strings.Contains(source, "default") {
		t.Errorf("source = %q", source)
	}
}

// The failure this file exists to prevent: a client command that creates its
// own database issues a real token into a file the server never opens, and the
// only symptom is "invalid token" from somewhere else entirely.
func TestClientCommandsRefuseToCreateADatabase(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	useServiceEnvFile(t, filepath.Join(dir, "absent.env"))
	unsetDB(t)

	st, _, err := openManaged(true)
	if err == nil {
		st.Close()
		t.Fatal("opened a database that does not exist")
	}
	if !strings.Contains(err.Error(), "Refusing to create") {
		t.Errorf("the refusal does not explain itself: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "wintermute.db")); err == nil {
		t.Error("a database was created anyway")
	}
}

// -migrate-only is how a database comes into existence, so it is the one
// caller that may.
func TestMigrateOnlyMayCreateADatabase(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	useServiceEnvFile(t, filepath.Join(dir, "absent.env"))
	t.Setenv("WINTERMUTE_DB", filepath.Join(dir, "fresh.db"))

	st, path, err := openManaged(false)
	if err != nil {
		t.Fatalf("openManaged: %v", err)
	}
	defer st.Close()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("no database at %s: %v", path, err)
	}
}
