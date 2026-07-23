package agentconfig

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nn1a/autogora/internal/model"
)

func TestPathPrecedenceAndNativeDefaults(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "users", "tester")
	tests := []struct {
		name string
		goos string
		env  map[string]string
		want string
	}{
		{
			name: "explicit config overrides data home",
			env: map[string]string{
				"AUTOGORA_CONFIG":    "/etc/autogora/agents.json",
				"AUTOGORA_DATA_HOME": "/var/lib/autogora",
			},
			want: "/etc/autogora/agents.json",
		},
		{
			name: "data home",
			env:  map[string]string{"AUTOGORA_DATA_HOME": "/var/lib/autogora"},
			want: "/var/lib/autogora/config.json",
		},
		{
			name: "linux xdg",
			goos: "linux",
			env:  map[string]string{"XDG_DATA_HOME": "/xdg/data"},
			want: "/xdg/data/autogora/config.json",
		},
		{
			name: "linux fallback",
			goos: "linux",
			want: filepath.Join(home, ".local", "share", "autogora", "config.json"),
		},
		{
			name: "macos",
			goos: "darwin",
			want: filepath.Join(home, "Library", "Application Support", "autogora", "config.json"),
		},
		{
			name: "windows local app data",
			goos: "windows",
			env:  map[string]string{"LOCALAPPDATA": filepath.Join(home, "Local")},
			want: filepath.Join(home, "Local", "autogora", "config.json"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path, err := Path(Options{
				GOOS: test.goos, HomeDirectory: home,
				Getenv: func(name string) string { return test.env[name] },
			})
			if err != nil {
				t.Fatal(err)
			}
			if path != test.want {
				t.Fatalf("path = %q, want %q", path, test.want)
			}
		})
	}

	for _, variable := range []string{"AUTOGORA_CONFIG", "AUTOGORA_DATA_HOME"} {
		t.Run("relative "+variable, func(t *testing.T) {
			_, err := Path(Options{HomeDirectory: home, Getenv: func(name string) string {
				if name == variable {
					return "relative/path"
				}
				return ""
			}})
			if err == nil || !strings.Contains(err.Error(), "absolute") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestSaveLoadRoundTripAndPermissions(t *testing.T) {
	dataHome := filepath.Join(t.TempDir(), "private", "autogora")
	options := Options{Getenv: func(name string) string {
		if name == "AUTOGORA_DATA_HOME" {
			return dataHome
		}
		return ""
	}}
	exists, err := Exists(options)
	if err != nil || exists {
		t.Fatalf("Exists before save = %t, %v", exists, err)
	}

	config := Config{
		Supervisor: Supervisor{AutoStart: true, MaxWorkers: 3, AllowWrites: true},
		Defaults:   Defaults{WorkerAgents: []string{"codex-primary"}, PlannerAgents: []string{"codex-primary"}},
		Agents: []Agent{{
			ID: "codex-primary", Runtime: model.RuntimeCodex, Model: "gpt-coding", Provider: "openai",
			Enabled: true, MaxConcurrent: 2, Roles: []Role{RoleWorker, RolePlanner}, Fallbacks: []string{"claude-backup"},
		}, {
			ID: "claude-backup", Runtime: model.RuntimeClaude, Command: "/opt/bin/claude", Model: "sonnet",
			Enabled: true, Roles: []Role{RoleWorker},
		}},
	}
	if err := Save(options, config); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dataHome, configName)
	if info, err := os.Stat(dataHome); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %v, err = %v", infoMode(info), err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v, err = %v", infoMode(info), err)
	}
	exists, err = Exists(options)
	if err != nil || !exists {
		t.Fatalf("Exists after save = %t, %v", exists, err)
	}
	loaded, err := Load(options)
	if err != nil {
		t.Fatal(err)
	}
	want := Normalize(config)
	if !reflect.DeepEqual(loaded, want) {
		t.Fatalf("loaded = %#v\nwant = %#v", loaded, want)
	}

	config.Supervisor.MaxWorkers = 4
	if err := Save(options, config); err != nil {
		t.Fatal(err)
	}
	loaded, err = Load(options)
	if err != nil || loaded.Supervisor.MaxWorkers != 4 {
		t.Fatalf("replacement load = %#v, %v", loaded, err)
	}
}

func TestLoadMissingReturnsNormalizedDefaults(t *testing.T) {
	options := Options{Getenv: func(name string) string {
		if name == "AUTOGORA_CONFIG" {
			return filepath.Join(t.TempDir(), "missing", "config.json")
		}
		return ""
	}}
	loaded, err := Load(options)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, Default()) {
		t.Fatalf("loaded = %#v, want %#v", loaded, Default())
	}
}

