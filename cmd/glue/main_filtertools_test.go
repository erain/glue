package main

import (
	"strings"
	"testing"

	"github.com/erain/glue"
)

func t(name string) glue.Tool {
	return glue.Tool{ToolSpec: glue.ToolSpec{Name: name}}
}

func TestFilterToolsNoFlagsReturnsInputUnchanged(t_ *testing.T) {
	t_.Parallel()
	in := []glue.Tool{t("read_file"), t("write_file")}
	out, err := filterTools(in, "", false)
	if err != nil {
		t_.Fatal(err)
	}
	if len(out) != 2 {
		t_.Fatalf("out = %d items, want 2", len(out))
	}
}

func TestFilterToolsNoToolsStripsAll(t_ *testing.T) {
	t_.Parallel()
	in := []glue.Tool{t("read_file"), t("write_file")}
	out, err := filterTools(in, "", true)
	if err != nil {
		t_.Fatal(err)
	}
	if out != nil {
		t_.Fatalf("out = %#v, want nil", out)
	}
}

func TestFilterToolsAllowlistPicks(t_ *testing.T) {
	t_.Parallel()
	in := []glue.Tool{t("read_file"), t("write_file"), t("grep"), t("shell_exec")}
	out, err := filterTools(in, "read_file,grep", false)
	if err != nil {
		t_.Fatal(err)
	}
	names := map[string]bool{}
	for _, x := range out {
		names[x.Name] = true
	}
	if len(out) != 2 || !names["read_file"] || !names["grep"] {
		t_.Fatalf("out names = %v, want read_file+grep", names)
	}
}

func TestFilterToolsAllowlistUnknownErrors(t_ *testing.T) {
	t_.Parallel()
	in := []glue.Tool{t("read_file")}
	_, err := filterTools(in, "read_file,does_not_exist", false)
	if err == nil || !strings.Contains(err.Error(), "does_not_exist") {
		t_.Fatalf("err = %v, want unknown-tool error", err)
	}
}

func TestFilterToolsConflictingFlagsError(t_ *testing.T) {
	t_.Parallel()
	in := []glue.Tool{t("read_file")}
	_, err := filterTools(in, "read_file", true)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t_.Fatalf("err = %v, want mutual-exclusion error", err)
	}
}

func TestFilterToolsIgnoresEmptyAndWhitespace(t_ *testing.T) {
	t_.Parallel()
	in := []glue.Tool{t("read_file"), t("write_file")}
	out, err := filterTools(in, "  read_file ,, write_file  ", false)
	if err != nil {
		t_.Fatal(err)
	}
	if len(out) != 2 {
		t_.Fatalf("out = %d items, want 2", len(out))
	}
}
