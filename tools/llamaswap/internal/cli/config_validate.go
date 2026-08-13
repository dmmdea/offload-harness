// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// pp:data-source local

package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/lsconfig"
)

func newConfigValidateCmd(flags *rootFlags) *cobra.Command {
	var showSchema bool

	cmd := &cobra.Command{
		Use:   "validate [file]",
		Short: "Check a config against the embedded llama-swap JSON schema, plus nearest-key hints for unknown top-level keys.",
		Long: "Validate a llama-swap config against the vendored upstream draft-07 schema.\n\n" +
			"Two layers, because the schema alone is not enough: upstream leaves\n" +
			"additionalProperties unset at the top level, so a misspelled top-level key\n" +
			"(macro: for macros:, startport: for startPort:) validates CLEAN and is then\n" +
			"silently ignored by llama-swap at boot. This command adds an\n" +
			"unknown-top-level-key check with a nearest-key suggestion on top.\n\n" +
			"The file is opened read-only. Omit [file] to validate the live config.",
		Example: "  llamaswap-pp-cli config validate\n" +
			"  llamaswap-pp-cli config validate ./candidate.yaml --json\n" +
			"  llamaswap-pp-cli config validate --show-schema",
		Annotations: map[string]string{
			"mcp:read-only":       "true",
			"pp:typed-exit-codes": fmt.Sprintf("0,2,%d", ExitConfigInvalid),
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if showSchema {
				_, err := out.Write(lsconfig.SchemaBytes())
				return err
			}
			path, err := resolveConfigPath(args, 0)
			if err != nil {
				return err
			}
			if dryRunOK(flags) {
				return writeDryRun(out, flags, "config validate "+path)
			}
			f, err := loadConfigFile(path)
			if err != nil {
				return err
			}
			res, err := lsconfig.Validate(f)
			if err != nil {
				return err
			}
			if wantsJSON(out, flags) {
				if err := printJSONFiltered(out, res, flags); err != nil {
					return err
				}
			} else {
				fmt.Fprintf(out, "%s\n", bold(res.Path))
				fmt.Fprintf(out, "  sha256 %s  models %d\n", res.Sha256[:16], res.ModelCount)
				fmt.Fprintf(out, "  schema %s (retrieved %s, llama-swap %s)\n", res.SchemaSource, res.SchemaRetrieved, res.SchemaForVersion)
				if res.Valid {
					fmt.Fprintf(out, "\n%s\n", green("VALID — no schema violations and no unknown top-level keys"))
				} else {
					fmt.Fprintf(out, "\n%s\n", red(fmt.Sprintf("INVALID — %d issue(s)", len(res.Issues))))
					for _, is := range res.Issues {
						loc := is.Pointer
						if loc == "" {
							loc = "(root)"
						}
						if is.Line > 0 {
							fmt.Fprintf(out, "  line %-4d %-28s %s\n", is.Line, loc, is.Message)
						} else {
							fmt.Fprintf(out, "  %-33s %s\n", loc, is.Message)
						}
					}
				}
			}
			if !res.Valid {
				return errConfigInvalid(fmt.Errorf("%s failed validation with %d issue(s)", path, len(res.Issues)))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&showSchema, "show-schema", false, "print the embedded llama-swap config schema and exit")
	return cmd
}
