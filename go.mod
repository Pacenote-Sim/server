module github.com/pacenote-sim/server

go 1.26.1

// The floor is a security floor, not a language one: go1.26.6 fixes seven
// vulnerabilities this server is affected by, in net/url, net/http, crypto/tls,
// html/template, encoding/xml and encoding/asn1. go.mod still says 1.26.1 as
// the language version, so nothing here needs a newer compiler to be
// understood — this only stops the binary being built by a toolchain with
// known holes in its standard library.
toolchain go1.26.8

require (
	github.com/hashicorp/go-hclog v1.6.3
	github.com/hashicorp/go-plugin v1.8.0
	github.com/jackc/pgx/v5 v5.9.2
	github.com/pacenote-sim/plugin v0.3.0
	github.com/pacenote-sim/protocol v0.2.0
	github.com/pressly/goose/v3 v3.26.0
	github.com/prometheus/client_golang v1.23.2
	github.com/sassoftware/relic/v8 v8.2.0
	github.com/stretchr/testify v1.12.1
	go.uber.org/goleak v1.3.0
	go.yaml.in/yaml/v3 v3.0.5
	golang.org/x/crypto v0.55.0
	golang.org/x/sync v0.23.0
	software.sslmate.com/src/go-pkcs12 v0.7.3
)

require (
	al.essio.dev/pkg/shellescape v1.5.1 // indirect
	github.com/ProtonMail/go-crypto v1.0.0 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cloudflare/circl v1.3.8 // indirect
	github.com/danieljoos/wincred v1.2.2 // indirect
	github.com/fatih/color v1.18.0 // indirect
	github.com/godbus/dbus/v5 v5.1.0 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/hashicorp/yamux v0.1.2 // indirect
	github.com/howeyc/gopass v0.0.0-20210920133722-c8aef6fb66ef // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/klauspost/compress v1.19.2 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mfridman/interpolate v0.0.2 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/oklog/run v1.1.0 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.66.1 // indirect
	github.com/prometheus/procfs v0.16.1 // indirect
	github.com/sethvargo/go-retry v0.3.0 // indirect
	github.com/spf13/cobra v1.8.1 // indirect
	github.com/spf13/pflag v1.0.5 // indirect
	github.com/zalando/go-keyring v0.2.6 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.yaml.in/yaml/v2 v2.4.2 // indirect
	golang.org/x/exp v0.0.0-20260410095643-746e56fc9e2f // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/term v0.45.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/grpc v1.83.2 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
	modernc.org/sqlite v1.44.3 // indirect
)
