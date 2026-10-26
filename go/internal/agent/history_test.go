package agent_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestHistory(t *testing.T) {
	for name, tc := range map[string]struct {
		shell, file, content string
		want                 []string
	}{
		"bash with timestamps": {
			shell: "/bin/bash", file: ".bash_history",
			content: "#1700000000\nls\n#1700000001\ngit status\nls\n",
			want:    []string{"ls", "git status"},
		},
		"zsh extended and multi-line": {
			shell: "/run/current-system/sw/bin/zsh", file: ".zsh_history",
			content: ": 1700000000:0;make\n: 1700000001:0;for f in *; do\\\necho $f\\\ndone\n: 1700000002:0;echo \xc5\x83\xa3\n",
			want:    []string{"echo Ń", "for f in *; do\necho $f\ndone", "make"},
		},
		"fish": {
			shell: "/run/current-system/sw/bin/fish", file: ".local/share/fish/fish_history",
			content: "- cmd: nix build\n  when: 1700000000\n- cmd: echo a\\\\nb\\nc\n  when: 1700000001\n  paths:\n    - b\n- cmd: nix build\n  when: 1700000002\n",
			want:    []string{"nix build", "echo a\\nb\nc"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, tc.file)
			must(t, os.MkdirAll(filepath.Dir(path), 0o755))
			must(t, os.WriteFile(path, []byte(tc.content), 0o600))
			cfg := baseConfig()
			cfg["shell"] = tc.shell
			cfg["history"] = path
			h := start(t, cfg, unixAddr)
			ctl, err := h.client.Control()
			if err != nil {
				t.Fatal(err)
			}
			defer ctl.Close()
			got, err := ctl.History(0)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("history = %q, want %q", got, tc.want)
			}
			if got, _ := ctl.History(1); len(got) != 1 {
				t.Errorf("limit 1 gave %q", got)
			}
		})
	}
}

func TestHistoryMissingFile(t *testing.T) {
	cfg := baseConfig()
	cfg["history"] = filepath.Join(t.TempDir(), "none")
	h := start(t, cfg, unixAddr)
	ctl, err := h.client.Control()
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	got, err := ctl.History(0)
	if err != nil || len(got) != 0 {
		t.Errorf("history = %q, %v; want empty", got, err)
	}
}
