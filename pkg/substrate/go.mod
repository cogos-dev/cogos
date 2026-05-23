module github.com/myrgic/cogos/pkg/substrate

go 1.25.0

require (
	github.com/myrgic/cogos/pkg/cogfield v0.0.0
	google.golang.org/protobuf v1.36.11
)

replace (
	github.com/myrgic/cogos/pkg/cogfield => ../cogfield
)