func TestCompareAndSwapHandlesMissingRevisionAndRejectsStaleWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	options := Options{Getenv: func(name string) string {
		if name == "AUTOGORA_CONFIG" {
			return path
		}
		return ""
	}}
	missing, err := LoadSnapshot(options)
	if err != nil {
		t.Fatal(err)
	}
	if missing.Exists || missing.Revision != MissingRevision ||
		!reflect.DeepEqual(missing.Config, Default()) {
		t.Fatalf("missing snapshot = %#v", missing)
	}
	first := missing.Config
	first.Supervisor.MaxWorkers = 2
	created, err := CompareAndSwap(options, missing.Revision, first)
	if err != nil {
		t.Fatal(err)
	}
	if !created.Exists || created.Revision == MissingRevision ||
		created.Config.Supervisor.MaxWorkers != 2 {
		t.Fatalf("created snapshot = %#v", created)
	}
	stale := missing.Config
	stale.Supervisor.MaxWorkers = 9
	_, err = CompareAndSwap(options, missing.Revision, stale)
	var conflict *RevisionConflictError
	if !errors.Is(err, ErrRevisionConflict) || !errors.As(err, &conflict) ||
		conflict.Expected != MissingRevision || conflict.Actual != created.Revision {
		t.Fatalf("stale missing-file CAS error = %#v", err)
	}
	loaded, err := LoadSnapshot(options)
	if err != nil || loaded.Config.Supervisor.MaxWorkers != 2 ||
		loaded.Revision != created.Revision {
		t.Fatalf("stale CAS changed config: %#v err=%v", loaded, err)
	}
}

