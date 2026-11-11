// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAXDevEnvAndScripts(t *testing.T) {
	axDevDir, err := filepath.Abs(filepath.Join("..", "scripts", "ax-dev"))
	if err != nil {
		t.Fatalf("resolve scripts/ax-dev: %v", err)
	}
	envScript := filepath.Join(axDevDir, "env.sh")

	t.Run("pinned versions", func(t *testing.T) {
		raw, err := os.ReadFile(filepath.Join(axDevDir, "versions.env"))
		if err != nil {
			t.Fatalf("read versions.env: %v", err)
		}
		text := string(raw)
		for _, want := range []string{
			`SUBSTRATE_TAG:-v1.0.0}`,
			`SUBSTRATE_COMMIT:-b34fbee742906494b5eb4f1f36987176bf8770a4}`,
			`AX_COMMIT:-56bd0fc55574e48ad2ce952a7ea38730694d1fcf}`,
		} {
			if !strings.Contains(text, want) {
				t.Fatalf("versions.env missing %q:\n%s", want, text)
			}
		}
	})

	cases := []struct {
		name      string
		imagesEnv string
		wantImage string
	}{
		{
			name:      "default task image before build",
			wantImage: "localhost:5001/glue-task:dev",
		},
		{
			name:      "loads digest-pinned image from images.env",
			imagesEnv: "export GLUE_TASK_IMAGE=\"localhost:5001/glue-task@sha256:111122223333\"\n",
			wantImage: "localhost:5001/glue-task@sha256:111122223333",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			state := filepath.Join(home, ".cache", "glue", "ax-dev")
			if err := os.MkdirAll(filepath.Join(home, ".kube"), 0o700); err != nil {
				t.Fatalf("mkdir .kube: %v", err)
			}
			kubeCfg := filepath.Join(home, ".kube", "config")
			const sentinel = "current-context: kind-e2e\n"
			if err := os.WriteFile(kubeCfg, []byte(sentinel), 0o600); err != nil {
				t.Fatalf("write kubeconfig: %v", err)
			}
			if tc.imagesEnv != "" {
				if err := os.MkdirAll(state, 0o755); err != nil {
					t.Fatalf("mkdir state: %v", err)
				}
				if err := os.WriteFile(filepath.Join(state, "images.env"), []byte(tc.imagesEnv), 0o644); err != nil {
					t.Fatalf("write images.env: %v", err)
				}
			}

			cmd := exec.Command("bash", "-c", `
				flags_before="$-"
				source "$1"
				source "$1"
				flags_after="$-"
				printf 'FLAGS=%s->%s\nKUBECONFIG=%s\nKUBE_CONTEXT=%s\nGLUE_AX_CONTEXT=%s\nGLUE_TASK_IMAGE=%s\nPATH_COUNT=%d\n' \
					"$flags_before" "$flags_after" "$KUBECONFIG" "$KUBE_CONTEXT" "$GLUE_AX_CONTEXT" "$GLUE_TASK_IMAGE" \
					"$(tr ':' '\n' <<<"$PATH" | grep -Fxc "$STATE/bin")"
			`, "bash", envScript)
			cmd.Env = []string{
				"HOME=" + home,
				"PATH=/usr/bin:/bin",
				"KUBECONFIG=" + kubeCfg,
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("source env.sh failed: %v\n%s", err, out)
			}
			got := string(out)
			for _, want := range []string{
				"KUBECONFIG=" + filepath.Join(state, "kubeconfig"),
				"KUBE_CONTEXT=kind-glue-ax",
				"GLUE_AX_CONTEXT=kind-glue-ax",
				"GLUE_TASK_IMAGE=" + tc.wantImage,
				"PATH_COUNT=1",
			} {
				if !strings.Contains(got, want+"\n") {
					t.Fatalf("missing %q in output:\n%s", want, got)
				}
			}
			firstLine := strings.SplitN(got, "\n", 2)[0]
			parts := strings.SplitN(strings.TrimPrefix(firstLine, "FLAGS="), "->", 2)
			if len(parts) != 2 || parts[0] != parts[1] {
				t.Fatalf("env.sh mutated shell flags: %s", firstLine)
			}
			afterKube, err := os.ReadFile(kubeCfg)
			if err != nil || string(afterKube) != sentinel {
				t.Fatalf("~/.kube/config was mutated: got %q, err=%v", afterKube, err)
			}

			evalCmd := exec.Command("bash", "-c", `eval "$("$1")" && printf '%s|%s' "$KUBECONFIG" "$GLUE_TASK_IMAGE"`, "bash", envScript)
			evalCmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
			evalOut, err := evalCmd.CombinedOutput()
			if err != nil || string(evalOut) != filepath.Join(state, "kubeconfig")+"|"+tc.wantImage {
				t.Fatalf("direct execution eval got %q, err=%v", evalOut, err)
			}
		})
	}

	t.Run("smoke and e2e scripts keep standing cluster intact", func(t *testing.T) {
		for _, rel := range []string{"env.sh", "up.sh", "status.sh", "smoke.sh", "e2e/submit.sh", "e2e/suspend-resume.sh"} {
			p := filepath.Join(axDevDir, rel)
			if out, err := exec.Command("bash", "-n", p).CombinedOutput(); err != nil {
				t.Fatalf("bash -n %s: %v\n%s", rel, err, out)
			}
			if rel == "smoke.sh" || strings.HasPrefix(rel, "e2e/") {
				raw, err := os.ReadFile(p)
				if err != nil {
					t.Fatalf("read %s: %v", rel, err)
				}
				if strings.Contains(string(raw), "kind delete cluster") || strings.Contains(string(raw), "down.sh") {
					t.Fatalf("%s must not tear down the standing kind cluster", rel)
				}
			}
		}
	})
}
