# server/ の開発用タスク。ツールは go run でバージョンを固定して動かす（グローバルに入れない）。
AIR_VERSION := v1.67.4
GOLANGCI_LINT_VERSION := v2.15.0

COVERPROFILE := coverage.out

.PHONY: all generate build test test-short cover lint dev check-generated

all: generate build lint test

## generate: api/openapi.yaml（oapi-codegen）と internal/db の SQL（sqlc）からコードを生成する
generate:
	go generate ./...

## build: ビルドする
build:
	go build ./...

## test: 結合テスト（testcontainers、Docker が必要）を含めて、race detector とカバレッジ付きでテストする
##       （合計は生成コードを除いて計算する）
test:
	go test -race -covermode=atomic -coverprofile=$(COVERPROFILE).tmp ./...
	@grep -v -e '/internal/api/gen/' -e '/internal/db/sqlc/' $(COVERPROFILE).tmp > $(COVERPROFILE) && rm -f $(COVERPROFILE).tmp
	@go tool cover -func=$(COVERPROFILE) | tail -n 1

## test-short: Docker を使う結合テストを飛ばして、ユニットテストだけを動かす
test-short:
	go test -short -race ./...

## cover: カバレッジを HTML で開く
cover: test
	go tool cover -html=$(COVERPROFILE)

## lint: golangci-lint を動かす
lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

## dev: air でホットリロードしながら API を動かす（../.env を読む）
dev:
	go run github.com/air-verse/air@$(AIR_VERSION)

## check-generated: 生成物が api/openapi.yaml と internal/db の SQL に一致しているか確かめる
check-generated: generate
	git diff --exit-code -- . ../api
	@test -z "$$(git status --porcelain -- .)" || { git status --porcelain -- .; echo "untracked generated files" >&2; exit 1; }
