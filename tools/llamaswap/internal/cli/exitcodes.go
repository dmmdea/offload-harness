package cli

// Typed exit codes for unattended callers (nightshift agents, the harness).
// These extend the generated framework codes (0 success, 1 generic error,
// 2 usage, 3 not-found, 4 unreachable, 5 auth, 7 rate-limit, 10 partial)
// with llama-swap-specific outcomes. Registered here as the single shared
// contract; all waves reference these constants, never raw ints.
//
// Commands that intentionally exit with one of these for non-error control
// flow MUST set cmd.Annotations["pp:typed-exit-codes"] accordingly and
// document the codes in their help text.
const (
	ExitOK = 0
	// ExitModelNotFound: the named model/alias resolves to nothing in the
	// roster (checked against ids AND meta.llamaswap.aliases).
	ExitModelNotFound = 3
	// ExitServerUnreachable: the proxy did not answer on 127.0.0.1.
	ExitServerUnreachable = 4
	// ExitKeepsetRefusal: the operation would touch a protected keep-set
	// member (matched by id or alias, sourced from config — never server ttl).
	ExitKeepsetRefusal = 20
	// ExitDrainTimeout: --drain could not confirm idle within the timeout;
	// nothing was unloaded (fail closed).
	ExitDrainTimeout = 21
	// ExitDrainUnobservable: /slots was unreadable (timeout/5xx) for one or
	// more targets; nothing was unloaded (fail closed). 404 endpoint-absent
	// falls back to the documented alternative check instead of this code.
	ExitDrainUnobservable = 22
	// ExitPortConflict: a scratch/test instance port is already listening or
	// sits inside the startPort span / reserved band.
	ExitPortConflict = 23
	// ExitConfigInvalid: schema or semantic validation failed.
	ExitConfigInvalid = 24
	// ExitDrift: live process flags diverge from the file (config drift,
	// seat show --diff-yaml). Not an error; a finding.
	ExitDrift = 25
	// ExitProbeFailed: verify --probe found the memory stack answering but
	// DEGRADED (cosine/score outside stored tolerance).
	ExitProbeFailed = 26
	// ExitUpstream5xx: the upstream model server answered 5xx.
	ExitUpstream5xx = 27
	// ExitFitRefusal: fit/ctx verdict lands inside the uncertainty band —
	// the command refuses to answer rather than guess. Also raised when the
	// header makes the standard KV formula inapplicable (MLA/SSM), when a
	// shard set is incomplete, or when the file is not a model at all.
	ExitFitRefusal = 28
	// ExitNotComparable: two measurement rows carry different comparability
	// keys, so their difference measures the configuration change rather than
	// the thing being compared. A finding, not a failure — the command
	// printed both rows and named the differing fields before exiting.
	ExitNotComparable = 29
)
