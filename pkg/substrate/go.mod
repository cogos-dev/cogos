module github.com/myrgic/cogos/pkg/substrate

go 1.25.0

require (
	github.com/myrgic/cogos/pkg/bep v0.0.0
	github.com/myrgic/cogos/pkg/cogfield v0.0.0
)

require google.golang.org/protobuf v1.36.11 // indirect

replace (
	github.com/myrgic/cogos/pkg/bep => ../bep
	github.com/myrgic/cogos/pkg/cogfield => ../cogfield
)
