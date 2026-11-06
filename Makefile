.PHONY: check quick build fmt demo-gif

check:            ## gofmt + vet + build + all tests (race, crash) + invariants + ADRs
	scripts/check.sh
quick:            ## what the hooks run
	scripts/check.sh --quick
build:
	go build -o bin/kairocode ./cmd/kairocode
	ln -sf kairocode bin/kairo        # the same command, as `kairo`
fmt:
	gofmt -w .
demo-gif: build    ## the README's recording (needs asciinema and agg)
	DEMO_PAUSE=1 KAIROCODE=$(CURDIR)/bin/kairocode asciinema rec --headless --overwrite --window-size 100x34 \
		-i 1.5 -c scripts/demo-durability.sh bin/durability.cast
	agg --idle-time-limit 1.5 --last-frame-duration 6 --font-size 14 bin/durability.cast docs/durability.gif
