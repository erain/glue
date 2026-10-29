package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/erain/glue/providers"
)

type catalogProvider struct {
	Provider     string                `json:"provider"`
	DefaultModel string                `json:"default_model"`
	EnvKey       string                `json:"env_key,omitempty"`
	ModelsURL    string                `json:"models_url,omitempty"`
	Models       []providers.ModelInfo `json:"models"`
}

func modelsCommand(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("glue models", flag.ContinueOnError)
	flags.SetOutput(stderr)
	provider := flags.String("provider", "", "filter by provider")
	asJSON := flags.Bool("json", false, "print catalog as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("glue models accepts no positional arguments")
	}
	names := providers.Known()
	if *provider != "" {
		name := strings.ToLower(strings.TrimSpace(*provider))
		if _, ok := providers.Lookup(name); !ok {
			return fmt.Errorf("unknown provider %q", name)
		}
		names = []string{name}
	}
	catalog := make([]catalogProvider, 0, len(names))
	for _, name := range names {
		f, _ := providers.Lookup(name)
		catalog = append(catalog, catalogProvider{name, f.DefaultModel, f.EnvKey, f.ModelsListURL, providers.Models(name)})
	}
	if *asJSON {
		return json.NewEncoder(stdout).Encode(catalog)
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "PROVIDER\tMODEL\tDEFAULT\tTHINKING LEVELS")
	for _, p := range catalog {
		for _, m := range p.Models {
			marker := ""
			if m.ID == p.DefaultModel {
				marker = "*"
			}
			levels := strings.Join(m.ThinkingLevels, ", ")
			if levels == "" {
				levels = "provider default"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.Provider, m.ID, marker, levels)
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintln(stdout, "Curated choices; account access is not checked. Custom IDs are accepted by --model.\nUse --json for credential names and provider catalog URLs.")
	return err
}
