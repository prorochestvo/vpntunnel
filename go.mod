module vpntunnel

go 1.26

require (
	github.com/go-telegram/bot v1.22.0
	github.com/prorochestvo/dsninjector v0.0.2
	github.com/prorochestvo/loginjector v1.0.8
	github.com/stretchr/testify v1.11.1
	go.etcd.io/bbolt v1.4.3
	golang.zx2c4.com/wireguard v0.0.0-20260522210424-ecfc5a8d5446
	golang.zx2c4.com/wireguard/wgctrl v0.0.0-20241231184526-a9ab2273dd10
)

require golang.org/x/net v0.55.0 // indirect

require (
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/google/btree v1.1.3 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	golang.org/x/crypto v0.51.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	golang.zx2c4.com/wintun v0.0.0-20230126152724-0fa3db229ce2 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	gvisor.dev/gvisor v0.0.0-20250503011706-39ed1f5ac29c // indirect; pinned — newer revisions have a "two packages in same dir" build error, do not bump without verifying netstack builds
)
