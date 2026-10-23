.PHONY: test test-python build acceptance composition-acceptance ledger site-build site-contract-check notices go-notices maven-notices site-notices site-notices-check legal-check release-check release-candidate

SITE_PYTHON ?= python3

test:
	go test -race ./...
	mvn -B -q -f maven-plugin/pom.xml verify
	$(MAKE) test-python

test-python:
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s acceptance -p '*_test.py' -v
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s integration-tests/live-v2 -p 'test_*.py' -v
	python3 acceptance/check.py --structure-only
	python3 acceptance/composition_check.py --structure-only

build:
	mkdir -p bin
	CGO_ENABLED=0 go build -o bin/brewlet ./cmd/brewlet
	go run ./scripts/notice --output bin/NOTICE.txt ./cmd/brewlet
	cp LICENSE.txt bin/LICENSE.txt

notices: go-notices maven-notices site-notices

go-notices:
	mkdir -p bin/notices
	@set -e; for arch in amd64 arm64; do \
	  go run ./scripts/notice --goos linux --goarch $$arch --output bin/notices/cli-$$arch.txt ./cmd/brewlet; \
	  go run ./scripts/notice --goos linux --goarch $$arch --output bin/notices/control-$$arch.txt ./kubernetes/cmd/operator ./kubernetes/cmd/admission; \
	  go run ./scripts/notice --goos linux --goarch $$arch --output bin/notices/admission-$$arch.txt ./kubernetes/cmd/admission; \
	  go run ./scripts/notice --goos linux --goarch $$arch --output bin/notices/worker-$$arch.txt ./cmd/brewlet-node ./shim/cmd/containerd-shim-brewlet-v2; \
	done

maven-notices:
	mvn -B -q -f maven-plugin/pom.xml -DskipTests package

site-notices:
	PYTHONDONTWRITEBYTECODE=1 $(SITE_PYTHON) site/scripts/generate-notice.py

site-notices-check:
	PYTHONDONTWRITEBYTECODE=1 $(SITE_PYTHON) site/scripts/generate-notice.py --check

legal-check: go-notices
	python3 scripts/check_notices.py

release-check:
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/release.py identity
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s acceptance -p 'release*_test.py' -v

release-candidate:
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/release.py build
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/release_assurance.py sboms
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/release.py verify
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/release_assurance.py verify

ledger:
	python3 acceptance/check.py --structure-only

acceptance:
	python3 acceptance/verify_evidence.py

composition-acceptance:
	python3 acceptance/verify_evidence.py --composition-only

site-contract-check:
	PYTHONDONTWRITEBYTECODE=1 $(SITE_PYTHON) -m unittest discover -s site/scripts -p 'test_*.py' -v
	node --check site/assets/js/site.js

site-build: site-contract-check
	$(SITE_PYTHON) site/scripts/build.py
