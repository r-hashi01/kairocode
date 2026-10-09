.PHONY: check quick build fmt

check:            ## gofmt + vet + build + all tests (race, crash) + invariants + ADRs
	scripts/check.sh
quick:            ## what the hooks run
	scripts/check.sh --quick
build:
	go build -o bin/kairocode ./cmd/kairocode
fmt:
	gofmt -w .
