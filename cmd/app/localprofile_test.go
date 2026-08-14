package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"scm.x5.ru/dis.cloud/core/provision-agent/pkg/userconfig"
)

// The profile reaches a box in an environment variable, and a variable with a
// newline in it does not arrive whole: the agent in a salty-claw box is started
// through s6-envdir, which takes the first line of a file and drops the rest. A
// pretty-printed profile arrived as "{" and stopped the box — correctly, as a
// malformed profile should, except this one was written correctly.
func TestLocalProfileTravelsOnOneLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "user-config.json")
	pretty := `{
  "gitConfigs": [
    {
      "host": "github.com",
      "user": "dhnikolas",
      "email": "dhnikolas@gmail.com",
      "name": "dhnikolas",
      "token": "t"
    }
  ]
}`
	if err := os.WriteFile(path, []byte(pretty), 0o600); err != nil {
		t.Fatal(err)
	}

	uc, raw, err := loadLocalUserConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if uc == nil {
		t.Fatal("the profile was not read")
	}
	if strings.ContainsAny(raw, "\n\r") {
		t.Errorf("the value carries a line break:\n%s", raw)
	}

	// And it is still the same profile after the squeeze.
	back, err := userconfig.Parse(raw)
	if err != nil {
		t.Fatalf("what was handed on does not parse: %v", err)
	}
	if len(back.Spec.GitConfigs) != 1 || back.Spec.GitConfigs[0].Host != "github.com" {
		t.Errorf("parsed back = %+v", back.Spec)
	}
}

// Most machines keep no profile, and that is not a failure.
func TestNoLocalProfileIsFine(t *testing.T) {
	uc, raw, err := loadLocalUserConfig(filepath.Join(t.TempDir(), "absent.json"))
	if uc != nil || raw != "" || err != nil {
		t.Errorf("loadLocalUserConfig = %v, %q, %v", uc, raw, err)
	}
}

// One that is there and is not a profile stops the agent: it was put there on
// purpose, and ignoring it would leave its owner believing it applied.
func TestMalformedLocalProfileIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user-config.json")
	if err := os.WriteFile(path, []byte(`{"gitConfigs":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadLocalUserConfig(path); err == nil {
		t.Error("a malformed profile was accepted")
	}
}