func TestConcurrentCompareAndSwapHasExactlyOneWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	options := Options{Getenv: func(name string) string {
		if name == "AUTOGORA_CONFIG" {
			return path
		}
		return ""
	}}
	if err := Save(options, Default()); err != nil {
		t.Fatal(err)
	}
	base, err := LoadSnapshot(options)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	type result struct {
		workers int
		value   Snapshot
		err     error
	}
	results := make(chan result, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for _, workers := range []int{3, 7} {
		go func() {
			config := base.Config
			config.Supervisor.MaxWorkers = workers
			ready.Done()
			<-start
			value, saveErr := CompareAndSwap(options, base.Revision, config)
			results <- result{workers: workers, value: value, err: saveErr}
		}()
	}
	ready.Wait()
	close(start)
	successes, conflicts, winner := 0, 0, 0
	for range 2 {
		outcome := <-results
		switch {
		case outcome.err == nil:
			successes++
			winner = outcome.workers
		case errors.Is(outcome.err, ErrRevisionConflict):
			conflicts++
		default:
			t.Fatalf("unexpected concurrent CAS error: %v", outcome.err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("CAS outcomes: successes=%d conflicts=%d", successes, conflicts)
	}
	loaded, err := LoadSnapshot(options)
	if err != nil || loaded.Config.Supervisor.MaxWorkers != winner ||
		loaded.Revision == base.Revision {
		t.Fatalf("winner was not persisted: winner=%d loaded=%#v err=%v", winner, loaded, err)
	}
}

func TestSaveAndCompareAndSwapShareTheSameFileLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	options := Options{Getenv: func(name string) string {
		if name == "AUTOGORA_CONFIG" {
			return path
		}
		return ""
	}}
	lock, err := acquireConfigLock(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		config := Default()
		config.Supervisor.MaxWorkers = 4
		done <- Save(options, config)
	}()
	select {
	case err := <-done:
		_ = lock.Close()
		t.Fatalf("Save bypassed the config lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Save did not continue after the config lock was released")
	}
}

func TestNormalizeAndEffectiveAgents(t *testing.T) {
	config := Normalize(Config{
		Defaults: Defaults{
			WorkerAgents:  []string{" backup ", "primary", "backup"},
			PlannerAgents: []string{" primary ", "primary"},
		},
		Agents: []Agent{{
			ID: " primary ", Runtime: model.Runtime(" codex "), Model: " gpt-test ", Provider: " openai ",
			Enabled: true, Roles: []Role{" planner ", RoleWorker, RoleWorker}, Fallbacks: []string{" backup ", "backup"},
		}, {
			ID: "backup", Runtime: model.RuntimeClaude, Enabled: true,
		}, {
			ID: "disabled", Runtime: model.RuntimeGemini, Enabled: false,
		}},
	})
	if config.SchemaVersion != 1 || config.Supervisor.MaxWorkers != 1 {
		t.Fatalf("defaults were not normalized: %#v", config)
	}
	primary := config.Agents[0]
	if primary.Command != "codex" || primary.MaxConcurrent != 1 || primary.Model != "gpt-test" || primary.Provider != "openai" {
		t.Fatalf("primary = %#v", primary)
	}
	if !reflect.DeepEqual(primary.Roles, []Role{RolePlanner, RoleWorker}) || !reflect.DeepEqual(primary.Fallbacks, []string{"backup"}) {
		t.Fatalf("primary lists = %#v", primary)
	}
	if !reflect.DeepEqual(config.Defaults.WorkerAgents, []string{"backup", "primary"}) {
		t.Fatalf("worker defaults = %#v", config.Defaults.WorkerAgents)
	}
	if err := Validate(config); err != nil {
		t.Fatal(err)
	}
	effective := config.Effective(RoleWorker)
	if len(effective) != 2 || effective[0].ID != "backup" || effective[1].ID != "primary" {
		t.Fatalf("effective workers = %#v", effective)
	}
	planners := config.Effective(RolePlanner)
	if len(planners) != 1 || planners[0].ID != "primary" {
		t.Fatalf("effective planners = %#v", planners)
	}
	if _, found := config.Find(" primary "); !found {
		t.Fatal("Find did not trim the requested id")
	}
}

func TestValidateRejectsInvalidRegistriesAndFallbackCycles(t *testing.T) {
	agent := func(id string, runtime model.Runtime, roles ...Role) Agent {
		return Agent{ID: id, Runtime: runtime, Command: string(runtime), Enabled: true, MaxConcurrent: 1, Roles: roles}
	}
	tests := []struct {
		name   string
		config Config
		want   string
	}{
		{name: "unsupported schema", config: Config{SchemaVersion: 2, Supervisor: Supervisor{MaxWorkers: 1}}, want: "schemaVersion"},
		{name: "invalid id", config: Config{SchemaVersion: 1, Supervisor: Supervisor{MaxWorkers: 1}, Agents: []Agent{agent("Bad_ID", model.RuntimeCodex, RoleWorker)}}, want: "invalid agent id"},
		{name: "manual runtime", config: Config{SchemaVersion: 1, Supervisor: Supervisor{MaxWorkers: 1}, Agents: []Agent{agent("manual", model.RuntimeManual, RoleWorker)}}, want: "unsupported worker runtime"},
		{name: "duplicate id", config: Config{SchemaVersion: 1, Supervisor: Supervisor{MaxWorkers: 1}, Agents: []Agent{agent("same", model.RuntimeCodex, RoleWorker), agent("same", model.RuntimeClaude, RoleWorker)}}, want: "duplicate agent id"},
		{name: "unknown role", config: Config{SchemaVersion: 1, Supervisor: Supervisor{MaxWorkers: 1}, Agents: []Agent{agent("worker", model.RuntimeCodex, "operator")}}, want: "unknown role"},
		{name: "missing default", config: Config{SchemaVersion: 1, Supervisor: Supervisor{MaxWorkers: 1}, Defaults: Defaults{WorkerAgents: []string{"missing"}}}, want: "unknown agent"},
		{name: "wrong default role", config: Config{SchemaVersion: 1, Supervisor: Supervisor{MaxWorkers: 1}, Defaults: Defaults{JudgeAgents: []string{"worker"}}, Agents: []Agent{agent("worker", model.RuntimeCodex, RoleWorker)}}, want: "without the judge role"},
		{name: "missing fallback", config: Config{SchemaVersion: 1, Supervisor: Supervisor{MaxWorkers: 1}, Agents: []Agent{func() Agent {
			value := agent("worker", model.RuntimeCodex, RoleWorker)
			value.Fallbacks = []string{"missing"}
			return value
		}()}}, want: "unknown fallback"},
		{name: "fallback without worker role", config: Config{SchemaVersion: 1, Supervisor: Supervisor{MaxWorkers: 1}, Agents: []Agent{
			func() Agent {
				value := agent("worker", model.RuntimeCodex, RoleWorker)
				value.Fallbacks = []string{"planner"}
				return value
			}(), agent("planner", model.RuntimeClaude, RolePlanner),
		}}, want: "without the worker role"},
		{name: "self fallback", config: Config{SchemaVersion: 1, Supervisor: Supervisor{MaxWorkers: 1}, Agents: []Agent{func() Agent {
			value := agent("worker", model.RuntimeCodex, RoleWorker)
			value.Fallbacks = []string{"worker"}
			return value
		}()}}, want: "itself"},
		{name: "fallback cycle", config: Config{SchemaVersion: 1, Supervisor: Supervisor{MaxWorkers: 1}, Agents: []Agent{
			func() Agent {
				value := agent("a", model.RuntimeCodex, RoleWorker)
				value.Fallbacks = []string{"b"}
				return value
			}(),
			func() Agent {
				value := agent("b", model.RuntimeClaude, RoleWorker)
				value.Fallbacks = []string{"c"}
				return value
			}(),
			func() Agent {
				value := agent("c", model.RuntimeGemini, RoleWorker)
				value.Fallbacks = []string{"a"}
				return value
			}(),
		}}, want: "a -> b -> c -> a"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := Validate(test.config)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func infoMode(info os.FileInfo) os.FileMode {
	if info == nil {
		return 0
	}
	return info.Mode().Perm()
}
