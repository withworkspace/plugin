# Build everything, and the example plugin to examples/hello/ws-hello.
build:
    go build ./...
    cd examples/hello && go build -o ws-hello .

# The framework's tests, then the example's.
test:
    go test ./...
    cd examples/hello && go test ./...

check:
    gofmt -l . | grep . && exit 1 || true
    go vet ./...
    cd examples/hello && go vet ./...
    go build ./...
    cd examples/hello && go tool apigen -check

golangci := "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0"

# golangci-lint (.golangci.yml) on the framework and its example. Built
# with this repo's Go, which a release may be older than.
lint:
    go run {{golangci}} run ./...
    cd examples/hello && go run {{golangci}} run ./...

# Write the example's ui/src/api.gen.ts again from its Go: what its
# endpoints take and answer, for its app.
api:
    cd examples/hello && go tool apigen

# Build the example's app into examples/hello/ui/dist, which is committed:
# Go alone builds the plugin. It needs Node and npm.
ui:
    cd examples/hello/ui && npm ci && npm run build

# Fail if the example's committed app isn't what its source builds.
ui-check: ui
    git diff --exit-code -- examples/hello/ui/dist

# Bring the protocol's types here from github.com/withworkspace/protocol, as
# generated from its spec at ref (a tag, plugin/v1.2.0, or a branch), for
# both kits: Go's protocol package is its file and its own rules.go, and the
# TypeScript kit's is ts/src/protocol.gen.ts.
protocol ref="main":
    gh api "repos/withworkspace/protocol/contents/plugin/gen/go/protocol.gen.go?ref={{ref}}" -H "Accept: application/vnd.github.raw" > protocol/protocol.gen.go
    gh api "repos/withworkspace/protocol/contents/plugin/gen/ts/protocol.gen.ts?ref={{ref}}" -H "Accept: application/vnd.github.raw" > ts/src/protocol.gen.ts
    go build ./... && go test ./protocol
    cd ts && npm run typecheck && npm test

# The TypeScript kit (ts/): typechecked, tested, built.
ts:
    cd ts && npm ci && npm run typecheck && npm test && npm run build
