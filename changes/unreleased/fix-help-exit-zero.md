### Fixed - `local-offload --help`, `-h` and `help` print the usage on stdout and exit 0

- **What changed.** A requested help (first argument `--help`, `-h` or `help`) now writes the usage to stdout and exits 0. A missing or unknown subcommand is unchanged: the usage goes to stderr and the exit is 2.
- **Why.** Asking for help is not a usage error; exiting 2 made `local-offload --help` look like a failure to scripts and to anyone piping the text to a pager.
- **Check.** Built the binary: `--help` and `help` exit 0, no arguments and `nosuchcmd` exit 2.
