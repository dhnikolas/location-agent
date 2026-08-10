package runtime

import (
	"testing"

	platform "scm.x5.ru/dis.cloud/core/agent-platform-cloop/api"
	pa "scm.x5.ru/dis.cloud/core/provision-agent/api"
)

// A template can ask for parameters of its own; the answers are the user's and
// live in their config. They are more specific than either the template's own
// env or the user's general env, so they land on top of both — and the
// platform's wiring still wins over everything, or a box could be pointed at
// another shard from a profile.
func TestTemplateParamsBeatTemplateAndUserEnv(t *testing.T) {
	tpl := templateWithHome()
	tpl.Name = "claude-box"
	tpl.Spec.Container.Env = []platform.EnvVar{
		{Name: "MODEL", Value: "from-template"},
		{Name: "SHARED", Value: "from-template"},
	}

	uc := &pa.UserConfig{}
	uc.Spec.Env = map[string]string{"SHARED": "from-user", "MODEL": "from-user-env"}
	uc.Spec.RuntimeEnvParams = map[string]map[string]string{
		"claude-box": {"MODEL": "from-template-params", "RUNTIME_NAME": "hijacked"},
	}

	rt := &platform.Runtime{}
	rt.Name = "box"
	c, err := Plan(Input{
		Runtime: rt, Template: tpl, UserConfig: uc, Location: "laptop",
		AgentEnv: map[string]string{"RUNTIME_NAME": "box"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if c.Env["MODEL"] != "from-template-params" {
		t.Errorf("MODEL = %q, want the template's own parameter to win", c.Env["MODEL"])
	}
	if c.Env["SHARED"] != "from-user" {
		t.Errorf("SHARED = %q, want the user's general env where no parameter overrides it", c.Env["SHARED"])
	}
	if c.Env["RUNTIME_NAME"] != "box" {
		t.Errorf("RUNTIME_NAME = %q — a profile must not be able to repoint a box", c.Env["RUNTIME_NAME"])
	}
}

// A template with no schema asks for nothing, and a profile with no answers for
// it changes nothing. Every template written before this field is that case.
func TestNoParamsChangesNothing(t *testing.T) {
	tpl := templateWithHome()
	tpl.Name = "claude-box"
	tpl.Spec.Container.Env = []platform.EnvVar{{Name: "MODEL", Value: "from-template"}}

	uc := &pa.UserConfig{}
	uc.Spec.RuntimeEnvParams = map[string]map[string]string{"other-template": {"MODEL": "not-mine"}}

	rt := &platform.Runtime{}
	rt.Name = "box"
	c, err := Plan(Input{Runtime: rt, Template: tpl, UserConfig: uc, Location: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Env["MODEL"] != "from-template" {
		t.Errorf("MODEL = %q — another template's parameters leaked in", c.Env["MODEL"])
	}
}

// The machine's own profile travels to the box verbatim, for the agent inside
// to apply to what it reads itself — this side only turns a profile into the
// environment and the files.
func TestLocalUserConfigReachesTheBox(t *testing.T) {
	rt := &platform.Runtime{}
	rt.Name = "box"
	raw := `{"gitConfigs":[{"host":"github.com","user":"me","token":"t"}]}`

	c, err := Plan(Input{
		Runtime:         rt,
		Template:        templateWithHome(),
		Location:        "laptop",
		LocalUserConfig: raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Env["LOCAL_USER_CONFIG"] != raw {
		t.Errorf("LOCAL_USER_CONFIG = %q, want the profile as written", c.Env["LOCAL_USER_CONFIG"])
	}
}

// A machine that keeps no profile must plan exactly the container it planned
// before: the variable is part of the spec hash, and an empty one set anyway
// would recreate every box on every machine once, for a feature none of them
// use.
func TestNoLocalUserConfigLeavesTheSpecAlone(t *testing.T) {
	rt := &platform.Runtime{}
	rt.Name = "box"
	in := Input{Runtime: rt, Template: templateWithHome(), Location: "laptop"}

	c, err := Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	if _, set := c.Env["LOCAL_USER_CONFIG"]; set {
		t.Error("the variable was set for a machine that has no profile")
	}
}
