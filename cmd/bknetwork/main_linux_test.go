package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstanceLockProtectsRecoveryOwnership(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockInstance(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := lockInstance(dir); err == nil {
		second()
		t.Fatal("second writer acquired lock")
	}
	unlock()
	third, err := lockInstance(dir)
	if err != nil {
		t.Fatal(err)
	}
	third()
}

func TestInstanceLockRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "keep")
	if err := os.WriteFile(target, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "instance.lock")); err != nil {
		t.Fatal(err)
	}
	if unlock, err := lockInstance(dir); err == nil {
		unlock()
		t.Fatal("followed symlink")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "untouched" {
		t.Fatal("target changed")
	}
}

func TestCLIRejectsUnknownOrExtraArguments(t *testing.T) {
	for _, args := range [][]string{{"unknown"}, {"run", "unexpected"}, {"status", "--state-dir", "relative"}, {""}} {
		if err := run(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
