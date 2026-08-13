module github.com/dmmdea/offload-harness

go 1.26.5

require (
	github.com/elastic/go-seccomp-bpf v1.6.0
	github.com/landlock-lsm/go-landlock v0.9.0
	github.com/modelcontextprotocol/go-sdk v1.6.1
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2
	go.etcd.io/bbolt v1.5.0
	golang.org/x/sys v0.46.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v0.1.9 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/exp v0.0.0-20250305212735-054e65f0b394 // indirect
	modernc.org/libc v1.62.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.9.1 // indirect
	modernc.org/sqlite v1.37.0 // indirect
)

require (
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/segmentio/asm v1.2.1 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/net v0.41.0 // indirect
	golang.org/x/oauth2 v0.36.0 // indirect
	golang.org/x/text v0.38.0 // indirect
	kernel.org/pub/linux/libs/security/libcap/psx v1.2.77 // indirect
	llamaswap-pp-cli v0.0.0
)

replace llamaswap-pp-cli => ./tools/llamaswap
