.PHONY: test build acceptance composition-acceptance ledger site-build site-contract-check legal-check

SITE_PYTHON ?= python3

test:
	go test -race ./...
	mvn -B -q -f maven-plugin/pom.xml verify
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s acceptance -p '*_test.py' -v
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s integration-tests/live-v2 -p 'test_*.py' -v
	python3 acceptance/check.py --structure-only
	python3 acceptance/composition_check.py --structure-only

build:
	mkdir -p bin
	CGO_ENABLED=0 go build -o bin/brewlet ./cmd/brewlet
	go run ./scripts/notice --output bin/NOTICE.txt ./cmd/brewlet
	cp LICENSE.txt bin/LICENSE.txt

legal-check:
	mkdir -p bin/notices
	@set -e; for arch in amd64 arm64; do \
	  go run ./scripts/notice --goos linux --goarch $$arch --output bin/notices/cli-$$arch.txt ./cmd/brewlet; \
	  go run ./scripts/notice --goos linux --goarch $$arch --output bin/notices/control-$$arch.txt ./kubernetes/cmd/operator ./kubernetes/cmd/admission; \
	  go run ./scripts/notice --goos linux --goarch $$arch --output bin/notices/worker-$$arch.txt ./cmd/brewlet-node ./shim/cmd/containerd-shim-brewlet-v2; \
	done
	python3 scripts/check_notices.py

ledger:
	python3 acceptance/check.py

acceptance:
	python3 acceptance/check.py --require-complete
	python3 acceptance/composition_check.py --require-complete
	python3 acceptance/qualify.py

composition-acceptance:
	python3 acceptance/composition_check.py --require-complete

site-contract-check:
	PYTHONDONTWRITEBYTECODE=1 $(SITE_PYTHON) -m unittest discover -s site/scripts -p 'test_*.py' -v
	node --check site/assets/js/site.js

site-build: site-contract-check
	$(SITE_PYTHON) site/scripts/build.py
